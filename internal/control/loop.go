package control

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/scheduler"
	"reasonix/internal/secrets"
)

// loopMaintenancePrompt is the built-in prompt for a bare /loop (no interval,
// no prompt). It continues work already in flight without starting new
// initiatives — the loop's job is tending, not exploring.
const loopMaintenancePrompt = `Continue the current session's ongoing work:

1. Continue any unfinished work from the conversation, in order.
2. Tend to the current branch's pull request: new review comments, failed CI runs, merge conflicts.
3. Run cleanup passes (bug hunts, simplification) only when nothing else is pending.

Do not start new initiatives outside that scope. Irreversible actions (pushing, deleting) proceed only when they continue something the transcript already authorized. When everything is handled, say so in one line.`

// maxLoopMDBytes caps loop.md content; longer files are truncated (matching
// the 25,000-byte budget the reference implementation uses).
const maxLoopMDBytes = 25_000

// StartLoop creates a scheduled task from a /loop command's arguments and
// returns a human-readable confirmation. Interval and prompt are both
// optional:
//
//	"/loop 5m check the deploy"      — fixed cron schedule
//	"/loop check the deploy"         — dynamic: agent picks each delay via schedule_wakeup
//	"/loop --forever 5m check deploy" — endless: exempt from the 7-day expiry
//	"/loop 5m" / "/loop"             — loop.md (or the built-in maintenance prompt)
//
// A fixed interval is a leading token parseable by scheduler.ParseInterval;
// anything else is the prompt. A prompt-only loop is dynamic. No prompt at
// all falls back to loop.md then the maintenance prompt; with an interval it
// runs on the fixed schedule, without one it is dynamic. A leading
// "--forever" marks the task NoExpire so it survives session resume past the
// 7-day prune (endless cron jobs).
func (c *Controller) StartLoop(input string) (string, error) {
	sched := c.scheduler
	if sched == nil {
		return "", fmt.Errorf("scheduler is unavailable in this session")
	}
	interval, prompt, noExpire := parseLoopArgs(input)
	if strings.TrimSpace(prompt) == "" {
		prompt = c.loadLoopMD()
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = loopMaintenancePrompt
	}
	var cronExpr string
	if interval != "" {
		cronExpr, _ = scheduler.ParseInterval(interval)
	}
	id, err := sched.Add(cronExpr, strings.TrimSpace(prompt), time.Now(), false, noExpire, false)
	if err != nil {
		return "", err
	}
	note := ""
	if noExpire {
		note = " (no expiry)"
	}
	if cronExpr != "" {
		return fmt.Sprintf("loop started — task %s: every %s%s\n%s", id, interval, note, promptPreview(prompt)), nil
	}
	return fmt.Sprintf("loop started — task %s: dynamic schedule, first fire now%s\n%s", id, note, promptPreview(prompt)), nil
}

// parseLoopArgs splits /loop arguments into an optional leading --forever
// flag, an optional interval token, and the remaining prompt.
func parseLoopArgs(input string) (interval, prompt string, noExpire bool) {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return "", "", false
	}
	rest := input
	if fields[0] == "--forever" {
		noExpire = true
		rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
		fields = strings.Fields(rest)
		if len(fields) == 0 {
			return "", "", true
		}
	}
	if _, ok := scheduler.ParseInterval(fields[0]); ok {
		return fields[0], strings.TrimSpace(strings.TrimPrefix(rest, fields[0])), noExpire
	}
	return "", rest, noExpire
}

