package model

import (
	"fmt"
	"strings"
)

type ProviderID string
type ModelID string

// ProtocolID is intentionally opaque in core. Concrete protocol values belong
// to provider adapters and persisted config, not to the domain model.
type ProtocolID string

type Provider struct {
	ID           ProviderID
	Name         string
	Protocol     ProtocolID
	BaseURL      string
	Secret       SecretRef
	DefaultModel ModelID
	Enabled      bool
	Metadata     map[string]string
}

type SecretRef struct {
	Name   string
	Source string
}

type Ref struct {
	Provider ProviderID
	ID       ModelID
}

func NewRef(provider ProviderID, id ModelID) Ref {
	return Ref{Provider: provider, ID: id}
}

func ParseRef(value string) (Ref, error) {
	provider, id, ok := strings.Cut(value, "/")
	if !ok || provider == "" || id == "" {
		return Ref{}, fmt.Errorf("invalid model ref %q", value)
	}
	return NewRef(ProviderID(provider), ModelID(id)), nil
}

func (r Ref) String() string {
	switch {
	case r.Provider == "" && r.ID == "":
		return ""
	case r.Provider == "":
		return string(r.ID)
	case r.ID == "":
		return string(r.Provider)
	default:
		return string(r.Provider) + "/" + string(r.ID)
	}
}

type Capabilities struct {
	Tools       bool
	Streaming   bool
	JSONOutput  bool
	Vision      bool
	Embeddings  bool
	Reasoning   bool
	Attachments bool
}

func DefaultCapabilities() Capabilities {
	return Capabilities{}
}

type Limits struct {
	ContextWindow   int
	MaxOutputTokens int
}

func DefaultLimits() Limits {
	return Limits{}
}

type Model struct {
	Ref          Ref
	Name         string
	Capabilities Capabilities
	Limits       Limits
	Enabled      bool
	Metadata     map[string]string
}

func NewModel(provider ProviderID, id ModelID) Model {
	return Model{
		Ref:          NewRef(provider, id),
		Name:         string(id),
		Capabilities: DefaultCapabilities(),
		Limits:       DefaultLimits(),
		Enabled:      true,
	}
}

type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ContentPartType string

const (
	ContentText ContentPartType = "text"
)

type ContentPart struct {
	Type ContentPartType
	Text string
}

type Message struct {
	Role       Role
	Name       string
	Content    []ContentPart
	ToolCallID string
	ToolCalls  []ToolCall
}

func TextMessage(role Role, text string) Message {
	return Message{
		Role: role,
		Content: []ContentPart{
			{Type: ContentText, Text: text},
		},
	}
}

type ToolSpec struct {
	Name        string
	Description string
	InputSchema map[string]any
}

type Request struct {
	Model           Ref
	Messages        []Message
	Tools           []ToolSpec
	MaxOutputTokens int
	Temperature     *float64
	Stream          bool
	Metadata        map[string]string
	// ToolChoice optionally constrains the model's tool use for this turn.
	// Empty string defers to provider defaults (typically "auto"). "required"
	// forces the model to call SOME tool — useful as the orchestrator's
	// follow-through nudge for models like GLM that often promise tool work
	// in text but do not actually emit a tool_call. "none" disables tools.
	ToolChoice string
}

type EventType string

const (
	EventStarted   EventType = "started"
	EventTextDelta EventType = "text_delta"
	EventToolCall  EventType = "tool_call"
	EventUsage     EventType = "usage"
	EventCompleted EventType = "completed"
	EventError     EventType = "error"
)

type Event struct {
	Type     EventType
	Text     string
	ToolCall *ToolCall
	Usage    *Usage
	Error    string
	Metadata map[string]string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments []byte
}

type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}
