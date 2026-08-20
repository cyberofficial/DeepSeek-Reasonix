// Package control - SplitReason: master-slave execution loop
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"reasonix/internal/event"
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
	Action      string   `json:"action"`       // tool name: "read", "write", "edit", "shell", "grep", "glob", "task"
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

## OUTPUT FORMAT (strict JSON):
{
  "handoff": {
    "objective": "string - what slave must accomplish",
    "instructions": [{"id": "1", "action": "tool_name", "args": "JSON string", "description": "what slave does"}]
    "success_criteria": ["verifiable condition 1"],
    "context_summary": "why this task, what user wants",
    "master_reasoning": "string - YOUR REASONING PROCESS FOR THE SLAVE",
    "allowed_tools": ["tool1", "tool2"],
    "budget_tokens": 1000
  }
}

You MUST include "master_reasoning" with your thinking process.
The slave will receive this as context to understand your intent.
Your output MUST be valid JSON. No text outside the JSON object.

PATTERNS:

USER: "Hello" / "Hi" / "Hey"
{"handoff":{"objective":"Greet user","instructions":[{"id":"1","action":"respond","args":"{"message":"Hello! How can I help?"}","description":"Greet"}],"success_criteria":["User greeted"],"context_summary":"User greeted","master_reasoning":"Simple greeting - respond politely without file ops","allowed_tools":["respond"],"budget_tokens":500}}

USER: "Tell me about X" / "What is X" / "Explain X" / "Describe X"
{
  "handoff": {
    "objective": "Answer user question about repository",
    "instructions": [
      {"id": "1", "action": "read_file", "args": "{"path": "REASONIX.md"}", "description": "Read project memory", "depends_on": []},
      {"id": "2", "action": "read_file", "args": "{"path": "README.md"}", "description": "Read project overview", "depends_on": ["1"]},
      {"id": "3", "action": "respond", "args": "{"message": "This repository is..."}", "description": "Deliver answer", "depends_on": ["1", "2"]}
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
      {"id": "1", "action": "read_file", "args": "{"path": "path/to/file"}", "description": "Read target file", "depends_on": []},
      {"id": "2", "action": "edit_file", "args": "{"path": "path/to/file", "old_text": "...", "new_text": "..."}", "description": "Make change", "depends_on": ["1"]},
      {"id": "3", "action": "shell", "args": "{"command": "go build ./..."}", "description": "Verify build", "depends_on": ["2"]}
    ],
    "success_criteria": ["Changes compile", "Tests pass"], "context_summary": "Code change requested", "master_reasoning": "Code change requested. Slave reads file, makes edit, verifies build.", "allowed_tools": ["read_file", "edit_file", "shell"], "budget_tokens": 8000
  }
}

VALID ACTIONS: "respond" (for answers), "read_file", "write_file", "edit_file", "multiedit", "shell", "grep", "glob", "task"
❌ NO TEXT OUTSIDE JSON.
❌ NO "read" — USE "read_file". NO "edit" — USE "edit_file". NO "write" — USE "write_file".
`

const SlaveSystemPrompt = `

You are the **Executor** in a master–slave execution loop.
Your role: EXECUTE PRECISE INSTRUCTIONS from the Architect.

## ⛔ CRITICAL: YOU OUTPUT ONLY A SINGLE JSON OBJECT - NOTHING ELSE
❌ NO reasoning, NO thinking, NO analysis, NO explanations, NO text before/after
✅ YOUR ENTIRE RESPONSE = ONE JSON OBJECT EXACTLY AS SPECIFIED BELOW

## Input:
You receive a structured Handoff with:
- objective, instructions[], success_criteria[], context_summary, allowed_tools, budget_tokens
- master_reasoning (optional): The master's reasoning process and intent for this task

## Your Behavior:
1. Execute instructions IN ORDER (respect depends_on)
2. Use ONLY these tools: "read_file", "write_file", "edit_file", "multiedit", "shell", "grep", "glob", "task"
2. For "read_file"/"write_file"/"edit_file"/"multiedit"/"shell"/"grep"/"glob": MAKE TOOL CALLS
3. For "respond": THIS IS YOUR FINAL OUTPUT FORMAT - see below — NOT A TOOL, PUT IN results[].output

