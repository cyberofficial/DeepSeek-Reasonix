import { useCallback, useEffect, useRef, useState, type RefObject } from "react";
import type { VirtuosoHandle } from "react-virtuoso";
import type { TranscriptRow } from "./transcriptRows";
import { useTranscriptVirtuosoFirstItemIndex } from "./transcriptVirtuosoIndex";
import {
  captureTranscriptLayoutAnchor,
  transcriptAnchorInitialLocation,
  transcriptElementViewportIsBlank,
  type TranscriptLayoutAnchor,
} from "./transcriptVirtuosoRecovery";

const LAYOUT_INVALIDATION_BATCH_MS = 48;
const BLANK_RECOVERY_COOLDOWN_MS = 2_000;
// A viewport that blanks while the user is actively scrolling is almost always
// a transient mount lag (slow renderer outrunning the fling), not a broken
// size tree. Resets wait for the scroll to go quiet; only a blank that
// survives into idle earns a rebuild.
const USER_SCROLL_IDLE_MS = 320;
// Anchor restores wait for the anchor row to actually mount. An 8-frame
// budget (~128 ms) expired before heavy rows mounted on WebView2, stranding
// the view at the estimate-based (higher) scrollToIndex landing — the
// scroll-down/snap-up loop. Bound by wall clock instead.
const ANCHOR_RESTORE_BUDGET_MS = 1_000;
// Ref-resolution patch bursts (session open, stream end) each bump the layout
// revision; rebuilding the size tree per patch is a remount storm. Coalesce
// revision-driven rebuilds to at most one per interval.
const REVISION_RESET_MIN_INTERVAL_MS = 600;

/** User-scroll signals the Transcript wires into its intent handlers. */
export type TranscriptRecoveryControl = {
  noteUserScrollIntent: () => void;
  invalidateAnchors: () => void;
};

