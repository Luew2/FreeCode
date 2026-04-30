package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Luew2/FreeCode/internal/app/contextmgr"
	"github.com/Luew2/FreeCode/internal/app/prompt"
	"github.com/Luew2/FreeCode/internal/core/artifact"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

const DefaultMaxSteps = 8

type Runner struct {
	Model  ports.ModelClient
	Tools  ports.ToolRegistry
	Log    ports.EventLog
	Prompt prompt.Builder
	Now    func() time.Time
}

type Request struct {
	SessionID      session.ID
	Model          model.Ref
	UserRequest    string
	Environment    prompt.Environment
	MaxSteps       int
	SessionContext string
	TurnContext    string
	ContextBudget  contextmgr.Budget
}

type Response struct {
	Text      string
	Steps     int
	ToolCalls int
}

func (r Runner) Run(ctx context.Context, request Request) (Response, error) {
	if r.Model == nil {
		return Response{}, errors.New("model client is required")
	}
	if strings.TrimSpace(request.UserRequest) == "" {
		return Response{}, errors.New("user request is required")
	}
	if request.Model == (model.Ref{}) {
		return Response{}, errors.New("model ref is required")
	}

	maxSteps := request.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}

	builder := r.Prompt
	if builder == (prompt.Builder{}) {
		builder = prompt.NewBuilder()
	}
	messages := withSessionContext(builder.Build(request.Environment, request.UserRequest), combinedContext(request.SessionContext, request.TurnContext))
	sequence := 0
	if err := r.append(ctx, request.SessionID, &sequence, session.EventUserMessage, "user", request.UserRequest, nil); err != nil {
		return Response{}, err
	}

	var totalToolCalls int
	for step := 1; step <= maxSteps; step++ {
		tools := r.toolSpecs()
		prepared, err := contextmgr.Prepare(messages, tools, request.ContextBudget)
		if err != nil {
			_ = r.append(ctx, request.SessionID, &sequence, session.EventError, "context", err.Error(), map[string]any{"step": step})
			return Response{}, err
		}
		if prepared.Compacted {
			messages = prepared.Messages
			if err := r.append(ctx, request.SessionID, &sequence, session.EventContextCompacted, "context", prepared.Notice, map[string]any{
				"step":              step,
				"input_tokens":      prepared.InputTokens,
				"max_input_tokens":  prepared.MaxInputTokens,
				"max_output_tokens": prepared.MaxOutputTokens,
			}); err != nil {
				return Response{}, err
			}
		}
		modelEvents, err := r.Model.Stream(ctx, model.Request{
			Model:           request.Model,
			Messages:        prepared.Messages,
			Tools:           tools,
			MaxOutputTokens: prepared.MaxOutputTokens,
			Stream:          true,
			Metadata: map[string]string{
				"estimated_input_tokens": fmt.Sprintf("%d", prepared.InputTokens),
			},
		})
		if err != nil {
			_ = r.append(ctx, request.SessionID, &sequence, session.EventError, "model", err.Error(), map[string]any{"step": step})
			return Response{}, err
		}

		turn, err := r.collectModelTurn(ctx, request.SessionID, &sequence, step, modelEvents)
		if err != nil {
			return Response{}, err
		}
		if len(turn.toolCalls) == 0 {
			text := strings.TrimSpace(turn.text)
			if needsToolFollowThrough(text, tools) && step < maxSteps {
				if err := r.append(ctx, request.SessionID, &sequence, session.EventAssistantMessage, "assistant", text, map[string]any{"step": step, "status": "needs_tool_followthrough"}); err != nil {
					return Response{}, err
				}
				messages = append(messages, model.TextMessage(model.RoleAssistant, text))
				messages = append(messages, model.TextMessage(model.RoleDeveloper, toolFollowThroughPrompt(tools)))
				continue
			}
			if err := r.append(ctx, request.SessionID, &sequence, session.EventAssistantMessage, "assistant", text, map[string]any{"step": step}); err != nil {
				return Response{}, err
			}
			return Response{Text: text, Steps: step, ToolCalls: totalToolCalls}, nil
		}

		totalToolCalls += len(turn.toolCalls)
		assistant := model.Message{Role: model.RoleAssistant, ToolCalls: turn.toolCalls}
		if strings.TrimSpace(turn.text) != "" {
			assistant.Content = []model.ContentPart{{Type: model.ContentText, Text: turn.text}}
		}
		messages = append(messages, assistant)

		for _, toolCall := range turn.toolCalls {
			resultText, err := r.runTool(ctx, request.SessionID, &sequence, toolCall)
			if err != nil {
				return Response{}, err
			}
			toolMessage := model.TextMessage(model.RoleTool, resultText)
			toolMessage.ToolCallID = toolCall.ID
			messages = append(messages, toolMessage)
		}
		if terminalWriteNeedsRead(turn.toolCalls, tools) {
			messages = append(messages, model.TextMessage(model.RoleDeveloper, "You just wrote to the shared terminal. Before giving a final answer, call terminal_read to inspect the command output and then answer based on what terminal_read returns."))
		}
	}

	err := fmt.Errorf("agent stopped after %d steps without final response", maxSteps)
	_ = r.append(ctx, request.SessionID, &sequence, session.EventError, "orchestrator", err.Error(), map[string]any{"max_steps": maxSteps})
	return Response{}, err
}

