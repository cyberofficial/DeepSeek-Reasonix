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

	// Context gathering phase (Phase 1) - for accumulating reasoning before planning
	Context *MasterContext `json:"context,omitempty"`

	// Slave simple response format (for backward compatibility)
	Type    string `json:"type,omitempty"`
	Content string `json:"content,omitempty"`

	// Slave → Master (response) — legacy fields
	Results     []InstructionResult `json:"results,omitempty"`
	Outcome     HandoffOutcome      `json:"outcome,omitempty"`
	Observations string            `json:"observations,omitempty"`
	Errors      []string            `json:"errors,omitempty"`

	// Slave → Master (response) — structured work report (new)
	WorkReport *WorkReport `json:"work_report,omitempty"`

	// Full handoff (for planning phase) - used when Context is nil but Handoff is present
	Handoff *Handoff `json:"handoff,omitempty"`
}

// MasterContext represents the context gathering phase output
type MasterContext struct {
	ReasoningAccumulated string   `json:"reasoning_accumulated,omitempty"`
	FilesExamined        []string `json:"files_examined,omitempty"`
	CommandsRun          []string `json:"commands_run,omitempty"`
	KeyFindings          string   `json:"key_findings,omitempty"`
	ReadyToPlan          bool      `json:"ready_to_plan"`
}

// WorkReport is the slave's structured completion report to the master.
type WorkReport struct {
	FilesRead     []string `json:"files_read,omitempty"`      // paths read
	FilesWritten  []string `json:"files_written,omitempty"`   // paths created
	FilesEdited   []string `json:"files_edited,omitempty"`    // paths modified
	CommandsRun   []string `json:"commands_run,omitempty"`    // shell commands executed
	Summary       string   `json:"summary"`                   // brief narrative of what was done
	Answer        string   `json:"answer,omitempty"`          // the user-facing answer (only if final)
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
	HandoffSuccess   HandoffOutcome = "success"   // work complete, awaiting master review
	HandoffPartial   HandoffOutcome = "partial"   // some steps done, more needed
	HandoffFailed    HandoffOutcome = "failed"    // error occurred
	HandoffAmbiguous HandoffOutcome = "ambiguous" // needs clarification
)

// Master and Slave system prompts
// ================================

