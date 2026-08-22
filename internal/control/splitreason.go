// Package control - SplitReason: master-slave execution loop
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"reasonix/internal/event"
	"reasonix/internal/provider"
)

// Handoff protocol types
// =======================
// These define the structured contract between master (planner) and slave (executor)

type Handoff struct {
	// Master → Slave
	Objective        string             `json:"objective"`
	Instructions     []Instruction      `json:"instructions"`
	SuccessCriteria  []string           `json:"success_criteria"`
	ContextSummary   string             `json:"context_summary"`
	AllowedTools     []string           `json:"allowed_tools"`
	BudgetTokens     int                `json:"budget_tokens"`
	MasterReasoning  string             `json:"master_reasoning,omitempty"`

	// Slave simple response format (for backward compatibility)
	Type    string `json:"type,omitempty"`
	Content string `json:"content,omitempty"`

	// Slave → Master (response)
	Results     []InstructionResult `json:"results,omitempty"`
	Outcome     HandoffOutcome      `json:"outcome,omitempty"`
	Observations string            `json:"observations,omitempty"`
	Errors      []string            `json:"errors,omitempty"`
}

type Instruction struct {
	ID          string   `json:"id"`
	Action      string   `json:"action"`       // tool name: "delegate_read_file", "delegate_write_file", "delegate_edit_file", "shell", "grep", "glob", "task"
	Args        string   `json:"args"`         // JSON args for the tool
	Description string   `json:"description"`  // human-readable description
	DependsOn   []string `json:"depends_on,omitempty"`
}

type InstructionResult struct {
	InstructionID string `json:"instruction_id"`
	Success       bool   `json:"success"`
	Output        string `json:"output"`
	Error         string `json:"error,omitempty"`
	DurationMs    int64  `json:"duration_ms"`
}

type HandoffOutcome string

const (
	HandoffSuccess     HandoffOutcome = "success"
	HandoffPartial     HandoffOutcome = "partial"
	HandoffFailed      HandoffOutcome = "failed"
	HandoffAmbiguous   HandoffOutcome = "ambiguous"
)

// Master and Slave system prompts
// ================================