// StartLoopInstant creates a scheduled task that fires IMMEDIATELY on creation,
// then continues on the specified interval schedule (or dynamic if no interval).
//
//	"/loopinstant 2m check the deploy"  — fires now, then every 2 minutes
//	"/loopinstant check the deploy"     — fires now, then dynamic (agent-controlled)
//	"/loopinstant --forever 2m check"   — fires now, endless: no 7-day expiry
func (c *Controller) StartLoopInstant(input string) (string, error) {
	sched := c.scheduler
	if sched == nil {
		return "", fmt.Errorf("scheduler is unavailable in this session")
	}
	interval, prompt, noExpire := parseLoopInstantArgs(input)
	if strings.TrimSpace(prompt) == "" {
		prompt = c.loadLoopMD()
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = loopMaintenancePrompt
	}
	var cronExpr string
	if interval != "" {
		cronExpr, _ = scheduler.ParseInterval(interval)
	}
	// Pass fireImmediately=true to make the first fire happen immediately
	id, err := sched.Add(cronExpr, strings.TrimSpace(prompt), time.Now(), false, noExpire, true)
	if err != nil {
		return "", err
	}
	note := ""
	if noExpire {
		note = " (no expiry)"
	}
	if cronExpr != "" {
		return fmt.Sprintf("loopinstant started — task %s: every %s (fires now)%s\n%s", id, interval, note, promptPreview(prompt)), nil
	}
	return fmt.Sprintf("loopinstant started — task %s: dynamic (fires now)%s\n%s", id, note, promptPreview(prompt)), nil
}

// parseLoopInstantArgs splits /loopinstant arguments into an optional leading --forever
// flag, an optional interval token, and the remaining prompt.
// The --instant behavior is implied by the command itself.
func parseLoopInstantArgs(input string) (interval, prompt string, noExpire bool) {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return "", "", false
	}
	rest := input
	if fields[0] == "--forever" {
		noExpire = true
		rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
		fields = strings.Fields(rest)
		if len(fields) == 0 {
			return "", "", true
		}
	}
	if _, ok := scheduler.ParseInterval(fields[0]); ok {
		return fields[0], strings.TrimSpace(strings.TrimPrefix(rest, fields[0])), noExpire
	}
	return "", rest, noExpire
}

// StartLoopAction creates a command-based scheduled task from a /loopaction
// command's arguments: on each fire the OS runs the command and the LLM is
// only woken when the output needs a decision (see runCommandAction).
func (c *Controller) StartLoopAction(input string) (string, error) {
	sched := c.scheduler
	if sched == nil {
		return "", fmt.Errorf("scheduler is unavailable in this session")
	}
	interval, command, matchPattern, actionResponse, noExpire, err := parseLoopActionArgs(input)
	if err != nil {
		return "", fmt.Errorf("loopaction: %v", err)
	}
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("loopaction: a command is required")
	}
	if interval == "" {
		interval = "1m"
	}
	cronExpr, ok := scheduler.ParseInterval(interval)
	if !ok {
		// Unreachable today (parseLoopActionArgs only accepts parseable
		// interval tokens), but encode the invariant so a future parser
		// change cannot create a dormant task with an empty cronExpr.
		return "", fmt.Errorf("loopaction: %q is not a valid interval token (s/m/h/d)", interval)
	}
	id, err := sched.AddAction(cronExpr, strings.TrimSpace(command), matchPattern,
		false, noExpire, actionResponse, time.Time{})
	if err != nil {
		return "", err
	}
	resp := "off"
	if actionResponse {
		resp = "on"
	}
	note := fmt.Sprintf("loopaction started — task %s: %s, ai-response: %s", id, interval, resp)
	if matchPattern != "" {
		note += fmt.Sprintf(", match: %q", matchPattern)
	}
	if noExpire {
		note += " (no expiry)"
	}
	note += "\ncommand: " + promptPreview(strings.TrimSpace(command))
	return note, nil
}

// StartLoopDelay creates a one-shot countdown trigger: the prompt (or, with
// --action, the host command) fires once after the delay and the task deletes
// itself. The LLM can keep it going afterwards via schedule_wakeup, turning
// the one-shot into a loop — or let it end.
func (c *Controller) StartLoopDelay(input string) (string, error) {
	sched := c.scheduler
	if sched == nil {
		return "", fmt.Errorf("scheduler is unavailable in this session")
	}
	delay, action, payload, matchPattern, actionResponse, err := parseLoopDelayArgs(input)
	if err != nil {
		return "", err
	}
	at := time.Now().Add(delay)
	var id string
	if action {
		id, err = sched.AddActionAt("", payload, matchPattern,
			true, false, actionResponse, time.Time{}, at)
	} else {
		id, err = sched.Add("", payload, at, true, false, false)
	}
	if err != nil {
		return "", err
	}
	kind := "trigger"
	if action {
		kind = "action trigger"
	}
	note := fmt.Sprintf("%s set — task %s fires in %s (%s)", kind, id, formatDuration(delay), at.Format("15:04:05"))
	if action && matchPattern != "" {
		note += fmt.Sprintf(", wake LLM on match: %q", matchPattern)
	}
	return note, nil
}

