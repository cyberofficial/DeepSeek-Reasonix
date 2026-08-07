package tool

import "context"

// CommandTaskRequest describes a scheduled OS-command task the agent wants to
// register (cron_create_action).
type CommandTaskRequest struct {
	// Command is the exact shell command that will run on each fire.
	Command string
	// Schedule is the cron expression or interval token the task recurs on.
	Schedule string
}

// CommandTaskApprover asks the user to confirm a scheduled OS-command task
// before it is registered. It is a fresh human decision: YOLO/auto approval
// must not answer it, and a nil approver (headless runs, sub-agent loops with
// no interactive parent) fails closed.
type CommandTaskApprover interface {
	ApproveCommandTask(ctx context.Context, req CommandTaskRequest) (allow bool, reason string, err error)
}

type commandTaskApproverContextKey struct{}

// WithCommandTaskApprover stamps an interactive command-task approver onto a
// tool execution context.
func WithCommandTaskApprover(ctx context.Context, approver CommandTaskApprover) context.Context {
	if approver == nil {
		return ctx
	}
	return context.WithValue(ctx, commandTaskApproverContextKey{}, approver)
}

// CommandTaskApproverFrom returns the command-task approver carried by ctx.
func CommandTaskApproverFrom(ctx context.Context) (CommandTaskApprover, bool) {
	if ctx == nil {
		return nil, false
	}
	approver, ok := ctx.Value(commandTaskApproverContextKey{}).(CommandTaskApprover)
	return approver, ok && approver != nil
}
