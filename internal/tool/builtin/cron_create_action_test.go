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

type fakeCommandTaskApprover struct{ allow bool }

func (f fakeCommandTaskApprover) ApproveCommandTask(context.Context, tool.CommandTaskRequest) (bool, string, error) {
	return f.allow, "", nil
}

func cronCreateActionCtx() context.Context {
	return scheduler.NewContext(context.Background(), scheduler.New())
}

// TestCronCreateActionFailsClosedWithoutApprover verifies the security gate:
// a headless context (no interactive approver) cannot register a scheduled
// OS command.
func TestCronCreateActionFailsClosedWithoutApprover(t *testing.T) {
	ctx := cronCreateActionCtx()
	args, _ := json.Marshal(map[string]any{
		"cron":    "5m",
		"command": "bash check.sh",
	})
	out, err := (cronCreateAction{}).Execute(ctx, args)
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

// TestCronCreateActionApproved verifies the approved path registers the task
// with command, pattern, and action_response.
func TestCronCreateActionApproved(t *testing.T) {
	ctx := tool.WithCommandTaskApprover(cronCreateActionCtx(), fakeCommandTaskApprover{allow: true})
	args, _ := json.Marshal(map[string]any{
		"cron":            "5m",
		"command":         "bash check.sh",
		"match_pattern":   "error",
		"action_response": false,
	})
	out, err := (cronCreateAction{}).Execute(ctx, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "bash check.sh") {
		t.Errorf("confirmation missing command: %q", out)
	}
	sched, _ := scheduler.FromContext(ctx)
	views := sched.Tasks()
	if len(views) != 1 {
		t.Fatalf("tasks = %d, want 1", len(views))
	}
	if views[0].Command != "bash check.sh" || views[0].MatchPattern != "error" || views[0].ActionResponse {
		t.Errorf("task = %+v, want command bash check.sh, pattern error, action_response false", views[0])
	}
}

// TestCronCreateActionExpiresIn verifies expires_in registers an ExpiresAt
// deadline on the task and that a bad token is rejected.
func TestCronCreateActionExpiresIn(t *testing.T) {
	ctx := tool.WithCommandTaskApprover(cronCreateActionCtx(), fakeCommandTaskApprover{allow: true})
	args, _ := json.Marshal(map[string]any{
		"cron":       "5m",
		"command":    "bash check.sh",
		"expires_in": "5m",
	})
	out, err := (cronCreateAction{}).Execute(ctx, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "expires:") {
		t.Errorf("confirmation missing expiry: %q", out)
	}
	sched, _ := scheduler.FromContext(ctx)
	views := sched.Tasks()
	if len(views) != 1 {
		t.Fatalf("tasks = %d, want 1", len(views))
	}
	want := time.Now().Add(5 * time.Minute)
	got, err := time.Parse(time.RFC3339, views[0].ExpiresAt)
	if err != nil {
		t.Fatalf("bad ExpiresAt %q: %v", views[0].ExpiresAt, err)
	}
	if got.Before(want.Add(-time.Minute)) || got.After(want.Add(time.Minute)) {
		t.Errorf("ExpiresAt = %v, want ~now+5m (%v)", got, want)
	}

	bad, _ := json.Marshal(map[string]any{
		"cron":       "5m",
		"command":    "bash check.sh",
		"expires_in": "5x",
	})
	if _, err := (cronCreateAction{}).Execute(tool.WithCommandTaskApprover(cronCreateActionCtx(), fakeCommandTaskApprover{allow: true}), bad); err == nil {
		t.Error("invalid expires_in token accepted")
	}
}

// TestCronCreateActionDeclined verifies a declined approval creates nothing.
func TestCronCreateActionDeclined(t *testing.T) {
	ctx := tool.WithCommandTaskApprover(cronCreateActionCtx(), fakeCommandTaskApprover{allow: false})
	args, _ := json.Marshal(map[string]any{
		"cron":    "5m",
		"command": "bash check.sh",
	})
	if _, err := (cronCreateAction{}).Execute(ctx, args); err == nil {
		t.Fatal("Execute with declined approval succeeded")
	}
	sched, _ := scheduler.FromContext(ctx)
	if sched.Count() != 0 {
		t.Error("task was created despite declined approval")
	}
}