// parseLoopDelayArgs splits /loopdelay into an optional --action flag, a
// duration, and the payload (prompt, or command in action mode). The duration
// is a leading Go token (2m, 90s, 1h30m) or a natural phrase ("in 2 minutes").
// --match <re> and --no-ai apply only in action mode.
func parseLoopDelayArgs(input string) (
	delay time.Duration, action bool, payload, matchPattern string,
	actionResponse bool, err error,
) {
	rest := strings.TrimSpace(input)
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "in"))
	// Flags may lead the duration: "--action 90s ..." and "90s --action ..."
	// both parse.
	actionResponse = true
	fields := strings.Fields(rest)
	for len(fields) > 0 && (fields[0] == "--action" || fields[0] == "--no-ai") {
		if fields[0] == "--action" {
			action = true
		} else {
			actionResponse = false
		}
		rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
		fields = strings.Fields(rest)
	}
	if len(fields) == 0 {
		return 0, false, "", "", true, fmt.Errorf("usage: /loopdelay [--action] <duration> <prompt|command>")
	}
	delay, consumed, ok := parseNaturalDuration(fields)
	if !ok || delay <= 0 {
		return 0, false, "", "", true, fmt.Errorf("loopdelay: expected a duration like 2m, 90s, or 'in 2 minutes'")
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, consumed))
	fields = strings.Fields(rest)
	for len(fields) > 0 {
		switch fields[0] {
		case "--action":
			action = true
		case "--no-ai":
			actionResponse = false
		case "--match":
			if len(fields) < 2 {
				return 0, false, "", "", true, fmt.Errorf("loopdelay: --match needs a regex")
			}
			matchPattern = fields[1]
			rest = strings.TrimSpace(strings.TrimPrefix(rest, "--match "+fields[1]))
			fields = strings.Fields(rest)
			continue
		default:
			payload = strings.TrimSpace(rest)
			if payload == "" {
				return 0, false, "", "", true, fmt.Errorf("loopdelay: a prompt or command is required")
			}
			return delay, action, payload, matchPattern, actionResponse, nil
		}
		rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
		fields = strings.Fields(rest)
	}
	return 0, false, "", "", true, fmt.Errorf("loopdelay: a prompt or command is required")
}

// parseNaturalDuration reads a duration from the leading fields: a Go token
// (2m, 90s) or a natural phrase (2 minutes, 1 hour). It returns the duration
// and the exact substring consumed, for the caller to strip.
func parseNaturalDuration(fields []string) (time.Duration, string, bool) {
	if len(fields) == 0 {
		return 0, "", false
	}
	if d, err := time.ParseDuration(fields[0]); err == nil && d > 0 {
		return d, fields[0], true
	}
	if len(fields) < 2 {
		return 0, "", false
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n <= 0 {
		return 0, "", false
	}
	unit := strings.ToLower(strings.TrimSuffix(fields[1], "s"))
	var mult time.Duration
	switch unit {
	case "minute":
		mult = time.Minute
	case "hour":
		mult = time.Hour
	case "second":
		mult = time.Second
	default:
		return 0, "", false
	}
	return time.Duration(n) * mult, fields[0] + " " + fields[1], true
}

// formatDuration renders a delay for notices, e.g. "2m0s" as "2m".
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		return strings.TrimSuffix(s, "0s")
	}
	return s
}

