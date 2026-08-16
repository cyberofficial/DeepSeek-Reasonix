// Run: tsx src/__tests__/transcript-recovery-race.test.tsx

import { JSDOM } from "jsdom";
import React, { act } from "react";
import { createRoot } from "react-dom/client";
import type { VirtuosoHandle } from "react-virtuoso";
import { useTranscriptVirtuosoRecovery } from "../lib/useTranscriptVirtuosoRecovery";
import type { TranscriptRow } from "../lib/transcriptRows";
import type { Item } from "../lib/useController";

let passed = 0;
let failed = 0;

function check(condition: unknown, label: string) {
  if (condition) {
    process.stdout.write(`  PASS  ${label}\n`);
    passed += 1;
  } else {
    process.stdout.write(`  FAIL  ${label}\n`);
    failed += 1;
  }
}

console.log("\ntranscript recovery races");

const dom = new JSDOM('<!doctype html><html><body><div id="root"></div><div id="scroll"><div class="transcript__row" data-row-key="row-a"></div></div></body></html>', {
  pretendToBeVisual: true,
  url: "http://localhost/",
});
(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
globalThis.window = dom.window as unknown as Window & typeof globalThis;
globalThis.document = dom.window.document;
globalThis.HTMLElement = dom.window.HTMLElement;
globalThis.Element = dom.window.Element;
globalThis.Node = dom.window.Node;

let nextFrame = 1;
const frames = new Map<number, FrameRequestCallback>();
const requestFrame = (callback: FrameRequestCallback) => {
  const id = nextFrame;
  nextFrame += 1;
  frames.set(id, callback);
  return id;
};
const cancelFrame = (id: number) => void frames.delete(id);
globalThis.requestAnimationFrame = requestFrame;
globalThis.cancelAnimationFrame = cancelFrame;
dom.window.requestAnimationFrame = requestFrame;
dom.window.cancelAnimationFrame = cancelFrame;

let clockNow = 10_000;
let nextTimer = 1;
const timers = new Map<number, { dueAt: number; run: () => void }>();
const originalDateNow = Date.now;
const originalSetTimeout = dom.window.setTimeout;
const originalClearTimeout = dom.window.clearTimeout;
Date.now = () => clockNow;
dom.window.setTimeout = ((handler: TimerHandler, timeout = 0, ...args: unknown[]) => {
  const id = nextTimer;
  nextTimer += 1;
  const run = typeof handler === "function"
    ? () => handler(...args)
    : () => { throw new Error("string timer handlers are unsupported in this test"); };
  timers.set(id, { dueAt: clockNow + Math.max(0, timeout), run });
  return id;
}) as typeof dom.window.setTimeout;
dom.window.clearTimeout = ((id: number | undefined) => {
  if (id !== undefined) timers.delete(id);
}) as typeof dom.window.clearTimeout;

async function advanceClock(milliseconds: number) {
  await act(async () => {
    const target = clockNow + milliseconds;
    while (true) {
      const next = [...timers.entries()]
        .filter(([, timer]) => timer.dueAt <= target)
        .sort(([leftID, left], [rightID, right]) => left.dueAt - right.dueAt || leftID - rightID)[0];
      if (!next) break;
      const [id, timer] = next;
      timers.delete(id);
      clockNow = timer.dueAt;
      timer.run();
    }
    clockNow = target;
  });
}

async function flushFrames() {
  const pending = [...frames.entries()];
  frames.clear();
  await act(async () => pending.forEach(([, callback]) => callback(performance.now())));
}

const scrollElement = dom.window.document.getElementById("scroll") as HTMLDivElement;
const rowElement = scrollElement.querySelector<HTMLElement>(".transcript__row")!;
scrollElement.getBoundingClientRect = () => ({ top: 0, bottom: 100, height: 100, left: 0, right: 800, width: 800, x: 0, y: 0, toJSON: () => ({}) });
rowElement.getBoundingClientRect = () => ({ top: 200, bottom: 300, height: 100, left: 0, right: 800, width: 800, x: 0, y: 200, toJSON: () => ({}) });
Object.defineProperty(scrollElement, "clientHeight", { configurable: true, value: 100 });
Object.defineProperty(scrollElement, "scrollHeight", { configurable: true, value: 500 });

const item: Item = { kind: "assistant", id: "a", text: "answer", reasoning: "", streaming: false };
const rows: TranscriptRow[] = [{ kind: "answer", key: "row-a", item }];
const scrollRef = { current: scrollElement };
const pinnedRef = { current: false };
const readyRef = { current: true };
let scrollByCalls = 0;
let scrollToIndexCalls = 0;
let scrollToBottomCalls = 0;
const virtuosoRef = {
  current: {
    scrollBy: () => { scrollByCalls += 1; },
    scrollToIndex: () => { scrollToIndexCalls += 1; },
  } as unknown as VirtuosoHandle,
};
let recovery: ReturnType<typeof useTranscriptVirtuosoRecovery> | undefined;

function Probe({ surfaceKey, revision = 0, hold = false }: { surfaceKey: string; revision?: number; hold?: boolean }) {
  recovery = useTranscriptVirtuosoRecovery({
    surfaceKey,
    historyLayoutRevision: revision,
    rows,
    rowIndexByKey: new Map([["row-a", 0]]),
    scrollRef,
    pinnedRef,
    virtuosoRef,
    readyRef,
    scrollToBottom: () => { scrollToBottomCalls += 1; },
    holdRevisionResets: hold,
  });
  return null;
}

const root = createRoot(dom.window.document.getElementById("root")!);
await act(async () => root.render(<Probe surfaceKey="surface-a" />));
await act(async () => recovery?.scheduleBlankViewportCheck());
await act(async () => root.render(<Probe surfaceKey="surface-b" />));
await flushFrames();
check(recovery?.resetKey === "surface-b:0", "surface switch cancels the previous blank-viewport watchdog");

await act(async () => recovery?.scheduleBlankViewportCheck());
await flushFrames();
await flushFrames();
check(recovery?.resetKey === "surface-b:1", "blank viewport schedules a controlled size-tree rebuild");
await act(async () => recovery?.handleItemsRendered(1));
await act(async () => root.render(<Probe surfaceKey="surface-c" />));
await flushFrames();
check(scrollByCalls === 0, "stale anchor correction cannot scroll the newly selected surface");

// ── invalidateAnchors: user intent cancels an in-flight restore (#8657/#8688)
await act(async () => recovery?.scheduleBlankViewportCheck());
await flushFrames();
await flushFrames();
check(recovery?.resetKey === "surface-c:2", "blank viewport rebuilds the size tree on the current surface");
scrollByCalls = 0;
scrollToIndexCalls = 0;
scrollToBottomCalls = 0;
await act(async () => recovery?.invalidateAnchors());
await act(async () => recovery?.handleItemsRendered(1));
await flushFrames();
check(scrollByCalls === 0, "invalidated anchor stops the restore correction loop");
check(scrollToIndexCalls === 0, "invalidated anchor never re-aims at the stale row");
check(scrollToBottomCalls === 1, "a reset without an anchor settles at the bottom");

// ── Blank-recovery cooldown: revision bump rebuilds, immediate re-blank is blocked
await act(async () => root.render(<Probe surfaceKey="surface-c" revision={1} />));
await advanceClock(60);
await flushFrames();
check(recovery?.resetKey === "surface-c:3", "layout revision rebuilds the size tree after the batch window");
await act(async () => recovery?.handleItemsRendered(1));
// Let the in-flight restore converge: place the anchor row at its target
// offset so the correction loop settles within two stable frames (real DOMs
// converge after each scrollBy; the stubbed rects here do not move unless we
// move them, and the wall-clock budget would otherwise keep it alive).
rowElement.getBoundingClientRect = () => ({ top: 0, bottom: 100, height: 100, left: 0, right: 800, width: 800, x: 0, y: 0, toJSON: () => ({}) });
for (let i = 0; i < 10; i += 1) await flushFrames();
rowElement.getBoundingClientRect = () => ({ top: 200, bottom: 300, height: 100, left: 0, right: 800, width: 800, x: 0, y: 200, toJSON: () => ({}) });
scrollByCalls = 0;
await act(async () => recovery?.scheduleBlankViewportCheck());
await flushFrames();
await flushFrames();
check(recovery?.resetKey === "surface-c:3", "blank recovery within the cooldown window is ignored");
check(scrollByCalls === 0, "cooldown-blocked blank check performs no correction");

// ── User-scroll quiescence: a layout revision must not rebuild mid-scroll
await act(async () => root.render(<Probe surfaceKey="surface-d" />));
await flushFrames();
const keyBeforeIntent = recovery?.resetKey;
await act(async () => recovery?.noteUserScrollIntent());
await act(async () => root.render(<Probe surfaceKey="surface-d" revision={1} />));
await advanceClock(60);
await flushFrames();
check(recovery?.resetKey === keyBeforeIntent, "layout revision does not rebuild the size tree mid-scroll");
await advanceClock(350);
await flushFrames();
check(recovery?.resetKey !== keyBeforeIntent && recovery?.resetKey.startsWith("surface-d:"), "deferred layout rebuild fires once the scroll goes quiet");

// ── A user scroll gesture aborts the restore that rebuild just started
scrollByCalls = 0;
scrollToIndexCalls = 0;
scrollToBottomCalls = 0;
await act(async () => recovery?.handleItemsRendered(1));
await flushFrames();
check(scrollByCalls > 0 || scrollToIndexCalls > 0, "anchor restore is in flight after the deferred rebuild");
await act(async () => recovery?.noteUserScrollIntent());
const frozenScrollBy = scrollByCalls;
const frozenScrollToIndex = scrollToIndexCalls;
await flushFrames();
await flushFrames();
await flushFrames();
check(scrollByCalls === frozenScrollBy && scrollToIndexCalls === frozenScrollToIndex, "a user scroll gesture aborts the in-flight restore");
await advanceClock(350);

// ── Blank detection is gated while the user scrolls, armed again at idle
await act(async () => root.render(<Probe surfaceKey="surface-e" />));
await flushFrames();
const keySurfaceE = recovery?.resetKey;
await act(async () => recovery?.noteUserScrollIntent());
await act(async () => recovery?.scheduleBlankViewportCheck());
await flushFrames();
await flushFrames();
check(recovery?.resetKey === keySurfaceE, "blank viewport during active user scrolling does not rebuild");
await advanceClock(350);
await flushFrames();
await flushFrames();
check(recovery?.resetKey !== keySurfaceE && recovery?.resetKey.startsWith("surface-e:"), "a blank that persists into scroll idle earns a rebuild");

// ── Restore waits for a slow-mounting anchor row beyond the old 8-frame budget
await act(async () => root.render(<Probe surfaceKey="surface-f" />));
await flushFrames();
const keySurfaceF = recovery?.resetKey;
await act(async () => recovery?.scheduleBlankViewportCheck());
await flushFrames();
await flushFrames();
check(recovery?.resetKey !== keySurfaceF, "rebuild armed for the slow-mount restore");
rowElement.remove();
scrollByCalls = 0;
scrollToIndexCalls = 0;
await act(async () => recovery?.handleItemsRendered(1));
for (let i = 0; i < 10; i += 1) await flushFrames();
check(scrollToIndexCalls > 8, "restore keeps re-aiming past the old 8-frame budget while the anchor row is unmounted");
scrollElement.appendChild(rowElement);
rowElement.getBoundingClientRect = () => ({ top: 50, bottom: 150, height: 100, left: 0, right: 800, width: 800, x: 0, y: 50, toJSON: () => ({}) });
await flushFrames();
check(scrollByCalls > 0, "restore corrects once the anchor row mounts");
rowElement.getBoundingClientRect = () => ({ top: 0, bottom: 100, height: 100, left: 0, right: 800, width: 800, x: 0, y: 0, toJSON: () => ({}) });
await flushFrames();
await flushFrames();
await flushFrames();
const settledScrollBy = scrollByCalls;
const settledScrollToIndex = scrollToIndexCalls;
await flushFrames();
check(scrollByCalls === settledScrollBy && scrollToIndexCalls === settledScrollToIndex, "restore settles on the mounted anchor within the wall-clock budget");

// ── Streaming hold: revisions defer until the stream ends
await act(async () => root.render(<Probe surfaceKey="surface-g" hold={true} />));
await flushFrames();
const keySurfaceG = recovery?.resetKey;
await act(async () => root.render(<Probe surfaceKey="surface-g" revision={1} hold={true} />));
await advanceClock(60);
await flushFrames();
check(recovery?.resetKey === keySurfaceG, "layout revision does not rebuild while the turn is streaming");
await act(async () => root.render(<Probe surfaceKey="surface-g" revision={1} hold={false} />));
await flushFrames();
check(recovery?.resetKey !== keySurfaceG && recovery?.resetKey.startsWith("surface-g:"), "the deferred rebuild runs when the stream ends");

// ── Revision rebuilds coalesce within the min interval
await act(async () => root.render(<Probe surfaceKey="surface-h" />));
await flushFrames();
const keySurfaceH0 = recovery?.resetKey;
await act(async () => root.render(<Probe surfaceKey="surface-h" revision={1} />));
await advanceClock(60);
await flushFrames();
check(recovery?.resetKey !== keySurfaceH0 && recovery?.resetKey.startsWith("surface-h:"), "first layout revision rebuilds after the batch window");
await act(async () => recovery?.invalidateAnchors()); // simulate the restore completing
const keySurfaceH1 = recovery?.resetKey;
await act(async () => root.render(<Probe surfaceKey="surface-h" revision={2} />));
await advanceClock(60);
await flushFrames();
check(recovery?.resetKey === keySurfaceH1, "a second revision inside the coalescing window does not rebuild again");
await advanceClock(620);
await flushFrames();
check(recovery?.resetKey !== keySurfaceH1, "the coalesced rebuild fires when the interval lapses");

await act(async () => root.unmount());
Date.now = originalDateNow;
dom.window.setTimeout = originalSetTimeout;
dom.window.clearTimeout = originalClearTimeout;
dom.window.close();

if (failed > 0) {
  console.error(`\n${failed} transcript recovery race test(s) failed; ${passed} passed.`);
  process.exit(1);
}
console.log(`\n${passed} transcript recovery race tests passed.`);