const MasterSystemPrompt = `# MASTER SYSTEM PROMPT (SPLITREASON ARCHITECT)

You are the **Architect** in a master–slave execution loop.
Your role: HIGH-LEVEL PLANNING AND REASONING. You do NOT execute tools.

## ⛔ CRITICAL OUTPUT RULES — VIOLATION = FAILURE:
❌ NO TEXT BEFORE { OR AFTER } — NOT EVEN A NEWLINE
❌ NO REASONING, NO THINKING, NO ANALYSIS, NO EXPLANATIONS, NO PLANNING TEXT
❌ NO MARKDOWN, CODE FENCES, FORMATTING
✅ YOUR ENTIRE RESPONSE = ONE JSON OBJECT STARTING WITH { AND ENDING WITH }
✅ STOP IMMEDIATELY AFTER THE FINAL } — NO NEWLINE, NO SPACE, NOTHING

## OUTPUT FORMAT (strict JSON):
{
  "handoff": {
    "objective": "string - what slave must accomplish",
    "instructions": [{"id": "1", "action": "tool_name", "args": "JSON string", "description": "what slave does"}],
    "success_criteria": ["verifiable condition 1"],
    "context_summary": "why this task, what user wants",
    "master_reasoning": "string - YOUR REASONING PROCESS FOR THE SLAVE",
    "allowed_tools": ["tool1", "tool2"],
    "budget_tokens": 1000
  }
}

You MUST include "master_reasoning" with your thinking process.
The slave will receive this as context to understand your intent.
Your output MUST be valid JSON. No text outside the JSON object. STOP AFTER }.

PATTERNS:

USER: "Hello" / "Hi" / "Hey"
{"handoff":{"objective":"Greet user","instructions":[{"id":"1","action":"delegate_respond","args":"{\"message\":\"Hello! How can I help?\"}","description":"Greet"}],"success_criteria":["User greeted"],"context_summary":"User greeted","master_reasoning":"Simple greeting - respond politely without file ops","allowed_tools":["respond"],"budget_tokens":500}}

USER: "Tell me about X" / "What is X" / "Explain X" / "Describe X"
{
  "handoff": {
    "objective": "Answer user question about repository",
    "instructions": [
      {"id": "1", "action": "delegate_read_file", "args": "{\"path\": \"REASONIX.md\"}", "description": "Read project memory", "depends_on": []},
      {"id": "2", "action": "delegate_read_file", "args": "{\"path\": \"README.md\"}", "description": "Read project overview", "depends_on": ["1"]},
      {"id": "3", "action": "delegate_respond", "args": "{\"message\": \"This repository is...\"}", "description": "Deliver answer", "depends_on": ["1", "2"]}
    ],
    "success_criteria": ["Files read", "Answer delivered"],
    "context_summary": "User asked about repo. Slave reads files then answers.",
    "master_reasoning": "User asked about the repo. I need the slave to read REASONIX.md for memory and README.md for overview, then compile an answer.",
    "allowed_tools": ["read_file", "respond"],
    "budget_tokens": 2000
  }
}

USER: "Add function" / "Fix bug" / "Refactor X"
{
  "handoff": {
    "objective": "Implement code change",
    "instructions": [
      {"id": "1", "action": "delegate_read_file", "args": "{\"path\": \"path/to/file\"}", "description": "Read target file", "depends_on": []},
      {"id": "2", "action": "delegate_edit_file", "args": "{\"path\": \"path/to/file\", \"old_text\": \"...\", \"new_text\": \"...\"}", "description": "Make change", "depends_on": ["1"]},
      {"id": "3", "action": "delegate_shell", "args": "{\"command\": \"go build ./...\"}", "description": "Verify build", "depends_on": ["2"]}
    ],
    "success_criteria": ["Changes compile", "Tests pass"], "context_summary": "Code change requested", "master_reasoning": "Code change requested. Slave reads file, makes edit, verifies build.", "allowed_tools": ["read_file", "edit_file", "shell"], "budget_tokens": 8000
  }
}

VALID ACTIONS (USE EXACTLY THESE NAMES):
- "delegate_respond" - send message to user (greetings, answers, final output) — NOT A TOOL, put in results
- "delegate_read_file" - read a file → maps to read_file tool
- "delegate_write_file" - create file → maps to write_file tool
- "delegate_edit_file" - edit file → maps to edit_file tool
- "delegate_multiedit" - batch edit file → maps to multiedit tool
- "delegate_shell" - run command → maps to shell tool
- "delegate_grep" - search text → maps to grep tool
- "delegate_glob" - find files → maps to glob tool
- "delegate_task" - spawn sub-agent (complex multi-step only)

❌ VIOLATION = FAILURE:
❌ Any text before { or after } — INCLUDING NEWLINES
❌ Any reasoning, thinking, analysis, explanations, planning text
❌ Any markdown, code fences, formatting
❌ Any action not in the list above
❌ USING REAL TOOL NAMES — USE DELEGATE_ PREFIX INSTEAD
❌ "Tell me about X" → YOU ANSWERING = FAIL. MUST DELEGATE.
❌ "Hello" → YOU READING FILES = FAIL. USE DELEGATE_RESPOND.
`

const SlaveSystemPrompt = `

You are the **Executor** in a master–slave execution loop.
Your role: EXECUTE the instructions from the Architect, then answer the user.

## Task
You are given an objective, some instructions describing steps to perform,
and context (including the Architect's reasoning). Carry out the steps using
the tools available to you (read_file, write_file, edit_file, multiedit,
shell, grep, glob, task), then report the result to the user in a clear,
concise, natural-language answer.

## How to behave
1. Follow the instructions in order, respecting their dependencies.
2. Use the tool described by each step's "Action" field with the "Args" as
   its arguments. "respond" means: that step is where you produce the final
   answer for the user.
3. If a step fails, report what happened and what you tried, and whether the
   work is complete.
4. Once you have the information, give the user a complete answer directly
   (this is your final response).
`