// parseLoopActionArgs splits /loopaction into flags, an optional interval, and
// the command. Flags may appear in any order before the command; the command is
// everything after them, kept verbatim so quoting survives. --no-ai sets
// actionResponse=false and nothing re-enables it.
func parseLoopActionArgs(input string) (
	interval, command, matchPattern string, actionResponse, noExpire bool, err error,
) {
	actionResponse = true // default: wake the LLM when there is output to act on
	rest := strings.TrimSpace(input)
	for {
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return
		}
		switch fields[0] {
		case "--forever":
			noExpire = true
			rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
		case "--no-ai":
			actionResponse = false
			rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
		case "--match":
			if len(fields) < 2 {
				err = fmt.Errorf("--match requires a pattern")
				return
			}
			matchPattern = fields[1]
			rest = rest[len(fields[0]):]         // drop "--match"
			rest = strings.TrimLeft(rest, " \t") // any whitespace before the pattern
			rest = rest[len(fields[1]):]         // drop the pattern
			rest = strings.TrimSpace(rest)
		default:
			if interval == "" {
				if _, ok := scheduler.ParseInterval(fields[0]); ok {
					interval = fields[0]
					rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
					continue
				}
			}
			command = rest // first non-flag token: everything remaining is the command
			return
		}
	}
}

// promptPreview shortens a loop prompt for confirmation/notice text.
func promptPreview(prompt string) string {
	p := strings.Join(strings.Fields(prompt), " ")
	if len(p) > 120 {
		return p[:117] + "..."
	}
	return p
}

// loadLoopMD returns the default loop prompt from loop.md: the project file
// (<root>/.reasonix/loop.md) wins over the user file (<home>/loop.md). It is
// re-read on every call so edits take effect on the next iteration; an empty
// result means no loop.md exists (callers fall back to the built-in prompt).
func (c *Controller) loadLoopMD() string {
	candidates := []string{}
	if root := c.workspaceRoot; strings.TrimSpace(root) != "" {
		candidates = append(candidates, filepath.Join(root, ".reasonix", "loop.md"))
	}
	candidates = append(candidates, filepath.Join(config.ReasonixHomeDir(), "loop.md"))
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if len(data) > maxLoopMDBytes {
			data = data[:maxLoopMDBytes]
		}
		if text := strings.TrimSpace(string(data)); text != "" {
			return text
		}
	}
	return ""
}

// runScheduledTurn fires a scheduled task, either by steering the prompt into
// the active turn's message queue (mid-turn fire — the agent picks it up at
// its next natural step) or, when no active turn can accept it, as a turn
// between foreground turns. It is invoked from the scheduler's ticker
// goroutine. MarkStarted runs on the accepted delivery path (injectScheduledTask)
// or inside the admitted body so a parked fire keeps its firing flag (no
// duplicate queued turns) until the turn genuinely begins.
func (c *Controller) runScheduledTurn(task scheduler.Task) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		// Contract: a delivered-but-unstarted fire must never leave the
		// task's firing flag set, even when the controller is already torn
		// down — release it so any survivor path (or a rebind that skips
		// Load) cannot wedge the task.
		c.scheduler.ReleaseFiring(task.ID)
		return
	}
	if c.injectScheduledTask(task) {
		// The fire was delivered into the running turn's message queue:
		// MarkStarted consumed the firing flag and re-armed the cron
		// schedule from now, so cycles that passed while the turn ran are
		// skipped instead of catching up.
		return
	}
	result := c.runGuardedOrPark(func(ctx context.Context) error {
		// Notice inside the admitted body: a fire that parks (or is dropped
		// during rotation) does not spam "fired" notices — the user only sees
		// one when the turn actually begins.
		c.scheduler.MarkStarted(task.ID)
		c.notice(fmt.Sprintf("⏰ scheduled task %s running", task.ID))
		return c.runGoalLoopWithRaw(ctx, task.Prompt, task.Prompt)
	})
	if result != turnStarted && result != turnParked {
		// Admission dropped the turn (controller rotating or closed) so the
		// body will never call MarkStarted. Release the firing flag: a cron
		// task re-fires on the next tick, a dynamic task stays paused instead
		// of silently dying with its wakeup consumed.
		c.scheduler.ReleaseFiring(task.ID)
	}
}