func needsToolFollowThrough(text string, tools []model.ToolSpec) bool {
	if len(tools) == 0 {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return false
	}
	cues := []string{
		"i'll", "i will", "i’ll", "i am going to", "i'm going to", "i’m going to",
		"let me", "i'll start", "i will start", "i’ll start", "start by", "first i'll", "first i will",
	}
	actions := []string{
		"inspect", "explore", "explor", "look", "read", "search", "scan", "check", "run", "test", "verify",
		"edit", "write", "patch", "change", "delegate", "spawn", "review", "analyz",
	}
	hasCue := containsAny(normalized, cues)
	hasAction := containsAny(normalized, actions)
	if !hasCue || !hasAction {
		return false
	}
	if strings.Contains(normalized, "let me know") && !strings.Contains(normalized, "let me check") && !strings.Contains(normalized, "let me run") && !strings.Contains(normalized, "let me inspect") && !strings.Contains(normalized, "let me read") && !strings.Contains(normalized, "let me search") {
		return false
	}
	return true
}

func toolFollowThroughPrompt(tools []model.ToolSpec) string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) != "" {
			names = append(names, tool.Name)
		}
	}
	return "You described tool-backed work but did not call a tool. Continue now by calling the available tool(s) needed for the work before giving a final answer. Do not ask the user to wait and do not only describe the next step. Available tools: " + strings.Join(names, ", ")
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func terminalWriteNeedsRead(calls []model.ToolCall, tools []model.ToolSpec) bool {
	hasTerminalRead := false
	for _, tool := range tools {
		if tool.Name == "terminal_read" {
			hasTerminalRead = true
			break
		}
	}
	if !hasTerminalRead {
		return false
	}
	for _, call := range calls {
		if call.Name == "terminal_write" {
			return true
		}
	}
	return false
}

type modelTurn struct {
	text      string
	toolCalls []model.ToolCall
}

func (r Runner) collectModelTurn(ctx context.Context, sessionID session.ID, sequence *int, step int, events <-chan model.Event) (modelTurn, error) {
	var text strings.Builder
	var toolCalls []model.ToolCall

	for event := range events {
		if err := r.append(ctx, sessionID, sequence, session.EventModel, "model", event.Text, modelEventPayload(step, event)); err != nil {
			return modelTurn{}, err
		}
		switch event.Type {
		case model.EventTextDelta:
			text.WriteString(event.Text)
		case model.EventToolCall:
			if event.ToolCall != nil {
				toolCalls = append(toolCalls, *event.ToolCall)
			}
		case model.EventError:
			return modelTurn{}, errors.New(event.Error)
		}
	}

	return modelTurn{text: text.String(), toolCalls: toolCalls}, nil
}