## YOUR FINAL OUTPUT = COMPLETED HANDOFF JSON (see format below)
⚠️ DO NOT USE "respond" AS A TOOL CALL - INSTEAD, INCLUDE THE MESSAGE IN results[].output
⚠️ YOU DO NOT CALL ANY TOOL FOR THE FINAL "respond" - PUT THE MESSAGE IN THE JSON

## YOUR FINAL OUTPUT MUST BE EXACTLY THIS JSON STRUCTURE:
{
  "handoff": {
    "objective": "same as input objective",
    "instructions": [
      {"id": "1", "action": "read_file", "args": "{\"path\": \"REASONIX.md\"}", "description": "..."}
    ],
    "success_criteria": ["Files read", "Answer delivered"],
    "context_summary": "User asked about repo, I read files and compiled answer",
    "allowed_tools": ["read_file", "respond"],
    "budget_tokens": 2000,
    "results": [
      {"instruction_id": "1", "success": true, "output": "file content here", "error": "", "duration_ms": 50},
      {"instruction_id": "2", "success": true, "output": "file content here", "error": "", "duration_ms": 30},
      {"instruction_id": "3", "success": true, "output": "Here is the repo overview...", "error": "", "duration_ms": 10}
    ],
    "outcome": "success",
    "observations": "Read REASONIX.md and README.md, compiled answer",
    "errors": []
  }
}

## ⛔ VIOLATION = IMMEDIATE FAILURE:
❌ ANY text before { or after }
❌ Any reasoning, thinking, analysis, explanation, planning text
❌ Any markdown, code fences, formatting
❌ "respond" as a tool call - it goes in the JSON "results" field
❌ For questions/info → YOU MUST include answer in results[].output, not as tool call
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

// Run executes the splitreason loop with the given user task
func (s *SplitReasonLoop) Run(ctx context.Context, userTask string) error {
	// Initialize master with user task
	masterInput := s.buildMasterInput(userTask, nil)

	for turn := 0; turn < s.maxTurns; turn++ {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// === MASTER: Generate handoff ===
		handoff, err := s.runMasterTurn(ctx, masterInput)
		if err != nil {
			return fmt.Errorf("master turn %d failed: %w", turn, err)
		}

		// Validate handoff
		if err := s.validateHandoff(handoff); err != nil {
			// Send correction to master
			masterInput = s.buildMasterInput(userTask, handoff, err)
			continue
		}

		// === SLAVE: Execute handoff ===
		completedHandoff, err := s.runSlaveTurn(ctx, handoff)
		if err != nil {
			// Slave infrastructure error - report to master
			handoff.Outcome = HandoffFailed
			handoff.Errors = []string{err.Error()}
			s.recordHandoff(handoff)
			masterInput = s.buildMasterInput(userTask, handoff)
			continue
		}

		// Record completed handoff
		s.recordHandoff(completedHandoff)

		// === MASTER: Review outcome ===
		done, nextInput := s.reviewHandoff(completedHandoff)
		if done {
			return nil
		}
		masterInput = nextInput
	}

	return fmt.Errorf("max turns (%d) exceeded", s.maxTurns)
}

// runMasterTurn runs a single master turn and parses the handoff JSON
func (s *SplitReasonLoop) runMasterTurn(ctx context.Context, input string) (*Handoff, error) {
	// The master controller's sink will receive events
	// We need to capture the final message and parse the handoff
	doneCh := make(chan struct{})
	var handoff *Handoff
	var firstErr error

	// Wrap sink to capture the final message
	origSink := s.masterController.Sink()
	s.masterController.SetSink(&capturingSink{
		Sink: origSink,
		onMessage: func(msg string) {
			// Try to extract handoff JSON from the message
			if extracted := extractHandoffJSON(msg); extracted != nil {
				handoff = extracted
			}
		},
		onDone: func() {
			close(doneCh)
		},
	})

	// Run master turn
	if err := s.masterController.RunTurn(ctx, input); err != nil {
		return nil, err
	}

	// Wait for completion
	<-doneCh

	// Restore original sink
	s.masterController.SetSink(origSink)

	if handoff == nil {
		return nil, fmt.Errorf("master did not emit valid handoff JSON")
	}

	return handoff, firstErr
}

