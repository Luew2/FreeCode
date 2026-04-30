package model

import "testing"

func TestModelRefRoundTrip(t *testing.T) {
	ref, err := ParseRef("local/coder")
	if err != nil {
		t.Fatalf("ParseRef returned error: %v", err)
	}
	if ref.Provider != "local" {
		t.Fatalf("Provider = %q, want local", ref.Provider)
	}
	if ref.ID != "coder" {
		t.Fatalf("ID = %q, want coder", ref.ID)
	}
	if got := ref.String(); got != "local/coder" {
		t.Fatalf("String() = %q, want local/coder", got)
	}
}

func TestNewModelDefaults(t *testing.T) {
	model := NewModel("local", "coder")
	if !model.Enabled {
		t.Fatal("new model should be enabled")
	}
	if model.Capabilities != (Capabilities{}) {
		t.Fatalf("Capabilities = %#v, want zero capabilities", model.Capabilities)
	}
	if model.Limits != (Limits{}) {
		t.Fatalf("Limits = %#v, want zero limits", model.Limits)
	}
}

func TestMessageCanRepresentToolLoop(t *testing.T) {
	assistant := Message{
		Role: RoleAssistant,
		ToolCalls: []ToolCall{
			{
				ID:        "call_1",
				Name:      "read_file",
				Arguments: []byte(`{"path":"README.md"}`),
			},
		},
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant tool calls = %d, want 1", len(assistant.ToolCalls))
	}

	tool := TextMessage(RoleTool, "file contents")
	tool.ToolCallID = "call_1"
	if tool.ToolCallID != assistant.ToolCalls[0].ID {
		t.Fatalf("tool call id = %q, want %q", tool.ToolCallID, assistant.ToolCalls[0].ID)
	}
}