// runCommandAction executes a loopaction task and decides whether to wake the
// LLM: --no-ai never wakes; an "ai-response: false" marker stays silent; an
// "ai-response: true" marker forces the wake (overriding the regex); otherwise
// the output wakes the LLM iff it matches MatchPattern, or always when no
// pattern is set. Silent fires just re-arm via MarkStarted.
func (c *Controller) runCommandAction(task scheduler.Task) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		// A fire delivered while the controller is closed never ran its
		// command; consume the cycle and re-arm to the next slot so the
		// ticker cannot re-deliver it every second until the rebind.
		c.scheduler.MarkStarted(task.ID)
		return
	}
	output := c.executeLoopActionCommand(task)
	if output == "" {
		c.scheduler.MarkStarted(task.ID) // no output: silent fire
		return
	}
	finalNote := ""
	if isFinalFire(task, time.Now()) {
		finalNote = " (expired — no further runs)"
	}
	if !task.ActionResponse {
		// --no-ai: never wake the LLM, but the output is still the user's
		// signal — surface it in chat instead of dropping it.
		c.scheduler.MarkStarted(task.ID)
		c.notice(fmt.Sprintf("⏰ loopaction task %s output: %s%s", task.ID, promptPreview(output), finalNote))
		return
	}
	if strings.Contains(output, "ai-response: false") {
		c.scheduler.MarkStarted(task.ID) // script opted out of a wake
		return
	}
	matched := true
	if task.MatchPattern != "" {
		var err error
		matched, err = regexp.MatchString(task.MatchPattern, output)
		if err != nil {
			c.notice(fmt.Sprintf("loopaction task %s: bad regex %q — %v",
				task.ID, task.MatchPattern, err))
			c.scheduler.MarkStarted(task.ID)
			return
		}
	}
	if !matched && !strings.Contains(output, "ai-response: true") {
		c.scheduler.MarkStarted(task.ID) // no match, no marker: silent fire
		return
	}
	steerText := fmt.Sprintf("loopaction task %s output:\n%s", task.ID, output)
	if !c.TrySteer(agent.MidTurnScheduledOutput(task.ID, steerText)) {
		// Idle session: no running turn can consume the steer, so run the
		// output as a full parked turn instead of dropping the fire.
		result := c.runGuardedOrPark(func(ctx context.Context) error {
			c.scheduler.MarkStarted(task.ID)
			c.notice(fmt.Sprintf("⏰ loopaction task %s running — output delivered", task.ID))
			return c.runGoalLoopWithRaw(ctx, steerText, steerText)
		})
		if result != turnStarted && result != turnParked {
			c.scheduler.ReleaseFiring(task.ID)
		}
		return
	}
	c.scheduler.MarkStarted(task.ID)
	c.notice(fmt.Sprintf("⏰ loopaction task %s triggered — output injected%s", task.ID, finalNote))
}

// isFinalFire reports whether this fire is the task's last: the next cron
// slot lands at or after the expiry deadline, so the task self-deletes
// before it can fire again.
func isFinalFire(t scheduler.Task, now time.Time) bool {
	if t.ExpiresAt.IsZero() || t.CronExpr == "" {
		return false
	}
	return !t.ExpiresAt.After(scheduler.Next(t.CronExpr, now))
}