const MasterSystemPrompt = `# MASTER SYSTEM PROMPT (SPLITREASON ARCHITECT)

You are the **Architect** in a master-slave execution loop.
Your role: CONTEXT GATHERING -> HIGH-LEVEL PLANNING -> VERIFICATION.

## CRITICAL OUTPUT RULES - VIOLATION = FAILURE:
- NO TEXT BEFORE { OR AFTER } - NOT EVEN A NEWLINE
- NO MARKDOWN, CODE FENCES, FORMATTING
- YOUR ENTIRE RESPONSE = ONE JSON OBJECT STARTING WITH { AND ENDING WITH }
- STOP IMMEDIATELY AFTER THE FINAL } - NO NEWLINE, NO SPACE, NOTHING

## THREE PHASES (determined by the user message you receive):

### PHASE 1: CONTEXT GATHERING (exploration)
When you need to understand the codebase before planning, YOU HAVE FULL TOOL ACCESS.
Use read_file, shell, grep, glob, task, MCP tools to explore the codebase.
After gathering context, output a "context" handoff with accumulated reasoning.

OUTPUT FORMAT (strict JSON):
{
  "context": {
    "reasoning_accumulated": "string - ALL your thinking so far, accumulated across turns",
    "files_examined": ["paths you read"],
    "commands_run": ["shell commands you ran"],
    "key_findings": "string - summary of what you learned",
    "ready_to_plan": false  // set true when context gathering is done
  }
}

### PHASE 2: PLANNING (when ready_to_plan=true or user message says "## Plan for Slave")
NOW produce the handoff for the slave. Include ALL accumulated reasoning as master_reasoning.
YOU HAVE FULL TOOL ACCESS during planning too if needed.

OUTPUT FORMAT (strict JSON):
{
  "handoff": {
    "objective": "string - what slave must accomplish",
    "instructions": [{"id": "1", "action": "tool_name", "args": "JSON string", "description": "what slave does"}],
    "success_criteria": ["verifiable condition 1"],
    "context_summary": "why this task, what user wants",
    "master_reasoning": "string - YOUR FULL ACCUMULATED REASONING (from all prior turns) FOR THE SLAVE",
    "allowed_tools": ["tool1", "tool2"],
    "budget_tokens": 1000
  }
}

### PHASE 3: REVIEW (when user message says "## Your Verification Tools")
You are reviewing the slave's completed work. You have FULL TOOL ACCESS.
Use read_file, shell, grep, glob, task, MCP tools to VERIFY the slave's work.
Check: re-read files, run tests, git diff, etc. DO NOT TRUST the report blindly.

OUTPUT FORMAT (strict JSON) - return ONE of these two shapes:
- Work done: {"done": true, "notes": "<what you verified against each criterion>"}
- Work incomplete/flawed: {"done": false, "handoff": {<a fresh handoff with
  corrective or follow-up instructions, including "master_reasoning">}}

Never trust the slave's self-reported outcome; decide from the report AND the
task AND your verification. No text outside the JSON object. STOP AFTER }.

VALID ACTIONS FOR PHASE 1 & 2 (USE EXACTLY THESE NAMES):
- "delegate_read_file" - read a file -> maps to read_file tool
- "delegate_write_file" - create file -> maps to write_file tool
- "delegate_edit_file" - edit file -> maps to edit_file tool
- "delegate_multiedit" - batch edit file -> maps to multiedit tool
- "delegate_shell" - run command -> maps to shell tool
- "delegate_grep" - search text -> maps to grep tool
- "delegate_glob" - find files -> maps to glob tool
- "delegate_task" - spawn sub-agent (complex multi-step only)
- "delegate_respond" - only in planning: send message to user (greetings, answers, final output) -- NOT A TOOL, put in handoff results

VIOLATION = FAILURE:
- Any text before { or after } -- INCLUDING NEWLINES
- Any markdown, code fences, formatting
- Any action not in the list above
- USING REAL TOOL NAMES -- USE DELEGATE_ PREFIX INSTEAD
- "Tell me about X" in planning -> YOU ANSWERING = FAIL. MUST DELEGATE via handoff.
- "Hello" in planning -> YOU READING FILES = FAIL. USE PHASE 1 CONTEXT GATHERING.
`

const SlaveSystemPrompt = `
You are the **Executor** in a master-slave execution loop.
Your role: EXECUTE the instructions from the Architect by CALLING TOOLS, then produce a structured WORK REPORT for the master to verify.

## CRITICAL EXECUTION RULE -- VIOLATION = FAILURE:
❌ DO NOT output the WorkReport JSON until YOU HAVE CALLED ALL REQUIRED TOOLS
✅ YOU MUST CALL TOOLS (read_file, write_file, shell, etc.) FOR EACH INSTRUCTION
✅ ONLY AFTER ALL TOOL CALLS COMPLETE, output the WorkReport JSON

## Task
You are given an objective, some instructions describing steps to perform,
and context (including the Architect's reasoning). For EACH instruction:
1. READ the instruction's "Action" and "Args"
2. CALL the corresponding tool with those Args
3. RECORD the result (file content, command output, error)
4. REPEAT for all instructions
5. ONLY THEN output the WorkReport JSON with ALL results

## Tools Available
- delegate_read_file -> call read_file tool
- delegate_write_file -> call write_file tool
- delegate_edit_file -> call edit_file tool
- delegate_multiedit -> call multiedit tool
- delegate_shell -> call shell tool
- delegate_grep -> call grep tool
- delegate_glob -> call glob tool
- delegate_task -> call task tool
- delegate_respond -> NOT A TOOL; put final answer in WorkReport.answer

## Output Format (STRICT -- only JSON, no extra text):
{
  "work_report": {
    "files_read": ["path1", "path2"],
    "files_written": ["path3"],
    "files_edited": ["path4"],
    "commands_run": ["cmd1", "cmd2"],
    "summary": "Brief narrative of what was accomplished",
    "answer": "Final user-facing answer (if this completes the task)"
  }
}

## How to behave
1. Follow the instructions in order, respecting their dependencies.
2. For EACH instruction, CALL THE TOOL described by its "Action" with the "Args".
3. Track every file you read, write, edit, and every shell command you run.
4. If a step fails, include the error in your work_report summary.
5. AFTER ALL INSTRUCTIONS ARE EXECUTED, output ONLY the WorkReport JSON.
6. YOUR FINAL OUTPUT MUST BE ONLY THE JSON OBJECT. NO TEXT OUTSIDE IT.
`

