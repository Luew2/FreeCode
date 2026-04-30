package contextmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

// LoadMessageHistory walks the session event log and rebuilds the canonical
// chat-completion message sequence the agent saw in prior turns: every user
// prompt, the assistant's reply (with any tool_calls preserved), and each
// tool result keyed by tool_call_id. The reconstruction is best-effort — if
// a payload is malformed it is skipped rather than blocking the next turn.
//
// The returned messages do NOT include the system / developer scaffolding;
// orchestrator.Run rebuilds those from the prompt.Builder. This way the same
// conversation can be replayed across model swaps without baking in a stale
// system prompt.
func LoadMessageHistory(ctx context.Context, log ports.EventLog, sessionID session.ID) ([]model.Message, error) {
	if log == nil {
		return nil, nil
	}
	stream, err := log.Stream(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var messages []model.Message
	for event := range stream {
		switch event.Type {
		case session.EventUserMessage:
			text := strings.TrimSpace(event.Text)
			if text == "" {
				continue
			}
			messages = append(messages, model.TextMessage(model.RoleUser, text))
		case session.EventAssistantMessage:
			msg := assistantFromEvent(event)
			if isAssistantEmpty(msg) {
				continue
			}
			messages = append(messages, msg)
		case session.EventTool:
			msg, ok := toolFromEvent(event)
			if !ok {
				continue
			}
			messages = append(messages, msg)
		}
	}
	return messages, nil
}

// HistoryWithBudget returns a clipped copy of LoadMessageHistory that fits
// within maxTokens, preferring the most recent turns. We always keep at least
// the most recent user/assistant/tool group together so the model never sees
// orphaned tool_call → tool_result pairs.
func HistoryWithBudget(messages []model.Message, maxTokens int) []model.Message {
	if maxTokens <= 0 || len(messages) == 0 {
		return messages
	}
	if EstimateMessages(messages) <= maxTokens {
		return messages
	}
	// Walk from the end, accumulating messages until we exceed the budget,
	// then advance forward to a safe boundary (keeping tool_call →
	// tool_result groups intact).
	keep := 0
	tokens := 0
	for i := len(messages) - 1; i >= 0; i-- {
		size := EstimateMessages([]model.Message{messages[i]})
		if tokens+size > maxTokens && keep > 0 {
			break
		}
		tokens += size
		keep++
	}
	if keep == 0 {
		return nil
	}
	start := len(messages) - keep
	// Advance start past any leading tool result that no longer has its
	// matching assistant tool_call in scope.
	for start < len(messages) && messages[start].Role == model.RoleTool {
		start++
	}
	if start >= len(messages) {
		return nil
	}
	return append([]model.Message(nil), messages[start:]...)
}

func assistantFromEvent(event session.Event) model.Message {
	msg := model.Message{Role: model.RoleAssistant}
	if text := strings.TrimSpace(event.Text); text != "" {
		msg.Content = []model.ContentPart{{Type: model.ContentText, Text: text}}
	}
	if event.Payload == nil {
		return msg
	}
	rawCalls, ok := event.Payload["tool_calls"].([]any)
	if !ok {
		return msg
	}
	for _, raw := range rawCalls {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := stringField(entry, "id", "call_id")
		name := stringField(entry, "name")
		args := argumentsField(entry["arguments"])
		if id == "" && name == "" {
			continue
		}
		msg.ToolCalls = append(msg.ToolCalls, model.ToolCall{
			ID:        id,
			Name:      name,
			Arguments: args,
		})
	}
	return msg
}

func toolFromEvent(event session.Event) (model.Message, bool) {
	if event.Payload == nil {
		return model.Message{}, false
	}
	callID := stringField(event.Payload, "call_id", "tool_call_id", "id")
	if callID == "" {
		return model.Message{}, false
	}
	text := strings.TrimSpace(event.Text)
	if text == "" {
		return model.Message{}, false
	}
	msg := model.TextMessage(model.RoleTool, text)
	msg.ToolCallID = callID
	return msg, true
}

func isAssistantEmpty(msg model.Message) bool {
	if len(msg.ToolCalls) > 0 {
		return false
	}
	for _, part := range msg.Content {
		if strings.TrimSpace(part.Text) != "" {
			return false
		}
	}
	return true
}

func stringField(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			switch v := value.(type) {
			case string:
				if v != "" {
					return v
				}
			case fmt.Stringer:
				if s := v.String(); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func argumentsField(value any) []byte {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		// arguments are typically logged as the JSON string the model
		// produced; preserve them verbatim if they look like JSON, otherwise
		// re-encode so downstream UnmarshalJSON does not blow up.
		trimmed := strings.TrimSpace(v)
		if json.Valid([]byte(trimmed)) {
			return []byte(trimmed)
		}
		encoded, _ := json.Marshal(v)
		return encoded
	case map[string]any:
		data, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return data
	case []any:
		data, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return data
	default:
		return nil
	}
}