// SplitReasonLoop orchestrates the master-slave execution loop
type SplitReasonLoop struct {
	masterController *Controller
	slaveController  *Controller
	handoffs         []Handoff
	maxTurns         int
	totalBudget      int
	mu               sync.Mutex
}

const (
	defaultMaxTurns    = 10
	defaultTotalBudget = 100000 // tokens
)

// NewSplitReasonLoopFromControllers creates a loop from pre-built controllers
// This is the preferred entry point called from runSplitReasonLoop
func NewSplitReasonLoopFromControllers(masterCtrl, slaveCtrl *Controller) *SplitReasonLoop {
	return &SplitReasonLoop{
		masterController: masterCtrl,
		slaveController:  slaveCtrl,
		handoffs:         make([]Handoff, 0, defaultMaxTurns),
		maxTurns:         defaultMaxTurns,
		totalBudget:      defaultTotalBudget,
	}
}

// Run executes the splitreason loop with the given user task and returns the
// final answer text delivered by the slave once the task is complete, or an
// error if it never finished.
func (s *SplitReasonLoop) Run(ctx context.Context, userTask string) (string, error) {
	masterInput := s.buildMasterInput(userTask, nil)

	for turn := 0; turn < s.maxTurns; turn++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		handoff, err := s.runMasterTurn(ctx, masterInput)
		if err != nil {
			return "", fmt.Errorf("master turn %d failed: %w", turn, err)
		}

		if err := s.validateHandoff(handoff); err != nil {
			masterInput = s.buildMasterInput(userTask, handoff, err)
			continue
		}

		completedHandoff, err := s.runSlaveTurn(ctx, handoff)
		if err != nil {
			handoff.Outcome = HandoffFailed
			handoff.Errors = []string{err.Error()}
			s.recordHandoff(handoff)
			masterInput = s.buildMasterInput(userTask, handoff)
			continue
		}

		s.recordHandoff(completedHandoff)

		// The master verifies the work before the loop is allowed to end.
		// Only a handoff the master judges complete (outcome=success and a
		// deliverable present) terminates and yields the answer.
		done, nextInput := s.reviewHandoff(completedHandoff)
		if done {
			return findHandoffAnswer(completedHandoff), nil
		}
		masterInput = nextInput
	}

	return "", fmt.Errorf("max turns (%d) exceeded", s.maxTurns)
}

// findHandoffAnswer returns the delivered answer text from a completed handoff:
// the last result's output, then observations, then content, whichever is set.
func findHandoffAnswer(h *Handoff) string {
	if h == nil {
		return ""
	}
	if len(h.Results) > 0 {
		if out := strings.TrimSpace(h.Results[len(h.Results)-1].Output); out != "" {
			return out
		}
	}
	if out := strings.TrimSpace(h.Content); out != "" {
		return out
	}
	return strings.TrimSpace(h.Observations)
}

// lastAssistantContent returns the text content of the most recent assistant
// message in the conversation history (scanning from the end), or "" if none.
func lastAssistantContent(hist []provider.Message) string {
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Role == provider.RoleAssistant {
			t := strings.TrimSpace(hist[i].Content)
			if t != "" {
				return t
			}
		}
	}
	return ""
}

// runMasterTurn runs a single master turn and parses the handoff JSON from the
// the master's final assistant message in the conversation. It does NOT rely on
// sink interception (which cannot see the executor's emitted events); it reads
// the result directly from History() after the synchronous RunTurn returns.
func (s *SplitReasonLoop) runMasterTurn(ctx context.Context, input string) (*Handoff, error) {
	if err := s.masterController.RunTurn(ctx, input); err != nil {
		return nil, err
	}
	text := lastAssistantContent(s.masterController.History())
	if text == "" {
		return nil, fmt.Errorf("master did not produce a handoff")
	}
	if h := extractHandoffJSON(text); h != nil {
		return h, nil
	}
	return nil, fmt.Errorf("master did not emit a valid handoff JSON")
}