// runSlaveTurn executes the handoff using the slave controller
func (s *SplitReasonLoop) runSlaveTurn(ctx context.Context, handoff *Handoff) (*Handoff, error) {
	// Convert handoff to slave input
	slaveInput := s.buildSlaveInput(handoff)

	// Wrap sink to capture the slave's response
	doneCh := make(chan struct{})
	var completedHandoff *Handoff

	origSink := s.slaveController.Sink()
	s.slaveController.SetSink(&capturingSink{
		Sink: origSink,
		onMessage: func(msg string) {
			if extracted := extractHandoffJSON(msg); extracted != nil {
				completedHandoff = extracted
			}
		},
		onDone: func() {
			close(doneCh)
		},
	})

	// Run slave turn
	if err := s.slaveController.RunTurn(ctx, slaveInput); err != nil {
		s.slaveController.SetSink(origSink)
		return s.createFailedHandoff(handoff, err), nil
	}

	// Wait for completion
	<-doneCh

	// Restore original sink
	s.slaveController.SetSink(origSink)

	if completedHandoff == nil {
		// If slave didn't output valid JSON, create a failed handoff
		return s.createFailedHandoff(handoff, fmt.Errorf("slave did not emit valid handoff JSON")), nil
	}

	// Ensure the completed handoff has the original objective and instructions
	completedHandoff.Objective = handoff.Objective
	completedHandoff.Instructions = handoff.Instructions
	completedHandoff.SuccessCriteria = handoff.SuccessCriteria
	completedHandoff.ContextSummary = handoff.ContextSummary
	completedHandoff.AllowedTools = handoff.AllowedTools
	completedHandoff.BudgetTokens = handoff.BudgetTokens

	// If the slave returned a simple response format (type + content) instead of the full
	// handoff structure, convert it to the proper format with outcome and results
	if completedHandoff.Outcome == "" && len(completedHandoff.Results) == 0 {
		// Try to extract content from simple format: {"type": "text", "content": "..."}
		if content := s.extractSimpleResponse(completedHandoff); content != "" {
			completedHandoff.Outcome = HandoffSuccess
			completedHandoff.Results = []InstructionResult{
				{
					InstructionID: "final",
					Success:       true,
					Output:        content,
					Error:         "",
					DurationMs:    0,
				},
			}
			completedHandoff.Observations = "Converted simple text response to handoff format"
			completedHandoff.Errors = nil
		}
	}

	return completedHandoff, nil
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

// buildSlaveInput constructs the input for the slave turn
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

	parts = append(parts, "\n## Instructions")
	for _, inst := range handoff.Instructions {
		parts = append(parts, fmt.Sprintf("\n### Step %s: %s", inst.ID, inst.Description))
		parts = append(parts, fmt.Sprintf("Action: %s", inst.Action))
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

	parts = append(parts, "\n## Output Format (STRICT - ONLY JSON):")
	parts = append(parts, "Return ONLY a valid JSON object with the completed handoff structure:")
	parts = append(parts, "{")
	parts = append(parts, `  "handoff": {`)
	parts = append(parts, `    "objective": "...",`)
	parts = append(parts, `    "instructions": [...],`)
	parts = append(parts, `    "success_criteria": [...],`)
	parts = append(parts, `    "context_summary": "...",`)
	parts = append(parts, `    "allowed_tools": [...],`)
	parts = append(parts, `    "budget_tokens": 1000,`)
	parts = append(parts, `    "results": [{"instruction_id": "1", "success": true, "output": "result", "error": "", "duration_ms": 100}],`)
	parts = append(parts, `    "outcome": "success|partial|failed|ambiguous",`)
	parts = append(parts, `    "observations": "what happened",`)
	parts = append(parts, `    "errors": []`)
	parts = append(parts, `  }`)
	parts = append(parts, `}`)
	parts = append(parts, "")
	parts = append(parts, "CRITICAL: Output ONLY the JSON. No reasoning, no explanations, no extra text!")

	return strings.Join(parts, "\n")
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

// verifySuccessCriteria checks if all success criteria are met based on results
func (s *SplitReasonLoop) verifySuccessCriteria(h *Handoff) bool {
	// Simple check: all instructions succeeded
	for _, r := range h.Results {
		if !r.Success {
			return false
		}
	}
	return len(h.Results) == len(h.Instructions)
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
					candidate := text[start : i+1]
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