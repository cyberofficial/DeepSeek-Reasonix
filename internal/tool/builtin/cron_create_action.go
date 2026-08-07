package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/scheduler"
	"reasonix/internal/tool"
)

func init() { tool.RegisterBuiltin(cronCreateAction{}) }

// cronCreateAction schedules a command-based task: on each fire the host runs
// the command and the LLM is only woken when the output needs a decision. The
// agent calls it when the user asks for an OS-level scheduled check (watch a
// log, poll an API, verify a deploy) that should not burn a model call when
// nothing changed.
type cronCreateAction struct{}

func (cronCreateAction) Name() string { return "cron_create_action" }

func (cronCreateAction) Description() string {
	return "Create a scheduled task that runs a host command every interval. " +
		"Set match_pattern (regex) to only invoke the LLM when output matches; " +
		"set action_response=false to skip the LLM wake (output still shows in chat). " +
		"Set expires_in (e.g. 5m/2h/1d) to self-delete the task after that long. " +
		"The command may emit 'ai-response: true' or 'ai-response: false' on " +
		"stdout to force or suppress the LLM wake. " +
		"Use with /loopaction or cron_list/cron_delete."
}

func (cronCreateAction) Schema() json.RawMessage {
	return json.RawMessage(`{
"type":"object",
"properties":{
  "cron":{"type":"string","description":"5-field cron expression (minute hour dom month dow), e.g. \"*/5 * * * *\", or an interval token like \"5m\"/\"2h\"/\"1d\"."},
  "command":{"type":"string","description":"The OS command to run (bash script or binary)."},
  "match_pattern":{"type":"string","description":"Regex to test against command output (optional)."},
  "action_response":{"type":"boolean","description":"Invoke the LLM with output when matched (default true); false = never wake the LLM."},
  "no_expire":{"type":"boolean","description":"Set true for an endless loop that never expires (default: tasks expire after 7 days)."},
  "expires_in":{"type":"string","description":"Expiry token like 5m/2h/1d: the task self-deletes after this long (default: 7-day expiry)."}
},
"required":["cron","command"]
}`)
}

func (cronCreateAction) ReadOnly() bool { return false }

func (cronCreateAction) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	sched, ok := scheduler.FromContext(ctx)
	if !ok || sched == nil {
		return "", fmt.Errorf("cron_create_action: no scheduler in this context")
	}
	var in struct {
		Cron           string `json:"cron"`
		Command        string `json:"command"`
		MatchPattern   string `json:"match_pattern"`
		ActionResponse *bool  `json:"action_response"` // nil = caller did not pass it
		NoExpire       bool   `json:"no_expire"`
		ExpiresIn      string `json:"expires_in"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("cron_create_action: %v", err)
	}
	cron := strings.TrimSpace(in.Cron)
	command := strings.TrimSpace(in.Command)
	if cron == "" || command == "" {
		return "", fmt.Errorf("cron_create_action: cron and command are required")
	}
	if interval, ok := scheduler.ParseInterval(cron); ok {
		cron = interval
	} else if !scheduler.Valid(cron) {
		return "", fmt.Errorf("cron_create_action: %q is neither a 5-field cron expression nor a valid interval token (s/m/h/d)", in.Cron)
	}
	actionResponse := true // default: wake the LLM when output needs a decision
	if in.ActionResponse != nil {
		actionResponse = *in.ActionResponse // explicit false = silent checks
	}
	// The command runs later, unattended and unsandboxed, so registering it is
	// a fresh human decision: an interactive approver must confirm, and a
	// headless context (no approver) fails closed.
	approver, ok := tool.CommandTaskApproverFrom(ctx)
	if !ok {
		return "", fmt.Errorf("cron_create_action: scheduled OS commands require interactive confirmation, which is unavailable in this context")
	}
	allow, reason, err := approver.ApproveCommandTask(ctx, tool.CommandTaskRequest{Command: command, Schedule: cron})
	if err != nil {
		return "", fmt.Errorf("cron_create_action: %v", err)
	}
	if !allow {
		if reason != "" {
			return "", fmt.Errorf("cron_create_action: %s", reason)
		}
		return "", fmt.Errorf("cron_create_action: command task declined")
	}
	var expiresAt time.Time
	if in.ExpiresIn != "" {
		d, ok := scheduler.ParseDurationToken(in.ExpiresIn)
		if !ok {
			return "", fmt.Errorf("cron_create_action: %q is not a valid expiry token (s/m/h/d)", in.ExpiresIn)
		}
		expiresAt = time.Now().Add(d)
	}
	id, err := sched.AddAction(cron, command, strings.TrimSpace(in.MatchPattern),
		false, in.NoExpire, actionResponse, expiresAt)
	if err != nil {
		return "", fmt.Errorf("cron_create_action: %v", err)
	}
	out := fmt.Sprintf("created action task %s — schedule %s, ai-response: %v\ncommand: %s", id, cron, actionResponse, promptPreviewForTool(command))
	if !expiresAt.IsZero() {
		out += fmt.Sprintf("\nexpires: %s", expiresAt.Format(time.RFC3339))
	}
	return out, nil
}