// runSlaveTurn executes the handoff using the slave controller. After the slave
// runs (executing its tools and answering), it reads the slave's final text
// answer from History() and wraps it into a completed handoff. No strict JSON
// is required of the slave — its natural-language answer becomes the result.
func (s *SplitReasonLoop) runSlaveTurn(ctx context.Context, handoff *Handoff) (*Handoff, error) {
	slaveInput := s.buildSlaveInput(handoff)

	runErr := s.slaveController.RunTurn(ctx, slaveInput)
	if runErr != nil && ctx.Err() == nil {
		return s.createFailedHandoff(handoff, runErr), nil
	}

	answer := lastAssistantContent(s.slaveController.History())

	completed := &Handoff{
		Objective:       handoff.Objective,
		Instructions:    handoff.Instructions,
		SuccessCriteria: handoff.SuccessCriteria,
		ContextSummary:  handoff.ContextSummary,
		AllowedTools:    handoff.AllowedTools,
		BudgetTokens:    handoff.BudgetTokens,
		Outcome:         HandoffSuccess,
	}

	if answer != "" {
		completed.Results = []InstructionResult{{
			InstructionID: "final",
			Success:       true,
			Output:        answer,
		}}
		completed.Observations = "Slave executed the handoff and delivered the answer"
	} else if ctx.Err() != nil {
		completed.Outcome = HandoffFailed
		completed.Errors = []string{ctx.Err().Error()}
	}
	return completed, nil
}

// buildMasterInput constructs the input for the master turn
func (s *SplitReasonLoop) buildMasterInput(userTask string, prevHandoff *Handoff, err ...error) string {
	var parts []string
	parts = append(parts, "## Task")
	parts = append(parts, userTask)

	if len(s.handoffs) > 0 {
		parts = append(parts, "\n## Handoff History")
		for i, h := range s.handoffs {
			parts = append(parts, fmt.Sprintf("\n### Handoff %d (outcome: %s)", i+1, h.Outcome))
			if h.Observations != "" {
				parts = append(parts, "Observations: "+h.Observations)
			}
			if len(h.Errors) > 0 {
				parts = append(parts, "Errors: "+strings.Join(h.Errors, "; "))
			}
		}
	}

	if prevHandoff != nil {
		parts = append(parts, "\n## Previous Handoff Result")
		parts = append(parts, "Outcome: "+string(prevHandoff.Outcome))
		if prevHandoff.Observations != "" {
			parts = append(parts, "Observations: "+prevHandoff.Observations)
		}
		if len(prevHandoff.Errors) > 0 {
			parts = append(parts, "Errors: "+strings.Join(prevHandoff.Errors, "; "))
		}
	}

	if len(err) > 0 && err[0] != nil {
		parts = append(parts, "\n## Parse Error")
		parts = append(parts, "Fix your JSON output: "+err[0].Error())
	}

	parts = append(parts, "\n## Instructions")
	parts = append(parts, "Return ONLY a JSON object with a \"handoff\" field containing the handoff structure. No extra text.")

	return strings.Join(parts, "\n")
}

// buildSlaveInput constructs the input for the slave turn.
// It translates the master's "delegate_*" action names to the real tool names
// the slave can actually call (read_file, edit_file, shell, etc.), so the
// slave does not try to call non-existent "delegate_*" tools.
func (s *SplitReasonLoop) buildSlaveInput(handoff *Handoff) string {
	var parts []string
	parts = append(parts, "## Objective")
	parts = append(parts, handoff.Objective)

	parts = append(parts, "\n## Master's Reasoning")
	if handoff.MasterReasoning != "" {
		parts = append(parts, handoff.MasterReasoning)
	} else {
		parts = append(parts, handoff.ContextSummary)
	}

	parts = append(parts, "\n## Context")
	parts = append(parts, handoff.ContextSummary)

	parts = append(parts, "\n## Instructions")
	for _, inst := range handoff.Instructions {
		parts = append(parts, fmt.Sprintf("\n### Step %s: %s", inst.ID, inst.Description))
		parts = append(parts, fmt.Sprintf("Action: %s", delegateToTool(inst.Action)))
		if inst.Args != "" {
			parts = append(parts, fmt.Sprintf("Args: %s", inst.Args))
		}
		if len(inst.DependsOn) > 0 {
			parts = append(parts, fmt.Sprintf("Depends on: %s", strings.Join(inst.DependsOn, ", ")))
		}
	}

	parts = append(parts, "\n## Success Criteria")
	for i, c := range handoff.SuccessCriteria {
		parts = append(parts, fmt.Sprintf("%d. %s", i+1, c))
	}

	parts = append(parts, "\n## Allowed Tools")
	parts = append(parts, strings.Join(handoff.AllowedTools, ", "))

	parts = append(parts, "\n## Budget")
	parts = append(parts, fmt.Sprintf("Token budget: %d", handoff.BudgetTokens))

	parts = append(parts, "\n## Final step")
	parts = append(parts, "After carrying out the steps above, give the user a direct, complete, natural-language answer. Do not describe the tools you used — just answer as if you examined the files yourself.")

	return strings.Join(parts, "\n")
}

