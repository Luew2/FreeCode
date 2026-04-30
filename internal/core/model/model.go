package model

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// debugEnabled is a process-wide toggle the TUI flips with :debug. When
// on, the model clients always populate Diagnostics.RawChunks (every
// chunk from every turn, not just turns we suspect we mishandled), and
// the workbench gives the UI an orange border so the user has a visible
// reminder that they're paying for the extra logging.
var debugEnabled atomic.Bool

// SetDebug toggles process-wide debug logging. Safe to call from any
// goroutine.
func SetDebug(on bool) { debugEnabled.Store(on) }

// Debug reports the current debug-mode state.
func Debug() bool { return debugEnabled.Load() }

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
	// Diagnostics is populated on EventCompleted to surface what the model
	// actually returned: finish_reason, chunk count, the count of tool
	// calls dropped because of missing fields, raw last-chunk JSON when
	// nothing else useful arrived. Used to debug "model returned nothing"
	// situations so the orchestrator can show a meaningful error instead
	// of swallowing them silently.
	Diagnostics *Diagnostics
}

type Diagnostics struct {
	FinishReason   string
	ChunkCount     int
	TextDeltaCount int
	ToolCallCount  int
	DroppedCalls   int
	// CompletionTokens is what the provider reports for output_tokens. If
	// this is > 0 but TextDeltaCount + ToolCallCount == 0 the model
	// produced something the parser didn't recognize — a strong signal
	// that the wire format has fields we're not handling (e.g.
	// reasoning_content, thinking blocks, or a legacy function_call).
	CompletionTokens int
	// RejectedChunks counts chunks the SDK accumulator silently rejected
	// (most commonly because the chunk's id didn't match the first chunk's
	// id — a quirk seen with some compat providers and proxies).
	RejectedChunks int
	// FallbackCalls counts tool calls recovered by our own per-chunk
	// accumulator when the SDK accumulator failed to capture them. A
	// non-zero value indicates the wire format had something the SDK
	// didn't handle directly.
	FallbackCalls int
	// ReasoningTokens estimates how much of the response lived in a
	// non-standard reasoning field (delta.reasoning_content for Z.ai's
	// GLM, DeepSeek, Qwen reasoning models; delta.thinking on some compat
	// providers). When the SDK reported completion_tokens > 0 but we got
	// no parseable content, the orchestrator surfaces this so the user
	// knows the model is "thinking" into a channel we don't render
	// directly.
	ReasoningTokens int
	// RawFirstChunk is the first chunk whose choices array was non-empty
	// (i.e. the first chunk that purportedly carried real model output).
	// This is what tells us which delta fields the provider is using —
	// "delta.content" vs "delta.reasoning_content" vs "delta.thinking"
	// vs the legacy "delta.function_call" — without us having to dump
	// the entire stream.
	RawFirstChunk string
	RawLastChunk  string
	// RawChunks holds every raw chunk JSON we received this turn (up to
	// the per-chunk and total caps applied at capture time). The model
	// client populates this only when it looks like the parser missed
	// content — i.e. when CompletionTokens > 0 but we extracted no text
	// and no tool calls. Dumping the whole stream is the only way to
	// debug a wire format we don't know about yet, and the user
	// explicitly asked for it; we keep it bounded so a runaway provider
	// can't blow up the session log.
	RawChunks []string
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
