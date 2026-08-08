package control

import (
	"strings"
	"testing"
	"time"

	"reasonix/internal/scheduler"
)

func TestParseLoopDelayArgs(t *testing.T) {
	cases := []struct {
		name           string
		input          string
		wantDelay      time.Duration
		wantAction     bool
		wantPayload    string
		wantMatch      string
		wantActionResp bool
		wantErr        bool
	}{
		{name: "token duration prompt", input: "2m check upstream for commits",
			wantDelay: 2 * time.Minute, wantPayload: "check upstream for commits", wantActionResp: true},
		{name: "natural phrase", input: "in 5 minutes check CI status",
			wantDelay: 5 * time.Minute, wantPayload: "check CI status", wantActionResp: true},
		{name: "action with match", input: "--action 90s --match UPSTREAM-HAS-NEW bash check.sh",
			wantDelay: 90 * time.Second, wantAction: true, wantPayload: "bash check.sh", wantMatch: "UPSTREAM-HAS-NEW", wantActionResp: true},
		{name: "no-ai action", input: "--action 1h --no-ai bash nightly.sh",
			wantDelay: time.Hour, wantAction: true, wantPayload: "bash nightly.sh", wantActionResp: false},
		{name: "hours token", input: "1h30m do the thing",
			wantDelay: 90 * time.Minute, wantPayload: "do the thing", wantActionResp: true},
		{name: "missing payload", input: "2m", wantErr: true},
		{name: "bad duration", input: "soon do the thing", wantErr: true},
		{name: "empty input", input: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delay, action, payload, match, resp, err := parseLoopDelayArgs(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseLoopDelayArgs(%q) = nil err, want error", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLoopDelayArgs(%q): %v", tc.input, err)
			}
			if delay != tc.wantDelay {
				t.Errorf("delay = %v, want %v", delay, tc.wantDelay)
			}
			if action != tc.wantAction {
				t.Errorf("action = %v, want %v", action, tc.wantAction)
			}
			if payload != tc.wantPayload {
				t.Errorf("payload = %q, want %q", payload, tc.wantPayload)
			}
			if match != tc.wantMatch {
				t.Errorf("match = %q, want %q", match, tc.wantMatch)
			}
			if resp != tc.wantActionResp {
				t.Errorf("actionResponse = %v, want %v", resp, tc.wantActionResp)
			}
		})
	}
}

func TestStartLoopDelayCreatesFutureOneShot(t *testing.T) {
	c := &Controller{scheduler: scheduler.New()}
	note, err := c.StartLoopDelay("2m check upstream and report, do not merge")
	if err != nil {
		t.Fatalf("StartLoopDelay: %v", err)
	}
	if !strings.Contains(note, "fires in 2m") {
		t.Errorf("note = %q, want mention of 'fires in 2m'", note)
	}
	views := c.scheduler.Tasks()
	if len(views) != 1 {
		t.Fatalf("tasks = %d, want 1", len(views))
	}
	v := views[0]
	if !v.OneShot {
		t.Error("delayed trigger is not one-shot")
	}
	next, err := time.Parse(time.RFC3339, v.NextFire)
	if err != nil {
		t.Fatalf("NextFire %q: %v", v.NextFire, err)
	}
	if d := next.Sub(time.Now()); d < time.Minute || d > 3*time.Minute {
		t.Errorf("NextFire %v is %v away, want ~2m", next, d)
	}
}

func TestStartLoopDelayActionCreatesFutureCommandTask(t *testing.T) {
	c := &Controller{scheduler: scheduler.New()}
	note, err := c.StartLoopDelay("--action 2m --match UPSTREAM-HAS-NEW bash check.sh")
	if err != nil {
		t.Fatalf("StartLoopDelay: %v", err)
	}
	if !strings.Contains(note, "action trigger set") {
		t.Errorf("note = %q, want 'action trigger set'", note)
	}
	views := c.scheduler.Tasks()
	if len(views) != 1 {
		t.Fatalf("tasks = %d, want 1", len(views))
	}
	v := views[0]
	if v.Command != "bash check.sh" {
		t.Errorf("Command = %q, want %q", v.Command, "bash check.sh")
	}
	if v.MatchPattern != "UPSTREAM-HAS-NEW" {
		t.Errorf("MatchPattern = %q, want %q", v.MatchPattern, "UPSTREAM-HAS-NEW")
	}
	if !v.OneShot {
		t.Error("delayed action is not one-shot")
	}
}
