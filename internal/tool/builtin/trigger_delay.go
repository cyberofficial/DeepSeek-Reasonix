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

func init() { tool.RegisterBuiltin(triggerDelay{}) }

// triggerDelay creates a one-shot countdown trigger: the prompt (or, with
// command, the host command) fires once after the delay and the task deletes
// itself. The LLM can keep it going afterwards via schedule_wakeup, turning
// the one-shot into a loop. It is the agent-facing twin of /loopdelay.
type triggerDelay struct{}

func (triggerDelay) Name() string { return "trigger_delay" }

func (triggerDelay) Description() string {
	return "Create a one-shot countdown trigger that fires once after a delay " +
		"(e.g. \"check in 30s\"). With prompt, the LLM receives the prompt at fire " +
		"time; with command, the host runs the command and the LLM is woken only " +
		"if the output matches match_pattern (or action_response=true). " +
		"The task deletes itself after firing; the LLM can continue it with " +
		"schedule_wakeup. Use with cron_list/cron_delete."
}

func (triggerDelay) Schema() json.RawMessage {
	return json.RawMessage(`{
"type":"object",
"properties":{
  "delay":{"type":"string","description":"How long until the single fire, e.g. \"30s\", \"2m\", \"1h30m\"."},
  "prompt":{"type":"string","description":"The prompt to deliver when the trigger fires."},
  "command":{"type":"string","description":"The OS command to run when the trigger fires (alternative to prompt)."},
  "match_pattern":{"type":"string","description":"Regex to test against command output (optional)."},
  "action_response":{"type":"boolean","description":"Invoke the LLM with command output when matched (default true)."}
},
"required":["delay"]
}`)
}

func (triggerDelay) ReadOnly() bool { return false }

func (triggerDelay) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	sched, ok := scheduler.FromContext(ctx)
	if !ok || sched == nil {
		return "", fmt.Errorf("trigger_delay: no scheduler in this context")
	}
	var in struct {
		Delay          string `json:"delay"`
		Prompt         string `json:"prompt"`
		Command        string `json:"command"`
		MatchPattern   string `json:"match_pattern"`
		ActionResponse *bool  `json:"action_response"` // nil = caller did not pass it
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("trigger_delay: %v", err)
	}
	delay, err := time.ParseDuration(strings.TrimSpace(in.Delay))
	if err != nil || delay <= 0 {
		return "", fmt.Errorf("trigger_delay: %q is not a valid duration (e.g. 30s, 2m, 1h30m)", in.Delay)
	}
	prompt := strings.TrimSpace(in.Prompt)
	command := strings.TrimSpace(in.Command)
	if prompt == "" && command == "" {
		return "", fmt.Errorf("trigger_delay: a prompt or a command is required")
	}
	at := time.Now().Add(delay)
	var id string
	if command != "" {
		actionResponse := true // default: wake the LLM when output needs a decision
		if in.ActionResponse != nil {
			actionResponse = *in.ActionResponse
		}
		// The command runs later, unattended and unsandboxed, so registering it
		// is a fresh human decision: an interactive approver must confirm, and
		// a headless context (no approver) fails closed.
		approver, ok := tool.CommandTaskApproverFrom(ctx)
		if !ok {
			return "", fmt.Errorf("trigger_delay: scheduled OS commands require interactive confirmation, which is unavailable in this context")
		}
		allow, reason, err := approver.ApproveCommandTask(ctx, tool.CommandTaskRequest{Command: command, Schedule: "once"})
		if err != nil {
			return "", fmt.Errorf("trigger_delay: %v", err)
		}
		if !allow {
			if reason != "" {
				return "", fmt.Errorf("trigger_delay: %s", reason)
			}
			return "", fmt.Errorf("trigger_delay: command trigger declined")
		}
		id, err = sched.AddActionAt("", command, strings.TrimSpace(in.MatchPattern),
			true, false, actionResponse, time.Time{}, at)
	} else {
		id, err = sched.Add("", prompt, at, true, false, false)
	}
	if err != nil {
		return "", fmt.Errorf("trigger_delay: %v", err)
	}
	kind := "trigger"
	if command != "" {
		kind = "action trigger"
	}
	out := fmt.Sprintf("%s set — task %s fires once in %s at %s",
		kind, id, formatDelay(delay), at.Format("15:04:05"))
	if command != "" {
		out += "\ncommand: " + promptPreviewForTool(command)
	}
	return out, nil
}

// formatDelay renders a duration for notices, e.g. "2m0s" as "2m".
func formatDelay(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		return strings.TrimSuffix(s, "0s")
	}
	return s
}