// delegateToTool maps the master's "delegate_*" action names to the real tool
// names the slave can call. The master uses delegate_ prefixes so its output
// never looks like tool calls, but the slave needs the real tool names.
func delegateToTool(action string) string {
	switch action {
	case "delegate_read_file":
		return "read_file"
	case "delegate_write_file":
		return "write_file"
	case "delegate_edit_file":
		return "edit_file"
	case "delegate_multiedit":
		return "multiedit"
	case "delegate_shell":
		return "shell"
	case "delegate_grep":
		return "grep"
	case "delegate_glob":
		return "glob"
	case "delegate_task":
		return "task"
	case "delegate_respond", "respond":
		return "respond" // handled specially by the slave as its final answer
	default:
		return action
	}
}

// validateHandoff validates the handoff structure
func (s *SplitReasonLoop) validateHandoff(h *Handoff) error {
	if h.Objective == "" {
		return fmt.Errorf("missing objective")
	}
	if len(h.Instructions) == 0 {
		return fmt.Errorf("no instructions")
	}
	if len(h.Instructions) > 10 {
		return fmt.Errorf("too many instructions (max 10)")
	}
	if h.BudgetTokens <= 0 {
		return fmt.Errorf("invalid budget_tokens")
	}
	for i, inst := range h.Instructions {
		if inst.ID == "" {
			return fmt.Errorf("instruction %d: missing id", i)
		}
		if inst.Action == "" {
			return fmt.Errorf("instruction %s: missing action", inst.ID)
		}
		if inst.Description == "" {
			return fmt.Errorf("instruction %s: missing description", inst.ID)
		}
		// Validate depends_on references earlier instructions
		for _, dep := range inst.DependsOn {
			found := false
			for _, prev := range h.Instructions {
				if prev.ID == dep {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("instruction %s: depends_on references unknown step %s", inst.ID, dep)
			}
		}
	}
	return nil
}

// reviewHandoff reviews the completed handoff and decides next action
func (s *SplitReasonLoop) reviewHandoff(h *Handoff) (done bool, nextInput string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch h.Outcome {
	case HandoffSuccess:
		// Verify all success criteria met
		if s.verifySuccessCriteria(h) {
			return true, "" // Done
		}
		// Some criteria not met - continue
		return false, s.buildMasterInput("", h)

	case HandoffPartial:
		// Partial success - master analyzes and continues with remaining
		return false, s.buildMasterInput("", h)

	case HandoffFailed:
		// Failure - master diagnoses and retries
		return false, s.buildMasterInput("", h)

	case HandoffAmbiguous:
		// Ambiguity - master clarifies
		return false, s.buildMasterInput("", h)

	default:
		// Unknown outcome - treat as failed
		return false, s.buildMasterInput("", h)
	}
}

// verifySuccessCriteria reports whether a completed handoff counts as done.
// The slave sets Outcome=Success and delivers its final answer as a single
// result, so we accept it when the outcome is success and an answer (result
// output, content, or observations) was actually produced — rather than
// requiring one result per instruction.
func (s *SplitReasonLoop) verifySuccessCriteria(h *Handoff) bool {
	if h == nil {
		return false
	}
	if h.Outcome != HandoffSuccess {
		return false
	}
	// Any failed result means it isn't done.
	for _, r := range h.Results {
		if !r.Success {
			return false
		}
	}
	// Must have at least one concrete deliverable (answer, content, or observation).
	if len(h.Results) > 0 {
		for _, r := range h.Results {
			if strings.TrimSpace(r.Output) != "" {
				return true
			}
		}
	}
	return strings.TrimSpace(h.Content) != "" || strings.TrimSpace(h.Observations) != ""
}

// createFailedHandoff creates a failed handoff from an error
func (s *SplitReasonLoop) createFailedHandoff(original *Handoff, err error) *Handoff {
	return &Handoff{
		Objective:       original.Objective,
		Instructions:    original.Instructions,
		SuccessCriteria: original.SuccessCriteria,
		ContextSummary:  original.ContextSummary,
		AllowedTools:    original.AllowedTools,
		BudgetTokens:    original.BudgetTokens,
		Outcome:         HandoffFailed,
		Errors:          []string{err.Error()},
	}
}

// recordHandoff adds a handoff to history
func (s *SplitReasonLoop) recordHandoff(h *Handoff) {
	s.handoffs = append(s.handoffs, *h)
	// Keep only last N handoffs
	if len(s.handoffs) > s.maxTurns {
		s.handoffs = s.handoffs[len(s.handoffs)-s.maxTurns:]
	}
}

// capturingSink wraps a sink to capture specific events
type capturingSink struct {
	event.Sink
	onMessage func(string)
	onDone    func()
}

func (c *capturingSink) Emit(e event.Event) {
	// Capture final message with handoff JSON
	if c.onMessage != nil && e.Kind == event.Message {
		c.onMessage(e.Text)
	}
	if c.onDone != nil && e.Kind == event.TurnDone {
		c.onDone()
	}
	// Forward to original sink
	if c.Sink != nil {
		c.Sink.Emit(e)
	}
}

// Close closes both controllers
func (s *SplitReasonLoop) Close() {
	if s.masterController != nil {
		s.masterController.Close()
	}
	if s.slaveController != nil {
		s.slaveController.Close()
	}
}

// extractHandoffJSON attempts to extract a valid Handoff from JSON embedded in text.
// It looks for JSON objects containing a "handoff" field anywhere in the text.
func extractHandoffJSON(text string) *Handoff {
	// Strategy: Find all {...} patterns and try to parse each
	depth := 0
	start := -1
	var results []string

	for i, ch := range text {
		if ch == '{' {
			if depth == 0 {
				start = i
			}
			depth++
		} else if ch == '}' {
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					candidate := text[start:i+1]
					results = append(results, candidate)
				}
			}
		}
	}

	// Try to parse each candidate
	for _, candidate := range results {
		var parsed struct {
			Handoff *Handoff `json:"handoff"`
		}
		if json.Unmarshal([]byte(candidate), &parsed) == nil && parsed.Handoff != nil {
			return parsed.Handoff
		}
	}

	// Fallback: try the whole text as JSON (in case it's clean)
	var fallback struct {
		Handoff *Handoff `json:"handoff"`
	}
	if json.Unmarshal([]byte(text), &fallback) == nil && fallback.Handoff != nil {
		return fallback.Handoff
	}

	return nil
}

// extractSimpleResponse attempts to extract text content from a simple response format
// like {"handoff": {"type": "text", "content": "..."}} and return the content.
func (s *SplitReasonLoop) extractSimpleResponse(h *Handoff) string {
	// If the Handoff has Type and Content fields set (from simple response format),
	// extract the content.
	if h.Type == "text" && h.Content != "" {
		return h.Content
	}
	return ""
}

// SetMaxTurns sets the maximum number of master turns
func (s *SplitReasonLoop) SetMaxTurns(n int) {
	s.maxTurns = n
}

// SetTotalBudget sets the total token budget
func (s *SplitReasonLoop) SetTotalBudget(n int) {
	s.totalBudget = n
}