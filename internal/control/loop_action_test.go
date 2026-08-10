package control

import (
	"strings"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/scheduler"
	"reasonix/internal/tool"
)

func TestParseLoopActionArgs(t *testing.T) {
	cases := []struct {
		name           string
		input          string
		interval       string
		command        string
		matchPattern   string
		actionResponse bool
		noExpire       bool
		wantErr        bool
	}{
		{
			name:           "bare command defaults",
			input:          "bash check.sh",
			interval:       "",
			command:        "bash check.sh",
			actionResponse: true,
		},
		{
			name:           "interval and command",
			input:          "5m bash check.sh",
			interval:       "5m",
			command:        "bash check.sh",
			actionResponse: true,
		},
		{
			name:           "match flag",
			input:          "5m --match error bash check.sh",
			interval:       "5m",
			command:        "bash check.sh",
			matchPattern:   "error",
			actionResponse: true,
		},
		{
			name:           "match with double space keeps command and does not loop",
			input:          "5m --match  error   bash check.sh",
			interval:       "5m",
			command:        "bash check.sh",
			matchPattern:   "error",
			actionResponse: true,
		},
		{
			name:           "no-ai stays off with a command",
			input:          "5m --no-ai bash check.sh",
			interval:       "5m",
			command:        "bash check.sh",
			actionResponse: false,
		},
		{
			name:           "no-ai plus match",
			input:          "--no-ai --match error bash check.sh",
			interval:       "",
			command:        "bash check.sh",
			matchPattern:   "error",
			actionResponse: false,
		},
		{
			name:           "forever flag",
			input:          "--forever 2h bash check.sh",
			interval:       "2h",
			command:        "bash check.sh",
			actionResponse: true,
			noExpire:       true,
		},
		{
			name:           "quoted command survives",
			input:          "5m bash 'check x.sh'",
			interval:       "5m",
			command:        "bash 'check x.sh'",
			actionResponse: true,
		},
		{
			name:    "match without pattern errors",
			input:   "5m --match",
			wantErr: true,
		},
		{
			name:           "flags in any order",
			input:          "--no-ai --forever --match err 3m bash x",
			interval:       "3m",
			command:        "bash x",
			matchPattern:   "err",
			actionResponse: false,
			noExpire:       true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			interval, command, matchPattern, actionResponse, noExpire, err := parseLoopActionArgs(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseLoopActionArgs(%q) = %q, want error", tc.input, command)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLoopActionArgs(%q): %v", tc.input, err)
			}
			if interval != tc.interval || command != tc.command || matchPattern != tc.matchPattern ||
				actionResponse != tc.actionResponse || noExpire != tc.noExpire {
				t.Errorf("parseLoopActionArgs(%q) = (%q, %q, %q, %v, %v), want (%q, %q, %q, %v, %v)",
					tc.input, interval, command, matchPattern, actionResponse, noExpire,
					tc.interval, tc.command, tc.matchPattern, tc.actionResponse, tc.noExpire)
			}
		})
	}
}

// TestIsFinalFire covers the expired-annotation decision: a fire is final when
// the next cron slot lands at or after the expiry deadline, and never for
// tasks without an expiry, without a cron, or with the deadline still ahead.
func TestIsFinalFire(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		cron      string
		expiresAt time.Time
		want      bool
	}{
		{"no expiry", "*/1 * * * *", time.Time{}, false},
		{"dynamic (no cron)", "", now.Add(5 * time.Minute), false},
		{"deadline after next slot", "*/5 * * * *", now.Add(10 * time.Minute), false},
		{"deadline exactly at next slot", "*/5 * * * *", now.Add(5 * time.Minute), true},
		{"deadline before next slot", "*/5 * * * *", now.Add(4 * time.Minute), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := scheduler.Task{CronExpr: tc.cron, ExpiresAt: tc.expiresAt}
			if got := isFinalFire(task, now); got != tc.want {
				t.Errorf("isFinalFire(cron=%q, expires=%v) = %v, want %v", tc.cron, tc.expiresAt, got, tc.want)
			}
		})
	}
}

// TestRearmSkipsDataFramedOutput verifies that a loopaction output steer lost
// to an abnormal turn end never re-arms the task: the command already ran, so
// re-arming would re-execute it. Prompt-framed fires still re-arm.
func TestRearmSkipsDataFramedOutput(t *testing.T) {
	sched := scheduler.New()
	id, err := sched.Add("*/5 * * * *", "check", time.Now(), false, false, false)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	c := &Controller{scheduler: sched}
	_, before, ok := sched.NextDue()
	if !ok {
		t.Fatal("task has no next fire")
	}

	// Data-framed output: rearm must not fire.
	c.rearmUnappliedScheduledTask(agent.MidTurnScheduledOutput(id, "boom"))
	_, after, ok := sched.NextDue()
	if !ok || !after.Equal(before) {
		t.Errorf("data-framed output re-armed the task (next %v -> %v); want the original cron slot", before, after)
	}

	// Prompt-framed fire: rearm still applies.
	c.rearmUnappliedScheduledTask(agent.MidTurnScheduledMessage(id, "check"))
	_, after, ok = sched.NextDue()
	if !ok || time.Since(after) > 5*time.Second {
		t.Errorf("prompt fire did not re-arm (next=%v, ok=%v); want ~now", after, ok)
	}
}

// TestRunCommandActionIdleDeliversOutputAsTurn verifies the idle fallback: a
// loopaction fire whose steer finds no running turn (TrySteer requires an
// active turn) must run the output as a full parked turn instead of silently
// dropping it — the regression the one-shot 30s trigger hit: the task fired
// and self-deleted with no visible output.
func TestRunCommandActionIdleDeliversOutputAsTurn(t *testing.T) {
	prov := &scriptedTurns{turns: [][]provider.Chunk{{
		{Type: provider.ChunkText, Text: "ok"},
		{Type: provider.ChunkDone},
	}}}
	ag := agent.New(prov, tool.NewRegistry(), agent.NewSession("sys"),
		agent.Options{ContextWindow: 1_000_000}, event.Discard)
	sched := scheduler.New()
	c := New(Options{Runner: ag, Executor: ag, Scheduler: sched})
	id, err := sched.AddAction("", `bash -c "echo hello"`, "", true, false, true, time.Time{})
	if err != nil {
		t.Fatalf("AddAction: %v", err)
	}
	task := scheduler.Task{ID: id, Command: `bash -c "echo hello"`, OneShot: true, ActionResponse: true}
	c.runCommandAction(task)
	waitIdle(t, c)
	defer c.Close()
	found := false
	for _, m := range ag.Session().Messages {
		if strings.Contains(m.Content, "loopaction task "+id+" output:") && strings.Contains(m.Content, "hello") {
			found = true
		}
	}
	if !found {
		joined := ""
		for _, m := range ag.Session().Messages {
			joined += m.Content + "\n"
		}
		t.Errorf("idle fire output not delivered as a turn; session:\n%s", joined)
	}
}

// TestLoopListShowsCommandTasks ensures held command tasks are labelled and
// their command is previewed so scheduled payloads stay auditable.
func TestLoopListShowsCommandTasks(t *testing.T) {
	sched := scheduler.New()
	sched.SetOrigin("session-a")
	if _, err := sched.AddAction("*/5 * * * *", "bash check.sh", "", false, false, true, time.Time{}); err != nil {
		t.Fatalf("AddAction: %v", err)
	}
	c := &Controller{scheduler: sched}
	text := c.LoopListText()
	if !strings.Contains(text, "cmd: bash check.sh") {
		t.Errorf("looplist missing command preview:\n%s", text)
	}
}