func (r Runner) runTool(ctx context.Context, sessionID session.ID, sequence *int, call model.ToolCall) (string, error) {
	if r.Tools == nil {
		message := "tool registry is not configured"
		if err := r.append(ctx, sessionID, sequence, session.EventError, "tool", message, toolCallPayload(call, nil)); err != nil {
			return "", err
		}
		return "tool error: " + message, nil
	}

	result, err := r.Tools.RunTool(ctx, call)
	if err != nil {
		message := err.Error()
		if logErr := r.append(ctx, sessionID, sequence, session.EventTool, "tool", message, toolCallPayload(call, map[string]any{"error": message})); logErr != nil {
			return "", logErr
		}
		return "tool error: " + message, nil
	}

	payload := stringMetadataPayload(result.Metadata)
	if result.Artifact != nil {
		if payload == nil {
			payload = map[string]any{}
		}
		payload["artifact"] = artifactPayload(*result.Artifact)
	}
	if err := r.append(ctx, sessionID, sequence, session.EventTool, "tool", result.Content, toolCallPayload(call, payload)); err != nil {
		return "", err
	}
	return result.Content, nil
}

func (r Runner) toolSpecs() []model.ToolSpec {
	if r.Tools == nil {
		return nil
	}
	return r.Tools.Tools()
}

func (r Runner) append(ctx context.Context, sessionID session.ID, sequence *int, eventType session.EventType, actor string, text string, payload map[string]any) error {
	if r.Log == nil {
		return nil
	}
	*sequence++
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	if err := r.Log.Append(ctx, session.Event{
		ID:        session.EventID(fmt.Sprintf("e%d", *sequence)),
		SessionID: sessionID,
		Type:      eventType,
		At:        now(),
		Actor:     actor,
		Text:      text,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("append session event: %w", err)
	}
	return nil
}

func withSessionContext(messages []model.Message, sessionContext string) []model.Message {
	sessionContext = strings.TrimSpace(sessionContext)
	if sessionContext == "" || len(messages) == 0 {
		return messages
	}
	contextMessage := model.TextMessage(model.RoleDeveloper, "Session context:\n"+sessionContext)
	insertAt := len(messages)
	if messages[len(messages)-1].Role == model.RoleUser {
		insertAt = len(messages) - 1
	}
	withContext := make([]model.Message, 0, len(messages)+1)
	withContext = append(withContext, messages[:insertAt]...)
	withContext = append(withContext, contextMessage)
	withContext = append(withContext, messages[insertAt:]...)
	return withContext
}

func combinedContext(sessionContext string, turnContext string) string {
	sessionContext = strings.TrimSpace(sessionContext)
	turnContext = strings.TrimSpace(turnContext)
	switch {
	case sessionContext == "" && turnContext == "":
		return ""
	case sessionContext == "":
		return "Active turn context:\n" + turnContext
	case turnContext == "":
		return sessionContext
	default:
		return sessionContext + "\n\nActive turn context:\n" + turnContext
	}
}

func modelEventPayload(step int, event model.Event) map[string]any {
	payload := map[string]any{
		"step":       step,
		"event_type": string(event.Type),
	}
	if event.ToolCall != nil {
		payload["tool_call"] = map[string]any{
			"id":        event.ToolCall.ID,
			"name":      event.ToolCall.Name,
			"arguments": string(event.ToolCall.Arguments),
		}
	}
	if event.Usage != nil {
		payload["usage"] = map[string]any{
			"input_tokens":  event.Usage.InputTokens,
			"output_tokens": event.Usage.OutputTokens,
			"total_tokens":  event.Usage.TotalTokens,
		}
	}
	if event.Error != "" {
		payload["error"] = event.Error
	}
	return payload
}

func toolCallPayload(call model.ToolCall, extra map[string]any) map[string]any {
	payload := map[string]any{
		"call_id":   call.ID,
		"name":      call.Name,
		"arguments": string(call.Arguments),
	}
	for key, value := range extra {
		payload[key] = value
	}
	return payload
}

func stringMetadataPayload(metadata map[string]string) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	payload := make(map[string]any, len(metadata))
	for key, value := range metadata {
		payload[key] = value
	}
	return payload
}

func artifactPayload(value artifact.Artifact) map[string]any {
	payload := map[string]any{
		"id":   value.ID.String(),
		"kind": string(value.Kind),
	}
	if value.Title != "" {
		payload["title"] = value.Title
	}
	if value.URI != "" {
		payload["uri"] = value.URI
	}
	if value.MIMEType != "" {
		payload["mime_type"] = value.MIMEType
	}
	if value.Body != "" {
		payload["body"] = value.Body
	}
	if len(value.Metadata) > 0 {
		metadata := make(map[string]any, len(value.Metadata))
		for key, item := range value.Metadata {
			metadata[key] = item
		}
		payload["metadata"] = metadata
	}
	return payload
}
