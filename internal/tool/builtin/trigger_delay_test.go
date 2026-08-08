package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"reasonix/internal/scheduler"
	"reasonix/internal/tool"
)

func triggerDelayCtx() context.Context {
	return scheduler.NewContext(context.Background(), scheduler.New())
}

// TestTriggerDelayPromptCreatesFutureOneShot verifies the prompt variant: a
// one-shot task with a future NextFire.
func TestTriggerDelayPromptCreatesFutureOneShot(t *testing.T) {
	ctx := triggerDelayCtx()
	args, _ := json.Marshal(map[string]any{
		"delay":  "30s",
		"prompt": "check upstream commits",
	})
	out, err := (triggerDelay{}).Execute(ctx, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "fires once in 30s") {
		t.Errorf("out = %q, want 'fires once in 30s'", out)
	}
	sched, _ := scheduler.FromContext(ctx)
	views := sched.Tasks()
	if len(views) != 1 {
		t.Fatalf("tasks = %d, want 1", len(views))
	}
	v := views[0]
	if !v.OneShot {
		t.Error("trigger is not one-shot")
	}
	next, err := time.Parse(time.RFC3339, v.NextFire)
	if err != nil {
		t.Fatalf("NextFire %q: %v", v.NextFire, err)
	}
	if d := next.Sub(time.Now()); d < 10*time.Second || d > 2*time.Minute {
		t.Errorf("NextFire %v is %v away, want ~30s", next, d)
	}
	if v.Prompt != "check upstream commits" {
		t.Errorf("Prompt = %q", v.Prompt)
	}
}

// TestTriggerDelayActionFailsClosedWithoutApprover verifies the command
// variant keeps the security gate: no interactive approver, no task.
func TestTriggerDelayActionFailsClosedWithoutApprover(t *testing.T) {
	ctx := triggerDelayCtx()
	args, _ := json.Marshal(map[string]any{
		"delay":   "30s",
		"command": "bash check.sh",
	})
	out, err := (triggerDelay{}).Execute(ctx, args)
	if err == nil {
		t.Fatalf("Execute without approver succeeded: %q", out)
	}
	if !strings.Contains(err.Error(), "interactive confirmation") {
		t.Errorf("error = %q, want fail-closed message", err)
	}
	sched, _ := scheduler.FromContext(ctx)
	if sched.Count() != 0 {
		t.Error("task was created despite fail-closed")
	}
}

// TestTriggerDelayActionApproved verifies the approved command variant
// registers a one-shot action task with the match pattern.
func TestTriggerDelayActionApproved(t *testing.T) {
	ctx := tool.WithCommandTaskApprover(triggerDelayCtx(), fakeCommandTaskApprover{allow: true})
	args, _ := json.Marshal(map[string]any{
		"delay":         "2m",
		"command":       "bash check.sh",
		"match_pattern": "UPSTREAM-HAS-NEW",
	})
	out, err := (triggerDelay{}).Execute(ctx, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "action trigger set") {
		t.Errorf("out = %q, want 'action trigger set'", out)
	}
	sched, _ := scheduler.FromContext(ctx)
	views := sched.Tasks()
	if len(views) != 1 {
		t.Fatalf("tasks = %d, want 1", len(views))
	}
	v := views[0]
	if !v.OneShot {
		t.Error("action trigger is not one-shot")
	}
	if v.Command != "bash check.sh" {
		t.Errorf("Command = %q", v.Command)
	}
	if v.MatchPattern != "UPSTREAM-HAS-NEW" {
		t.Errorf("MatchPattern = %q", v.MatchPattern)
	}
}

// TestTriggerDelayValidatesArgs verifies required-field and duration errors.
func TestTriggerDelayValidatesArgs(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{name: "bad duration", args: map[string]any{"delay": "soon", "prompt": "x"}},
		{name: "missing payload", args: map[string]any{"delay": "30s"}},
		{name: "missing delay", args: map[string]any{"prompt": "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(tc.args)
			if _, err := (triggerDelay{}).Execute(triggerDelayCtx(), args); err == nil {
				t.Error("Execute succeeded, want error")
			}
		})
	}
}
