package control

import (
	"testing"

	"reasonix/internal/provider"
)

func TestCountRoleToolMessages(t *testing.T) {
	hist := []provider.Message{
		{Role: provider.RoleUser, Content: "go"},
		{Role: provider.RoleAssistant, Content: "read the file"},
		{Role: provider.RoleTool, Content: `{"ok":true}`},
		{Role: provider.RoleAssistant, Content: "done"},
		{Role: provider.RoleTool, Content: `{"ok":false}`},
	}
	if got := countRoleToolMessages(hist); got != 2 {
		t.Fatalf("countRoleToolMessages = %d, want 2", got)
	}
	if got := countRoleToolMessages(nil); got != 0 {
		t.Fatalf("countRoleToolMessages(nil) = %d, want 0", got)
	}
}

func TestHandoffRequiresTools(t *testing.T) {
	if handoffRequiresTools(&Handoff{}) {
		t.Fatal("empty handoff must not require tools")
	}
	if handoffRequiresTools(&Handoff{Instructions: []Instruction{
		{ID: "1", Action: "delegate_respond", Description: "answer the user"},
	}}) {
		t.Fatal("respond-only handoff must not require tools")
	}
	if !handoffRequiresTools(&Handoff{Instructions: []Instruction{
		{ID: "1", Action: "delegate_read_file", Description: "read the file"},
		{ID: "2", Action: "delegate_respond", Description: "answer the user"},
	}}) {
		t.Fatal("handoff with a tool instruction must require tools")
	}
}
