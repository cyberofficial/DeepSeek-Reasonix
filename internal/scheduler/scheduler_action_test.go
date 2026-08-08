package scheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAddActionStampsOriginAndForeignLoadHolds verifies the security gate on
// command tasks: AddAction stamps the creating session, Load pauses command
// tasks from other sessions (never auto-run), and the creating session's own
// load re-arms them normally.
func TestAddActionStampsOriginAndForeignLoadHolds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scheduled-tasks.json")
	s := New()
	s.SetOrigin("session-a")
	s.SetPersistPath(path)
	id, err := s.AddAction("*/5 * * * *", "bash check.sh", "error", false, true, true, time.Time{})
	if err != nil {
		t.Fatalf("AddAction: %v", err)
	}
	views := s.Tasks()
	if len(views) != 1 || views[0].Session != "session-a" || views[0].Command != "bash check.sh" {
		t.Fatalf("task = %+v, want session-a origin with command", views[0])
	}
	s.Flush()

	// A different session loads the same sidecar: the command task is held.
	other := New()
	other.SetOrigin("session-b")
	other.Load(path)
	otherViews := other.Tasks()
	if len(otherViews) != 1 {
		t.Fatalf("held task must stay visible, got %d tasks", len(otherViews))
	}
	if next := otherViews[0].NextFire; next != "" {
		t.Errorf("foreign command task not held: next = %q, want empty (paused)", next)
	}
	// Held tasks must not fire.
	fired := false
	other.OnFire(func(Task) { fired = true })
	other.OnCommand(func(Task) { fired = true })
	other.fireDue()
	if fired {
		t.Error("held task fired")
	}

	// The owning session's load re-arms the same task.
	owner := New()
	owner.SetOrigin("session-a")
	owner.Load(path)
	ownerViews := owner.Tasks()
	if len(ownerViews) != 1 || ownerViews[0].NextFire == "" {
		t.Fatalf("owning session must re-arm the task, got %+v", ownerViews)
	}
	if ownerViews[0].ID != id {
		t.Errorf("task id changed on round-trip: %s != %s", ownerViews[0].ID, id)
	}
}

// TestAddActionExpiresAt verifies the explicit expiry: a task whose ExpiresAt
// has passed is dropped by fireDue (never fires again) and by Load (not
// restored from the sidecar).
func TestAddActionExpiresAt(t *testing.T) {
	s := New()
	expired, err := s.AddAction("*/5 * * * *", "bash check.sh", "", false, false, true, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("AddAction: %v", err)
	}
	live, err := s.AddAction("*/5 * * * *", "bash live.sh", "", false, false, true, time.Now().Add(10*time.Minute))
	if err != nil {
		t.Fatalf("AddAction: %v", err)
	}
	fired := false
	s.OnCommand(func(Task) { fired = true })
	s.fireDue()
	if fired {
		t.Error("expired task fired")
	}
	views := s.Tasks()
	for _, v := range views {
		if v.ID == expired {
			t.Error("expired task still present after fireDue")
		}
	}
	if len(views) != 1 || views[0].ID != live {
		t.Errorf("tasks = %d (%s), want only the live task %s", len(views), views[0].ID, live)
	}

	// Load drops an expired entry from the sidecar.
	dir := t.TempDir()
	path := filepath.Join(dir, "scheduled-tasks.json")
	sidecar := `[{"id":"expired1","cron":"*/5 * * * *","command":"bash evil.sh","noExpire":true,"created":"` +
		time.Now().Add(-time.Hour).Format(time.RFC3339) + `","nextFire":"` +
		time.Now().Add(-time.Minute).Format(time.RFC3339) + `","expiresAt":"` +
		time.Now().Add(-time.Minute).Format(time.RFC3339) + `"}]`
	if err := os.WriteFile(path, []byte(sidecar), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	s2 := New()
	s2.SetOrigin("session-x")
	s2.Load(path)
	if n := len(s2.Tasks()); n != 0 {
		t.Errorf("Load kept %d expired task(s), want 0", n)
	}
}

// TestParseDurationToken covers the expiry-token grammar used by expires_in.
func TestParseDurationToken(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  time.Duration
		ok    bool
	}{
		{"30s", 30 * time.Second, true},
		{"5m", 5 * time.Minute, true},
		{"2h", 2 * time.Hour, true},
		{"1d", 24 * time.Hour, true},
		{"", 0, false},
		{"5x", 0, false},
		{"m", 0, false},
	} {
		got, ok := ParseDurationToken(tc.token)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("ParseDurationToken(%q) = (%v, %v), want (%v, %v)", tc.token, got, ok, tc.want, tc.ok)
		}
	}
}

// TestLoadHoldsUnattributedCommandTasks verifies that a repo-shipped sidecar
// entry (no session origin) is held rather than executed.
func TestLoadHoldsUnattributedCommandTasks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scheduled-tasks.json")
	sidecar := `[{"id":"deadbeef","cron":"*/5 * * * *","command":"bash evil.sh","noExpire":true,"created":"` +
		time.Now().Add(-time.Hour).Format(time.RFC3339) + `","nextFire":"` +
		time.Now().Add(time.Minute).Format(time.RFC3339) + `"}]`
	if err := os.WriteFile(path, []byte(sidecar), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	s := New()
	s.SetOrigin("session-x")
	s.Load(path)
	views := s.Tasks()
	if len(views) != 1 {
		t.Fatalf("tasks = %d, want 1 held entry", len(views))
	}
	if views[0].NextFire != "" {
		t.Errorf("unattributed command task not held: next = %q", views[0].NextFire)
	}
}

// TestAddActionAtDelayed verifies a delayed one-shot action: the task is
// created with a future NextFire, does not fire before it, fires once when
// due, and deletes itself after delivery.
func TestAddActionAtDelayed(t *testing.T) {
	s := New()
	var fired []Task
	s.OnCommand(func(t Task) { fired = append(fired, t) })
	at := time.Now().Add(2 * time.Minute)
	id, err := s.AddActionAt("", "bash check.sh", "UPSTREAM-HAS-NEW", true, false, true, time.Time{}, at)
	if err != nil {
		t.Fatalf("AddActionAt: %v", err)
	}
	s.mu.Lock()
	task := s.tasks[id]
	future := task.NextFire.After(time.Now())
	oneShot := task.OneShot
	s.mu.Unlock()
	if !future {
		t.Error("delayed action NextFire is not in the future")
	}
	if !oneShot {
		t.Error("delayed action is not one-shot")
	}
	s.fireDue()
	if len(fired) != 0 {
		t.Fatalf("fired before the delay = %d, want 0", len(fired))
	}
	s.mu.Lock()
	s.tasks[id].NextFire = time.Now().Add(-time.Second)
	s.mu.Unlock()
	s.fireDue()
	if len(fired) != 1 {
		t.Fatalf("fired = %d, want 1", len(fired))
	}
	s.mu.Lock()
	_, gone := s.tasks[id]
	s.mu.Unlock()
	if gone {
		t.Error("one-shot action still present after delivery")
	}
}