// executeLoopActionCommand runs task.Command via the system shell and returns
// the combined stdout+stderr. A failed command yields no output (the fire
// stays silent) and is surfaced as a notice.
func (c *Controller) executeLoopActionCommand(task scheduler.Task) string {
	// fireDue invokes this callback synchronously on the ticker goroutine, so
	// a hung command must time out rather than wedge the scheduler.
	ctx, cancel := context.WithTimeout(context.Background(), loopActionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", task.Command)
	cmd.Env = secrets.ProcessEnv() // strip credential env vars from the child
	setShellKillTree(cmd)
	cmd.WaitDelay = loopActionWaitDelay
	cap := &limitedBuffer{}
	cmd.Stdout = cap
	cmd.Stderr = cap
	if err := cmd.Run(); err != nil {
		c.notice(fmt.Sprintf("loopaction task %s: command error — %v", task.ID, err))
		return ""
	}
	out := cap.String()
	if cap.Truncated() {
		// Make boundary-truncated output visible: the LLM (and regex/marker
		// scans) must know the tail was cut rather than silently absent.
		out += "\n…[truncated]…"
	}
	return out
}

// loopActionTimeout bounds a single loopaction command run; the callback runs
// on the scheduler ticker goroutine, so an unbounded command would stall every
// other scheduled task.
const loopActionTimeout = 60 * time.Second

// loopActionWaitDelay lets the killed process group exit before Run returns.
const loopActionWaitDelay = 5 * time.Second

// injectScheduledTask delivers a due scheduled task into the active turn's
// message queue as a labeled steering message. It reports whether the fire
// was accepted: true means the running agent will see the prompt at its next
// natural step (between tool rounds, never mid-tool); false means the caller
// falls back to the parked-turn path (turn in its finishing window, steer
// intake already closed, or no executor bound).
//
// Consent posture: steering runs inside the CURRENT turn's approval context —
// any tool calls the model makes in response pass the same approval gates as
// the running turn's own calls, so a scheduled fire opens no new consent
// surface. The injected message is explicitly labeled as a scheduled task
// (never as the user) and the notice shows a promptPreview, so a poisoned
// prompt is visible in the transcript and UI rather than silently executed.
func (c *Controller) injectScheduledTask(task scheduler.Task) bool {
	if !c.TrySteer(agent.MidTurnScheduledMessage(task.ID, task.Prompt)) {
		return false
	}
	c.scheduler.MarkStarted(task.ID)
	c.notice(fmt.Sprintf("⏰ scheduled task %s injected into the running turn — %s", task.ID, promptPreview(task.Prompt)))
	return true
}

// rearmUnappliedScheduledTask retries a scheduled fire whose injection was
// accepted into the steer queue but never reached the model: the turn ended
// abnormally (cancel, provider error, rotation) and the steer was flushed
// unapplied. Rearming makes the task due again on the next tick — matching
// the parked path's ReleaseFiring semantics for dropped turns, so a fire is
// never silently spent. User steers and plain text carry no scheduled-task
// label and are ignored.
func (c *Controller) rearmUnappliedScheduledTask(text string) {
	// A loopaction fire already ran its command; only prompt-based fires may
	// re-arm. Rearming an output steer would re-execute the side-effecting
	// command after its output was lost to an abnormal turn end.
	if strings.HasPrefix(text, agent.MidTurnScheduledDataPrefix) {
		return
	}
	id, ok := agent.ScheduledTaskID(text)
	if !ok {
		return
	}
	c.mu.Lock()
	sched := c.scheduler
	c.mu.Unlock()
	if sched != nil {
		sched.Rearm(id)
	}
}

// Scheduler exposes the session scheduler to frontends (the TUI's Esc-to-pause
// reads HasPendingDynamic through it).
func (c *Controller) Scheduler() *scheduler.Scheduler {
	return c.scheduler
}

// LoopListText renders the session's scheduled tasks for a local slash command
// (/looplist) — no model call, no tokens spent.
func (c *Controller) LoopListText() string {
	if c.scheduler == nil {
		return "scheduled tasks are unavailable in this session"
	}
	views := c.scheduler.Tasks()
	if len(views) == 0 {
		return "no scheduled tasks"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d scheduled task(s):\n", len(views))
	for _, v := range views {
		schedule := "dynamic"
		if v.CronExpr != "" {
			schedule = v.CronExpr
		}
		next := v.NextFire
		if next == "" {
			if v.Command != "" {
				next = "held" // foreign session's command task: loaded, never auto-run
			} else {
				next = "paused"
			}
		}
		oneShot := ""
		if v.OneShot {
			oneShot = " (one-shot)"
		}
		noExpire := ""
		if v.NoExpire {
			noExpire = " (no expiry)"
		}
		if v.Command != "" {
			fmt.Fprintf(&b, "  %s  %-14s  next %s%s%s  cmd: %s\n", v.ID, schedule, next, oneShot, noExpire, promptPreview(v.Command))
			continue
		}
		fmt.Fprintf(&b, "  %s  %-14s  next %s%s%s\n", v.ID, schedule, next, oneShot, noExpire)
	}
	return strings.TrimRight(b.String(), "\n")
}

// LoopDeleteText cancels a scheduled task by ID for a local slash command
// (/loopdel <id>) — no model call. Returns the confirmation text.
func (c *Controller) LoopDeleteText(id string) string {
	if c.scheduler == nil {
		return "scheduled tasks are unavailable in this session"
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "usage: /loopdel <task-id>"
	}
	if c.scheduler.Delete(id) {
		return "deleted scheduled task " + id
	}
	return "no scheduled task " + id
}