/** Rebuilds stale Virtuoso size trees while preserving the logical viewport. */
export function useTranscriptVirtuosoRecovery({
  surfaceKey,
  historyLayoutRevision,
  rows,
  rowIndexByKey,
  scrollRef,
  pinnedRef,
  virtuosoRef,
  readyRef,
  scrollToBottom,
  holdRevisionResets = false,
}: {
  surfaceKey: string;
  historyLayoutRevision: number;
  rows: readonly TranscriptRow[];
  rowIndexByKey: ReadonlyMap<string, number>;
  scrollRef: RefObject<HTMLDivElement | null>;
  pinnedRef: RefObject<boolean>;
  virtuosoRef: RefObject<VirtuosoHandle | null>;
  readyRef: RefObject<boolean>;
  scrollToBottom: () => void;
  // While true (the turn is streaming), revision-driven rebuilds are deferred:
  // a mid-stream remount snaps a tail-following view up to the last history
  // row because the live footer is not part of the restored frame, and it
  // blanks the region a history-reading user is looking at. One rebuild runs
  // when the stream ends.
  holdRevisionResets?: boolean;
}) {
  const [resetEpoch, setResetEpoch] = useState(0);
  const appliedRevisionRef = useRef(historyLayoutRevision);
  const latestRevisionRef = useRef(historyLayoutRevision);
  const resetTimerRef = useRef<number | null>(null);
  const blankCheckFrameRef = useRef<number | null>(null);
  const pendingAnchorRef = useRef<{ surfaceKey: string; anchor: TranscriptLayoutAnchor } | null>(null);
  const stableManualAnchorRef = useRef<Extract<TranscriptLayoutAnchor, { mode: "manual" }> | null>(null);
  const lastBlankRecoveryRef = useRef("");
  const lastBlankRecoveryAtRef = useRef(0);
  const userScrollActiveRef = useRef(false);
  const userScrollIdleTimerRef = useRef<number | null>(null);
  const deferredLayoutResetRef = useRef(false);
  const deferredResetTimerRef = useRef<number | null>(null);
  const lastRevisionResetAtRef = useRef(0);
  const holdRevisionResetsRef = useRef(holdRevisionResets);
  holdRevisionResetsRef.current = holdRevisionResets;
  latestRevisionRef.current = historyLayoutRevision;

  useEffect(() => {
    appliedRevisionRef.current = latestRevisionRef.current;
    pendingAnchorRef.current = null;
    stableManualAnchorRef.current = null;
    lastBlankRecoveryRef.current = "";
    lastBlankRecoveryAtRef.current = 0;
    userScrollActiveRef.current = false;
    deferredLayoutResetRef.current = false;
    lastRevisionResetAtRef.current = 0;
    if (resetTimerRef.current !== null) window.clearTimeout(resetTimerRef.current);
    if (blankCheckFrameRef.current !== null) cancelAnimationFrame(blankCheckFrameRef.current);
    if (userScrollIdleTimerRef.current !== null) window.clearTimeout(userScrollIdleTimerRef.current);
    if (deferredResetTimerRef.current !== null) window.clearTimeout(deferredResetTimerRef.current);
    resetTimerRef.current = null;
    blankCheckFrameRef.current = null;
    userScrollIdleTimerRef.current = null;
    deferredResetTimerRef.current = null;
  }, [surfaceKey]);

  useEffect(() => () => {
    if (resetTimerRef.current !== null) window.clearTimeout(resetTimerRef.current);
    if (blankCheckFrameRef.current !== null) cancelAnimationFrame(blankCheckFrameRef.current);
    if (userScrollIdleTimerRef.current !== null) window.clearTimeout(userScrollIdleTimerRef.current);
    if (deferredResetTimerRef.current !== null) window.clearTimeout(deferredResetTimerRef.current);
  }, []);

  const requestReset = useCallback((): boolean => {
    const element = scrollRef.current;
    if (!element || pendingAnchorRef.current?.surfaceKey === surfaceKey) return false;
    const anchor = stableManualAnchorRef.current
      ?? (pinnedRef.current ? { mode: "tail" } as const : captureTranscriptLayoutAnchor(element, false));
    if (!anchor) return false;
    pendingAnchorRef.current = { surfaceKey, anchor };
    readyRef.current = false;
    setResetEpoch((epoch) => epoch + 1);
    return true;
  }, [pinnedRef, readyRef, scrollRef, surfaceKey]);

  // Explicit user scroll intent outranks recovery: drop any pending/cached
  // anchor so an in-flight restore loop exits at its next frame check and a
  // later reset re-captures from the user's own position (#8657/#8688).
  const invalidateAnchors = useCallback(() => {
    pendingAnchorRef.current = null;
    stableManualAnchorRef.current = null;
  }, []);

  // Single attempt path for revision-driven rebuilds. Every gate either
  // settles the defer marker (already applied / reset issued) or leaves it
  // set for the next trigger — scroll idle, streaming-hold lift, or the
  // retry timer armed below.
  const flushDeferredLayoutReset = useCallback(function flushDeferred() {
    if (!deferredLayoutResetRef.current) return;
    if (appliedRevisionRef.current === latestRevisionRef.current) {
      deferredLayoutResetRef.current = false;
      return;
    }
    if (userScrollActiveRef.current || holdRevisionResetsRef.current) return;
    if (deferredResetTimerRef.current !== null) window.clearTimeout(deferredResetTimerRef.current);
    if (pendingAnchorRef.current?.surfaceKey === surfaceKey) {
      // A restore is still in flight; retry shortly instead of stacking a
      // second reset on top of it.
      deferredResetTimerRef.current = window.setTimeout(() => {
        deferredResetTimerRef.current = null;
        flushDeferred();
      }, LAYOUT_INVALIDATION_BATCH_MS);
      return;
    }
    const sinceLastReset = Date.now() - lastRevisionResetAtRef.current;
    if (sinceLastReset < REVISION_RESET_MIN_INTERVAL_MS) {
      // Patch bursts (session open, stream end) each bump the revision;
      // coalesce their rebuilds instead of remounting per patch.
      deferredResetTimerRef.current = window.setTimeout(() => {
        deferredResetTimerRef.current = null;
        flushDeferred();
      }, REVISION_RESET_MIN_INTERVAL_MS - sinceLastReset);
      return;
    }
    // Anchor captures pause once a revision is pending; refresh from the
    // user's resting position so the rebuild restores where they stopped.
    const element = scrollRef.current;
    const fresh = element ? captureTranscriptLayoutAnchor(element, pinnedRef.current) : undefined;
    if (fresh?.mode === "manual") stableManualAnchorRef.current = fresh;
    else if (fresh?.mode === "tail") stableManualAnchorRef.current = null;
    deferredLayoutResetRef.current = false;
    if (requestReset()) lastRevisionResetAtRef.current = Date.now();
    appliedRevisionRef.current = latestRevisionRef.current;
  }, [pinnedRef, requestReset, scrollRef, surfaceKey]);

  useEffect(() => {
    if (appliedRevisionRef.current === historyLayoutRevision) return;
    if (resetTimerRef.current !== null) window.clearTimeout(resetTimerRef.current);
    const flush = () => {
      resetTimerRef.current = null;
      deferredLayoutResetRef.current = true;
      flushDeferredLayoutReset();
    };
    resetTimerRef.current = window.setTimeout(flush, LAYOUT_INVALIDATION_BATCH_MS);
    return () => {
      if (resetTimerRef.current !== null) window.clearTimeout(resetTimerRef.current);
      resetTimerRef.current = null;
    };
  }, [historyLayoutRevision, flushDeferredLayoutReset, surfaceKey]);

  const resetKey = `${surfaceKey}:${resetEpoch}`;
  const firstItemIndex = useTranscriptVirtuosoFirstItemIndex(rows, resetKey);
  const pendingAnchor = pendingAnchorRef.current?.surfaceKey === surfaceKey ? pendingAnchorRef.current.anchor : undefined;
  const restoreLocation = transcriptAnchorInitialLocation(pendingAnchor, rowIndexByKey, firstItemIndex);

  const scheduleBlankViewportCheck = useCallback(() => {
    if (
      appliedRevisionRef.current === historyLayoutRevision
      && resetTimerRef.current === null
      && pendingAnchorRef.current?.surfaceKey !== surfaceKey
    ) {
      const element = scrollRef.current;
      const anchor = element ? captureTranscriptLayoutAnchor(element, pinnedRef.current) : undefined;
      if (anchor?.mode === "manual") stableManualAnchorRef.current = anchor;
      else if (anchor?.mode === "tail") stableManualAnchorRef.current = null;
    }
    if (
      blankCheckFrameRef.current !== null
      || resetTimerRef.current !== null
      || pendingAnchorRef.current?.surfaceKey === surfaceKey
    ) return;
    blankCheckFrameRef.current = requestAnimationFrame(() => {
      blankCheckFrameRef.current = requestAnimationFrame(() => {
        blankCheckFrameRef.current = null;
        if (userScrollActiveRef.current) return;
        const element = scrollRef.current;
        if (!element || !transcriptElementViewportIsBlank(element)) return;
        // Dedup on surface + content revision only. scrollTop drifts
        // continuously while streaming, so including it disabled the dedup
        // and allowed back-to-back full-list remounts (#8657/#8688).
        const recoveryKey = `${surfaceKey}:${historyLayoutRevision}`;
        const now = Date.now();
        if (lastBlankRecoveryRef.current === recoveryKey) return;
        if (now - lastBlankRecoveryAtRef.current < BLANK_RECOVERY_COOLDOWN_MS) return;
        lastBlankRecoveryRef.current = recoveryKey;
        lastBlankRecoveryAtRef.current = now;
        requestReset();
      });
    });
  }, [historyLayoutRevision, pinnedRef, requestReset, scrollRef, surfaceKey]);

  // Runs when user-driven scrolling has been quiet for USER_SCROLL_IDLE_MS:
  // flush a layout rebuild deferred mid-scroll, then re-check the viewport —
  // a blank that persists into idle is genuine breakage, not mount lag.
  const handleUserScrollIdle = useCallback(() => {
    userScrollActiveRef.current = false;
    flushDeferredLayoutReset();
    scheduleBlankViewportCheck();
  }, [flushDeferredLayoutReset, scheduleBlankViewportCheck]);

  const armUserScrollIdleTimer = useCallback(() => {
    if (userScrollIdleTimerRef.current !== null) window.clearTimeout(userScrollIdleTimerRef.current);
    userScrollIdleTimerRef.current = window.setTimeout(() => {
      userScrollIdleTimerRef.current = null;
      handleUserScrollIdle();
    }, USER_SCROLL_IDLE_MS);
  }, [handleUserScrollIdle]);

  // Wheel/touch/keyboard/pointer intent is explicit user scroll intent: it
  // aborts any in-flight recovery restore (user intent > recovery) and holds
  // layout rebuilds until the scroll goes quiet (#8657/#8688 follow-up).
  const noteUserScrollIntent = useCallback(() => {
    userScrollActiveRef.current = true;
    invalidateAnchors();
    armUserScrollIdleTimer();
  }, [armUserScrollIdleTimer, invalidateAnchors]);

  // Scroll events after an intent keep the active window alive; programmatic
  // scrolls (tail follow, restores) never arm it on their own.
  const noteScrollActivity = useCallback(() => {
    if (userScrollActiveRef.current) armUserScrollIdleTimer();
  }, [armUserScrollIdleTimer]);

  // Stream end lifts the revision hold: run the rebuild deferred mid-stream,
  // unless the user is mid-scroll (the idle handler owns that flush).
  useEffect(() => {
    if (holdRevisionResets) return;
    if (userScrollActiveRef.current) return;
    flushDeferredLayoutReset();
  }, [holdRevisionResets, flushDeferredLayoutReset]);

  const restoreAnchor = useCallback((anchor: TranscriptLayoutAnchor) => {
    if (pendingAnchorRef.current?.surfaceKey !== surfaceKey) return;
    if (anchor.mode === "tail") {
      pendingAnchorRef.current = null;
      scrollToBottom();
      return;
    }
    const deadline = Date.now() + ANCHOR_RESTORE_BUDGET_MS;
    const restore = (stableFrames: number) => {
      if (pendingAnchorRef.current?.surfaceKey !== surfaceKey) return;
      const element = scrollRef.current;
      if (!element) {
        pendingAnchorRef.current = null;
        return;
      }
      const row = Array.from(element.querySelectorAll<HTMLElement>(".transcript__row[data-row-key]"))
        .find((candidate) => candidate.dataset.rowKey === anchor.rowKey);
      if (!row) {
        // Heavy rows can take far longer than a few frames to mount after a
        // rebuild on slow renderers. Keep re-aiming until the wall-clock
        // budget expires; a user gesture drops the anchor instead (#8657/#8688).
        if (Date.now() >= deadline) {
          pendingAnchorRef.current = null;
          return;
        }
        const location = transcriptAnchorInitialLocation(anchor, rowIndexByKey, firstItemIndex);
        if (location) virtuosoRef.current?.scrollToIndex(location);
        requestAnimationFrame(() => restore(0));
        return;
      }
      const viewportTop = element.getBoundingClientRect().top;
      const correction = row.getBoundingClientRect().top - viewportTop - anchor.offset;
      if (Math.abs(correction) > 1) virtuosoRef.current?.scrollBy({ top: correction, behavior: "auto" });
      const nextStableFrames = Math.abs(correction) <= 1 ? stableFrames + 1 : 0;
      if (Date.now() < deadline && nextStableFrames < 2) {
        requestAnimationFrame(() => restore(nextStableFrames));
        return;
      }
      pendingAnchorRef.current = null;
      stableManualAnchorRef.current = anchor;
    };
    requestAnimationFrame(() => restore(0));
  }, [firstItemIndex, rowIndexByKey, scrollRef, scrollToBottom, virtuosoRef]);

  const handleItemsRendered = useCallback((renderedCount: number) => {
    if (!readyRef.current && renderedCount > 0) {
      readyRef.current = true;
      const pending = pendingAnchorRef.current;
      if (pending?.surfaceKey === surfaceKey) restoreAnchor(pending.anchor);
      else requestAnimationFrame(scrollToBottom);
    }
    scheduleBlankViewportCheck();
  }, [readyRef, restoreAnchor, scheduleBlankViewportCheck, scrollToBottom, surfaceKey]);

  return {
    resetKey,
    firstItemIndex,
    restoreLocation,
    handleItemsRendered,
    scheduleBlankViewportCheck,
    invalidateAnchors,
    noteUserScrollIntent,
    noteScrollActivity,
  };
}