type MasterPhase int

const (
	PhaseContextGathering MasterPhase = iota
	PhasePlanning
	PhaseReview
)

// SplitReasonLoop orchestrates the master-slave execution loop
type SplitReasonLoop struct {
	masterController      *Controller
	slaveController       *Controller
	handoffs              []Handoff
	maxTurns              int
	totalBudget           int
	mu                    sync.Mutex
	accumulatedReasoning  string
	phase                 MasterPhase
	contextFilesExamined  []string
	contextCommandsRun    []string
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
	s.phase = PhaseContextGathering
	masterInput := s.buildMasterInput(userTask, nil)

	for turn := 0; turn < s.maxTurns; turn++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		handoff, err := s.runMasterTurn(ctx, masterInput, turn)
		if err != nil {
			return "", fmt.Errorf("master turn %d failed: %w", turn, err)
		}

		// Handle context handoff (Phase 1)
		if handoff.Context != nil {
			s.accumulateReasoning(handoff)
			// Check if master is ready to plan
			if handoff.Context.ReadyToPlan {
				s.phase = PhasePlanning
				masterInput = s.buildMasterInput(userTask, handoff)
			} else {
				// Continue gathering context
				masterInput = s.buildMasterInput(userTask, handoff)
			}
			continue
		}

		// Handle planning handoff (Phase 2)
		if handoff.Handoff != nil {
			if err := s.validateHandoff(handoff.Handoff); err != nil {
				masterInput = s.buildMasterInput(userTask, handoff.Handoff, err)
				continue
			}

			s.accumulateReasoning(handoff.Handoff)
			s.recordHandoff(*handoff.Handoff)

			completedHandoff, err := s.runSlaveTurn(ctx, handoff.Handoff)
			if err != nil {
				handoff.Handoff.Outcome = HandoffFailed
				handoff.Handoff.Errors = []string{err.Error()}
				s.recordHandoff(*handoff.Handoff)
				masterInput = s.buildMasterInput(userTask, handoff.Handoff)
				continue
			}

			s.phase = PhaseReview
			s.recordHandoff(completedHandoff)

			// The master must VERIFY the slave's work before the loop is allowed to
			// end. It takes a real review turn over the slave's report.
			done, nextInput, err := s.runMasterReview(ctx, userTask, completedHandoff)
			if err != nil {
				return "", fmt.Errorf("master review turn %d failed: %w", turn, err)
			}
			if done {
				return findHandoffAnswer(completedHandoff), nil
			}
			masterInput = nextInput
			continue
		}

		// Handle direct context/handoff from validation errors etc.
		if err := s.validateHandoff(handoff); err != nil {
			masterInput = s.buildMasterInput(userTask, handoff, err)
			continue
		}

		masterInput = s.buildMasterInput(userTask, handoff)
	}

	return "", fmt.Errorf("max turns (%d) exceeded", s.maxTurns)
}

// accumulateReasoning adds the master's reasoning to the accumulated context
func (s *SplitReasonLoop) accumulateReasoning(h *Handoff) {
	if h.MasterReasoning != "" {
		if s.accumulatedReasoning != "" {
			s.accumulatedReasoning += "\n\n---\n\n" + h.MasterReasoning
		} else {
			s.accumulatedReasoning = h.MasterReasoning
		}
	}
	if h.Context != nil {
		if h.Context.ReasoningAccumulated != "" {
			if s.accumulatedReasoning != "" {
				s.accumulatedReasoning += "\n\n---\n\n" + h.Context.ReasoningAccumulated
			} else {
				s.accumulatedReasoning = h.Context.ReasoningAccumulated
			}
		}
		for _, f := range h.Context.FilesExamined {
			s.contextFilesExamined = append(s.contextFilesExamined, f)
		}
		for _, c := range h.Context.CommandsRun {
			s.contextCommandsRun = append(s.contextCommandsRun, c)
		}
	}
}

// findHandoffAnswer returns the delivered answer text from a completed handoff:
// prefer WorkReport.answer, then fall back to legacy fields.
func findHandoffAnswer(h *Handoff) string {
	if h == nil {
		return ""
	}
	if h.WorkReport != nil && strings.TrimSpace(h.WorkReport.Answer) != "" {
		return strings.TrimSpace(h.WorkReport.Answer)
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

// lastAssistantReasoning returns the thinking-block (ReasoningContent) of the
// most recent assistant message, scanning from the end. The master's real
// deliberation lives here — ahead of the short master_reasoning summary it
// writes into the handoff — and is what the slave should receive as its diluted
// context so it understands the why behind each instruction.
func lastAssistantReasoning(hist []provider.Message) string {
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Role == provider.RoleAssistant {
			r := strings.TrimSpace(hist[i].ReasoningContent)
			if r != "" {
				return r
			}
		}
	}
	return ""
}

// runMasterTurn runs a single master turn and parses the handoff/context JSON
// from the master's final assistant message. It does NOT rely on sink
// interception; it reads the result directly from History() after RunTurn.
func (s *SplitReasonLoop) runMasterTurn(ctx context.Context, input string, turn int) (*Handoff, error) {
	if err := s.masterController.RunTurn(ctx, input); err != nil {
		return nil, err
	}
	hist := s.masterController.History()
	text := lastAssistantContent(hist)
	if text == "" {
		return nil, fmt.Errorf("master did not produce output")
	}
	h := extractHandoffOrContextJSON(text)
	if h == nil {
		return nil, fmt.Errorf("master did not emit valid JSON")
	}
	// Capture reasoning for accumulation
	if r := lastAssistantReasoning(hist); r != "" {
		h.MasterReasoning = r
	}
	return h, nil
}

// extractHandoffOrContextJSON parses either a {handoff: {...}} or {context: {...}}
// JSON object from the master's output.
func extractHandoffOrContextJSON(text string) *Handoff {
	// Look for top-level JSON objects with either "handoff" or "context" keys
	depth := 0
	start := -1
	var candidates []string
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
					candidates = append(candidates, text[start:i+1])
				}
			}
		}
	}
	for _, c := range candidates {
		var parsed struct {
			Context *MasterContext `json:"context"`
			Handoff *Handoff       `json:"handoff"`
		}
		if json.Unmarshal([]byte(c), &parsed) != nil {
			continue
		}
		if parsed.Context != nil {
			return &Handoff{Context: parsed.Context}
		}
		if parsed.Handoff != nil {
			return &Handoff{Handoff: parsed.Handoff}
		}
	}
	return nil
}

// runSlaveTurn executes the handoff using the slave controller. After the slave
// runs (executing its tools), it outputs a structured WorkReport JSON. This
// function parses that report and wraps it into a completed handoff.
func (s *SplitReasonLoop) runSlaveTurn(ctx context.Context, handoff *Handoff) (*Handoff, error) {
	slaveInput := s.buildSlaveInput(handoff)

	runErr := s.slaveController.RunTurn(ctx, slaveInput)
	if runErr != nil && ctx.Err() == nil {
		return s.createFailedHandoff(handoff, runErr), nil
	}

	hist := s.slaveController.History()
	text := lastAssistantContent(hist)
	if text == "" {
		return s.createFailedHandoff(handoff, fmt.Errorf("slave produced no output")), nil
	}

	workReport := s.extractWorkReport(text)
	if workReport == nil {
		// Fallback: wrap the raw text as a basic work report with answer
		workReport = &WorkReport{
			Summary: "Slave completed work (raw output captured)",
			Answer:  text,
		}
	}

	completed := &Handoff{
		Objective:       handoff.Objective,
		Instructions:    handoff.Instructions,
		SuccessCriteria: handoff.SuccessCriteria,
		ContextSummary:  handoff.ContextSummary,
		AllowedTools:    handoff.AllowedTools,
		BudgetTokens:    handoff.BudgetTokens,
		Outcome:         HandoffSuccess,
		WorkReport:      workReport,
	}
	return completed, nil
}

// extractWorkReport parses the slave's output JSON to extract the work_report.
func (s *SplitReasonLoop) extractWorkReport(text string) *WorkReport {
	// Find JSON with "work_report" field
	depth := 0
	start := -1
	var candidates []string
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
					candidates = append(candidates, text[start:i+1])
				}
			}
		}
	}
	for _, c := range candidates {
		var parsed struct {
			WorkReport *WorkReport `json:"work_report"`
		}
		if json.Unmarshal([]byte(c), &parsed) == nil && parsed.WorkReport != nil {
			return parsed.WorkReport
		}
	}
	return nil
}

// buildMasterInput constructs the input for the master turn based on current phase
func (s *SplitReasonLoop) buildMasterInput(userTask string, prevHandoff *Handoff, err ...error) string {
	var parts []string
	parts = append(parts, "## Task")
	parts = append(parts, userTask)

	// Always include accumulated reasoning as context for the master
	if s.accumulatedReasoning != "" {
		parts = append(parts, "\n## Accumulated Reasoning (from all prior turns)")
		parts = append(parts, s.accumulatedReasoning)
	}

	// Include context gathered so far
	if len(s.contextFilesExamined) > 0 {
		parts = append(parts, "\n## Files Examined So Far")
		parts = append(parts, strings.Join(s.contextFilesExamined, ", "))
	}
	if len(s.contextCommandsRun) > 0 {
		parts = append(parts, "\n## Commands Run So Far")
		parts = append(parts, strings.Join(s.contextCommandsRun, ", "))
	}

	if len(s.handoffs) > 0 {
		parts = append(parts, "\n## Handoff History")
		for i, h := range s.handoffs {
			parts = append(parts, fmt.Sprintf("\n### Handoff %d (outcome: %s)", i+1, h.Outcome))
			if h.WorkReport != nil {
				wr := h.WorkReport
				if len(wr.FilesRead) > 0 {
					parts = append(parts, "Files Read: "+strings.Join(wr.FilesRead, ", "))
				}
				if len(wr.FilesWritten) > 0 {
					parts = append(parts, "Files Written: "+strings.Join(wr.FilesWritten, ", "))
				}
				if len(wr.FilesEdited) > 0 {
					parts = append(parts, "Files Edited: "+strings.Join(wr.FilesEdited, ", "))
				}
				if len(wr.CommandsRun) > 0 {
					parts = append(parts, "Commands Run: "+strings.Join(wr.CommandsRun, ", "))
				}
				if wr.Summary != "" {
					parts = append(parts, "Summary: "+wr.Summary)
				}
				if wr.Answer != "" {
					parts = append(parts, "User Answer: "+wr.Answer)
				}
			} else if h.Observations != "" {
				parts = append(parts, "Observations: "+h.Observations)
			}
			if len(h.Errors) > 0 {
				parts = append(parts, "Errors: "+strings.Join(h.Errors, "; "))
			}
		}
	}

	if prevHandoff != nil {
		parts = append(parts, "\n## Previous Handoff Result")
		if prevHandoff.Context != nil {
			ctx := prevHandoff.Context
			parts = append(parts, "Context Gathering Phase Complete")
			if ctx.ReadyToPlan {
				parts = append(parts, "Status: READY TO PLAN")
			} else {
				parts = append(parts, "Status: STILL GATHERING CONTEXT")
			}
			if ctx.ReasoningAccumulated != "" {
				parts = append(parts, "Reasoning from this turn: "+ctx.ReasoningAccumulated)
			}
			if ctx.KeyFindings != "" {
				parts = append(parts, "Key Findings: "+ctx.KeyFindings)
			}
			if len(ctx.FilesExamined) > 0 {
				parts = append(parts, "Files Examined This Turn: "+strings.Join(ctx.FilesExamined, ", "))
			}
			if len(ctx.CommandsRun) > 0 {
				parts = append(parts, "Commands Run This Turn: "+strings.Join(ctx.CommandsRun, ", "))
			}
		} else if prevHandoff.Handoff != nil {
			parts = append(parts, "Planning Phase - Handoff Produced")
		} else if prevHandoff.WorkReport != nil {
			wr := prevHandoff.WorkReport
			parts = append(parts, "Slave execution completed")
			if len(wr.FilesRead) > 0 {
				parts = append(parts, "Files Read: "+strings.Join(wr.FilesRead, ", "))
			}
			if len(wr.FilesWritten) > 0 {
				parts = append(parts, "Files Written: "+strings.Join(wr.FilesWritten, ", "))
			}
			if len(wr.FilesEdited) > 0 {
				parts = append(parts, "Files Edited: "+strings.Join(wr.FilesEdited, ", "))
			}
			if len(wr.CommandsRun) > 0 {
				parts = append(parts, "Commands Run: "+strings.Join(wr.CommandsRun, ", "))
			}
			if wr.Summary != "" {
				parts = append(parts, "Summary: "+wr.Summary)
			}
			if wr.Answer != "" {
				parts = append(parts, "Proposed Answer: "+wr.Answer)
			}
		} else if prevHandoff.Observations != "" {
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

	// Phase-specific instructions
	parts = append(parts, "\n## Instructions")
	switch s.phase {
	case PhaseContextGathering:
		parts = append(parts, "You are in CONTEXT GATHERING PHASE. Explore the codebase using tools (read_file, shell, grep, glob, task, MCP) to understand the task.")
		parts = append(parts, "Output ONLY a JSON object with a \"context\" field containing your accumulated reasoning.")
		parts = append(parts, "Set \"ready_to_plan\": true when you have enough context to create a plan for the slave.")
		parts = append(parts, "Format: {\"context\": {\"reasoning_accumulated\": \"...\", \"files_examined\": [...], \"commands_run\": [...], \"key_findings\": \"...\", \"ready_to_plan\": true/false}}")
		parts = append(parts, "NO TEXT OUTSIDE THE JSON OBJECT. STOP AFTER THE FINAL }.")
	case PhasePlanning:
		parts = append(parts, "You are in PLANNING PHASE. Create a handoff for the slave using ALL accumulated reasoning above.")
		parts = append(parts, "Output ONLY a JSON object with a \"handoff\" field containing the structured handoff.")
		parts = append(parts, "Include ALL accumulated reasoning in the \"master_reasoning\" field so the slave understands the full context.")
		parts = append(parts, "Format: {\"handoff\": {\"objective\": \"...\", \"instructions\": [...], \"success_criteria\": [...], \"context_summary\": \"...\", \"master_reasoning\": \"ALL ACCUMULATED REASONING\", \"allowed_tools\": [...], \"budget_tokens\": 1000}}")
		parts = append(parts, "NO TEXT OUTSIDE THE JSON OBJECT. STOP AFTER THE FINAL }.")
	case PhaseReview:
		parts = append(parts, "You are in REVIEW PHASE. The slave has completed execution. Verify the work using tools.")
		parts = append(parts, "Return ONLY a JSON object with ONE of these two shapes:")
		parts = append(parts, `ACCEPT (work is complete): {"done": true, "notes": "<what you verified against each criterion>"}`)
		parts = append(parts, `REJECT (work needs fixes): {"done": false, "handoff": {<a fresh handoff with corrective/follow-up instructions, including "master_reasoning">}}`)
		parts = append(parts, "No text outside the JSON object. STOP AFTER THE FINAL }.")
	}

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
		if delegateToTool(inst.Action) == "respond" {
			// "respond" is a marker for the slave's final natural-language
			// answer, not a real tool. Render it as such so the slave never
			// tries to call a nonexistent "respond" tool.
			parts = append(parts, `This step is where you deliver the final answer. Do NOT call any tool. Compose a direct, complete answer to the user based on what you did above; that answer becomes the result.`)
			if inst.Args != "" {
				if msg := extractRespondMessage(inst.Args); msg != "" {
					parts = append(parts, fmt.Sprintf("Suggested message: %s", msg))
				}
			}
			continue
		}
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

	parts = append(parts, "\n## Output Format (REQUIRED -- output ONLY this JSON):")
	parts = append(parts, "{")
	parts = append(parts, "  \"work_report\": {")
	parts = append(parts, "    \"files_read\": [\"paths you read\"],")
	parts = append(parts, "    \"files_written\": [\"paths you created\"],")
	parts = append(parts, "    \"files_edited\": [\"paths you modified\"],")
	parts = append(parts, "    \"commands_run\": [\"shell commands you ran\"],")
	parts = append(parts, "    \"summary\": \"brief narrative of what was accomplished\",")
	parts = append(parts, "    \"answer\": \"final user-facing answer if task is complete\"")
	parts = append(parts, "  }")
	parts = append(parts, "}")
	parts = append(parts, "NO TEXT OUTSIDE THE JSON OBJECT. STOP AFTER THE FINAL }.")

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

// extractRespondMessage pulls the optional {"message": ...} out of a respond
// step's args JSON, so the master's suggested wording can be shown to the slave
// without ever being mistaken for a tool call. Returns "" if absent/unparseable.
func extractRespondMessage(args string) string {
	if strings.TrimSpace(args) == "" {
		return ""
	}
	var v struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(args), &v) != nil || strings.TrimSpace(v.Message) == "" {
		return ""
	}
	return v.Message
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

// reviewInput builds the master's review-turn prompt over a completed handoff.
// It lays out the task, the success criteria the work owed, and the slave's
// structured WorkReport (files read/written/edited, commands run, summary, answer).
// The master can now USE TOOLS (read_file, shell, grep, glob, task, MCP tools) to
// VERIFY the work before deciding. It returns a verdict JSON or a follow-up handoff.
func (s *SplitReasonLoop) reviewInput(userTask string, completed *Handoff) string {
	var parts []string
	parts = append(parts, "## Task")
	parts = append(parts, userTask)

	if completed != nil {
		if completed.Objective != "" {
			parts = append(parts, "\n## Objective")
			parts = append(parts, completed.Objective)
		}
		if len(completed.SuccessCriteria) > 0 {
			parts = append(parts, "\n## Success Criteria")
			for i, c := range completed.SuccessCriteria {
				parts = append(parts, fmt.Sprintf("%d. %s", i+1, c))
			}
		}
		parts = append(parts, "\n## Slave's Work Report")
		if completed.WorkReport != nil {
			wr := completed.WorkReport
			if len(wr.FilesRead) > 0 {
				parts = append(parts, "Files Read: "+strings.Join(wr.FilesRead, ", "))
			}
			if len(wr.FilesWritten) > 0 {
				parts = append(parts, "Files Written: "+strings.Join(wr.FilesWritten, ", "))
			}
			if len(wr.FilesEdited) > 0 {
				parts = append(parts, "Files Edited: "+strings.Join(wr.FilesEdited, ", "))
			}
			if len(wr.CommandsRun) > 0 {
				parts = append(parts, "Commands Run: "+strings.Join(wr.CommandsRun, ", "))
			}
			if wr.Summary != "" {
				parts = append(parts, "Summary: "+wr.Summary)
			}
			if wr.Answer != "" {
				parts = append(parts, "Proposed User Answer: "+wr.Answer)
			}
		} else if completed.Observations != "" {
			parts = append(parts, "Observations: "+completed.Observations)
		}
		if len(completed.Errors) > 0 {
			parts = append(parts, "Errors: "+strings.Join(completed.Errors, "; "))
		}
	}

	parts = append(parts, "\n## Your Verification Tools")
	parts = append(parts, "You have FULL TOOL ACCESS (read_file, write_file, edit_file, shell, grep, glob, task, MCP tools).")
	parts = append(parts, "USE THEM to verify the slave's work: re-read files, run commands, check git diff, etc.")
	parts = append(parts, "Do NOT trust the slave's report blindly -- VERIFY.")

	parts = append(parts, "\n## Instructions")
	parts = append(parts, "You are reviewing work your slave just performed. Judge it against the success criteria above.")
	parts = append(parts, "Return ONLY a JSON object with ONE of these two shapes:")
	parts = append(parts, `ACCEPT (work is complete): {"done": true, "notes": "<what you verified against each criterion>"}`)
	parts = append(parts, `REJECT (work needs fixes): {"done": false, "handoff": {<a fresh handoff with corrective/follow-up instructions, including "master_reasoning">}}`)
	parts = append(parts, "No text outside the JSON object. STOP AFTER THE FINAL }.")

	return strings.Join(parts, "\n")
}

// runMasterReview runs a real master review turn over the slave's completed
// work. It parses the master's verdict: done (accept) or a follow-up handoff
// (corrective work). This is what actually verifies the slave's output before
// the loop ends — the previous code trusted the slave's Outcome=Success label,
// which is why the host flagged the mutation as unverified/unreviewed.
func (s *SplitReasonLoop) runMasterReview(ctx context.Context, userTask string, completed *Handoff) (done bool, nextInput string, err error) {
	reviewIn := s.reviewInput(userTask, completed)

	if err := s.masterController.RunTurn(ctx, reviewIn); err != nil {
		return false, "", err
	}
	hist := s.masterController.History()
	text := lastAssistantContent(hist)
	if text == "" {
		return false, "", fmt.Errorf("master review produced no verdict")
	}

	verdict := extractReviewVerdict(text)
	if verdict == nil {
		return false, "", fmt.Errorf("master review did not emit a valid verdict JSON")
	}

	if verdict.Done {
		return true, "", nil
	}
	if verdict.Handoff == nil {
		return false, "", fmt.Errorf("master review says not done but no follow-up handoff")
	}
	if r := lastAssistantReasoning(hist); r != "" {
		verdict.Handoff.MasterReasoning = r
	}
	// Continue with a master turn seeded by the follow-up handoff so the loop
	// re-plans around the corrective instructions.
	return false, s.buildMasterInput(userTask, verdict.Handoff), nil
}

// reviewVerdict is the master's structured answer to a review turn.
type reviewVerdict struct {
	Done    bool     `json:"done"`
	Notes   string   `json:"notes,omitempty"`
	Handoff *Handoff `json:"handoff,omitempty"`
}

// extractReviewVerdict parses a {done, handoff} JSON object embedded anywhere in
// text, mirroring extractHandoffJSON's brace-scanning strategy.
func extractReviewVerdict(text string) *reviewVerdict {
	depth := 0
	start := -1
	var candidates []string
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
					candidates = append(candidates, text[start:i+1])
				}
			}
		}
	}
	for _, c := range candidates {
		var v reviewVerdict
		if json.Unmarshal([]byte(c), &v) == nil && (v.Done || v.Handoff != nil) {
			return &v
		}
	}
	var whole reviewVerdict
	if json.Unmarshal([]byte(text), &whole) != nil {
		return nil
	}
	// The whole-text parse should only count if it actually carries a verdict.
	if !whole.Done && whole.Handoff == nil {
		return nil
	}
	return &whole
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

// filteredSink wraps a sink to suppress Message events from internal loop
// controllers while passing through Phase, Reasoning, Usage, etc. for UX and History.
type filteredSink struct {
	event.Sink
}

func (f *filteredSink) Emit(e event.Event) {
	// Suppress internal Message events (master's JSON verdict, slave's WorkReport)
	// but allow all other event kinds for proper UX rendering and history.
	if e.Kind == event.Message {
		return
	}
	if f.Sink != nil {
		f.Sink.Emit(e)
	}
}

// NewFilteredSink creates a sink wrapper that suppresses Message events.
// Use for internal loop controllers (master/slave) to prevent their
// JSON outputs from reaching the user directly.
func NewFilteredSink(base event.Sink) event.Sink {
	return &filteredSink{base}
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