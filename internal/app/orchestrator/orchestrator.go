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

const (
	DefaultMaxSteps = 8
	// maxEmptyRetries caps how many times the orchestrator will re-poll the
	// model after an empty turn (no text, no tool calls). Empty turns are
	// almost always provider glitches; retrying twice with progressively
	// stronger nudges recovers from them without spinning forever when the
	// model is genuinely stuck.
	maxEmptyRetries = 2
)

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
	// PriorMessages is the canonical chat history from previous turns: user
	// prompts, assistant replies, tool calls, and tool results in order. When
	// non-empty it replaces the textual SessionContext as the source of
	// continuity, giving the model real tool_call_id pairings instead of a
	// flattened summary. SessionContext can still be supplied for terminal
	// snapshots and other supplementary context.
	PriorMessages []model.Message
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
	messages := buildInitialMessages(builder, request)
	sequence := 0
	if err := r.append(ctx, request.SessionID, &sequence, session.EventUserMessage, "user", request.UserRequest, nil); err != nil {
		return Response{}, err
	}

	var totalToolCalls int
	// Tool-followthrough can only fire once per Run. The heuristic is a
	// nudge, not a contract: if the model produces a "I'll check..." style
	// response without calling a tool we ask it once more. If the second
	// response is also tool-less, we accept it as the final answer rather
	// than spinning the loop until max_steps and surfacing a confusing
	// "agent stopped" error to the user.
	followThroughFired := false
	// forceToolNextTurn is set after the followthrough heuristic fires so the
	// next request goes out with tool_choice="required". Models like GLM
	// happily promise "I'll check the README" but never emit a tool_call
	// even after the orchestrator's polite nudge developer message — forcing
	// tool_choice=required gives them no escape hatch on the retry turn.
	// After the forced turn we drop back to default (auto) regardless.
	forceToolNextTurn := false
	emptyRetries := 0
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
		toolChoice := ""
		if forceToolNextTurn && len(tools) > 0 {
			toolChoice = "required"
		}
		modelEvents, err := r.Model.Stream(ctx, model.Request{
			Model:           request.Model,
			Messages:        prepared.Messages,
			Tools:           tools,
			MaxOutputTokens: prepared.MaxOutputTokens,
			Stream:          true,
			ToolChoice:      toolChoice,
			Metadata: map[string]string{
				"estimated_input_tokens": fmt.Sprintf("%d", prepared.InputTokens),
			},
		})
		// Single-shot: only force on the immediate retry, not subsequent turns.
		forceToolNextTurn = false
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
			// Empty-turn retry: a turn with no text AND no tool calls is
			// almost always a provider glitch (dropped chunk, tool_choice
			// matched nothing, content-filter redact). Real "I have
			// nothing to say" responses are vanishingly rare in an agent
			// context. Retry up to maxEmptyRetries times with a
			// progressively stronger nudge before accepting the empty
			// outcome. This keeps the agent from quietly giving up after
			// a single transient hiccup.
			if text == "" && emptyRetries < maxEmptyRetries && step < maxSteps {
				emptyRetries++
				notice := fmt.Sprintf("model returned empty response, retrying %d/%d", emptyRetries, maxEmptyRetries)
				if hint := diagnosticHint(turn.diagnostics); hint != "" {
					notice += " (" + hint + ")"
				}
				// Inline a short raw-chunk snippet directly in the chat
				// message so the user can immediately see what shape the
				// model is producing. Full dump goes to an artifact below.
				if turn.diagnostics != nil && turn.diagnostics.RawLastChunk != "" {
					snippet := turn.diagnostics.RawLastChunk
					if len(snippet) > 600 {
						snippet = snippet[:600] + "...[truncated]"
					}
					notice += "\n\nLast raw chunk:\n```json\n" + snippet + "\n```"
				}
				payload := assistantMessagePayload(step, "empty_retry", nil)
				if turn.diagnostics != nil {
					payload["diagnostics"] = diagnosticsPayload(turn.diagnostics)
				}
				if err := r.append(ctx, request.SessionID, &sequence, session.EventAssistantMessage, "assistant", notice, payload); err != nil {
					return Response{}, err
				}
				// Also log the raw response shape as an artifact so the user
				// can `d`etail it from chat. Without this, debugging "model
				// said tool_calls but we got nothing" requires tailing the
				// JSONL log file by hand.
				if turn.diagnostics != nil && turn.diagnostics.RawLastChunk != "" {
					if err := r.appendRawDump(ctx, request.SessionID, &sequence, step, turn.diagnostics); err != nil {
						return Response{}, err
					}
				}
				messages = append(messages, model.TextMessage(model.RoleDeveloper, emptyResponseRetryPrompt(emptyRetries, tools)))
				// Force tool use on the retry once tools exist; the empty
				// response often means the model couldn't decide what to do
				// next and needs the constraint nudge.
				if len(tools) > 0 {
					forceToolNextTurn = true
				}
				continue
			}
			if !followThroughFired && needsToolFollowThrough(text, tools) && step < maxSteps {
				if err := r.append(ctx, request.SessionID, &sequence, session.EventAssistantMessage, "assistant", text, assistantMessagePayload(step, "needs_tool_followthrough", nil)); err != nil {
					return Response{}, err
				}
				messages = append(messages, model.TextMessage(model.RoleAssistant, text))
				messages = append(messages, model.TextMessage(model.RoleDeveloper, toolFollowThroughPrompt(tools)))
				followThroughFired = true
				forceToolNextTurn = true
				continue
			}
			if err := r.append(ctx, request.SessionID, &sequence, session.EventAssistantMessage, "assistant", text, assistantMessagePayload(step, "final", nil)); err != nil {
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
		if err := r.append(ctx, request.SessionID, &sequence, session.EventAssistantMessage, "assistant", strings.TrimSpace(turn.text), assistantMessagePayload(step, "tool_calls", turn.toolCalls)); err != nil {
			return Response{}, err
		}

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

// emptyResponseRetryPrompt returns the developer-message nudge we inject
// after an empty model turn. The first retry is gentle ("did you mean to
// answer?"); the second is stricter ("if you have nothing to say, say so
// in plain text"). We deliberately avoid escalating into a forced tool
// call by name — emptyRetries is for ANY empty case, including ones where
// the model legitimately has nothing tool-relevant to do.
func emptyResponseRetryPrompt(attempt int, tools []model.ToolSpec) string {
	if attempt <= 1 {
		base := "Your previous response was empty. Either call a tool to do the work or write a brief textual answer for the user. Do not return another empty response."
		if len(tools) > 0 {
			names := toolNameList(tools)
			base += " Available tools: " + names + "."
		}
		return base
	}
	return "Your previous response was still empty. This is the last retry — respond with at least one word of text now, even if it is just a short status update like \"done\" or \"unable to proceed because <reason>\". Do not stay silent."
}

// appendRawDump records the raw last chunk of a model response as an
// artifact-bearing event so the user can inspect it from the workbench
// when an empty turn happens. Surfacing this in the UI is the difference
// between "the model is broken somehow" and "the model emitted X but we
// dropped it because Y" — the former is a dead end, the latter is a fix.
func (r Runner) appendRawDump(ctx context.Context, sessionID session.ID, sequence *int, step int, diag *model.Diagnostics) error {
	if r.Log == nil || diag == nil || diag.RawLastChunk == "" {
		return nil
	}
	*sequence++
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	ts := now()
	artifactID := fmt.Sprintf("dump-%d", ts.UnixNano())
	return r.Log.Append(ctx, session.Event{
		ID:        session.EventID(fmt.Sprintf("e%d", *sequence)),
		SessionID: sessionID,
		Type:      session.EventArtifact,
		At:        ts,
		Actor:     "model",
		Text:      "raw response from empty turn",
		Payload: map[string]any{
			"artifact": map[string]any{
				"id":        artifactID,
				"kind":      "model_response_dump",
				"title":     fmt.Sprintf("model response dump (step %d)", step),
				"body":      diag.RawLastChunk,
				"mime_type": "application/json",
				"metadata": map[string]any{
					"step":             fmt.Sprintf("%d", step),
					"finish_reason":    diag.FinishReason,
					"chunks":           fmt.Sprintf("%d", diag.ChunkCount),
					"text_deltas":      fmt.Sprintf("%d", diag.TextDeltaCount),
					"tool_calls":       fmt.Sprintf("%d", diag.ToolCallCount),
					"dropped_calls":    fmt.Sprintf("%d", diag.DroppedCalls),
					"local_only":       "false",
					"share_with_model": "false",
				},
			},
		},
	})
}

// diagnosticHint summarizes what the model actually returned in a one-line
// human-readable form for the empty-retry chat notice. Returns "" when the
// model client did not populate diagnostics or there is nothing useful to
// say about the failure.
func diagnosticHint(diag *model.Diagnostics) string {
	if diag == nil {
		return ""
	}
	var parts []string
	if diag.FinishReason != "" {
		parts = append(parts, "finish="+diag.FinishReason)
	}
	if diag.ChunkCount > 0 {
		parts = append(parts, fmt.Sprintf("chunks=%d", diag.ChunkCount))
	}
	if diag.RejectedChunks > 0 {
		parts = append(parts, fmt.Sprintf("sdk_rejected_chunks=%d", diag.RejectedChunks))
	}
	if diag.FallbackCalls > 0 {
		parts = append(parts, fmt.Sprintf("fallback_recovered_calls=%d", diag.FallbackCalls))
	}
	if diag.DroppedCalls > 0 {
		parts = append(parts, fmt.Sprintf("dropped_tool_calls=%d", diag.DroppedCalls))
	}
	if diag.TextDeltaCount == 0 && diag.ToolCallCount == 0 {
		parts = append(parts, "no text or tool_calls in response")
	}
	return strings.Join(parts, "; ")
}

func diagnosticsPayload(diag *model.Diagnostics) map[string]any {
	if diag == nil {
		return nil
	}
	out := map[string]any{
		"chunk_count":      diag.ChunkCount,
		"text_delta_count": diag.TextDeltaCount,
		"tool_call_count":  diag.ToolCallCount,
		"dropped_calls":    diag.DroppedCalls,
	}
	if diag.FinishReason != "" {
		out["finish_reason"] = diag.FinishReason
	}
	if diag.RawLastChunk != "" {
		// Cap a second time at 1KB for the JSONL log file: the larger raw
		// response is already capped at 2KB by the client; this is just to
		// keep individual log lines tractable.
		raw := diag.RawLastChunk
		if len(raw) > 1024 {
			raw = raw[:1024] + "...[truncated]"
		}
		out["raw_last_chunk"] = raw
	}
	return out
}

func toolNameList(tools []model.ToolSpec) string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) != "" {
			names = append(names, tool.Name)
		}
	}
	return strings.Join(names, ", ")
}

func toolFollowThroughPrompt(tools []model.ToolSpec) string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) != "" {
			names = append(names, tool.Name)
		}
	}
	// Give the model an explicit out: tool-call OR commit to the answer it
	// already produced. The earlier wording ("Continue now by calling the
	// available tool(s)") sometimes pushed the model into a stuck pattern
	// where it acknowledged the request but still refused to tool-call,
	// then the heuristic fired again next turn and the run timed out at
	// max_steps. Now we make the second turn the last word either way.
	return "Your last reply described tool-backed work but did not call a tool. " +
		"Either call the appropriate tool now (available: " + strings.Join(names, ", ") + ") " +
		"or treat your previous reply as the final answer if you no longer need a tool. " +
		"Do not write another \"I'll check...\" message — the next response will end the turn."
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
	text        string
	toolCalls   []model.ToolCall
	diagnostics *model.Diagnostics
}

func (r Runner) collectModelTurn(ctx context.Context, sessionID session.ID, sequence *int, step int, events <-chan model.Event) (modelTurn, error) {
	var text strings.Builder
	var toolCalls []model.ToolCall
	var diagnostics *model.Diagnostics

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
		case model.EventCompleted:
			if event.Diagnostics != nil {
				diagnostics = event.Diagnostics
			}
		case model.EventError:
			return modelTurn{}, errors.New(event.Error)
		}
	}

	return modelTurn{text: text.String(), toolCalls: toolCalls, diagnostics: diagnostics}, nil
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

func buildInitialMessages(builder prompt.Builder, request Request) []model.Message {
	scaffold := builder.Build(request.Environment, request.UserRequest)
	combined := combinedContext(request.SessionContext, request.TurnContext)
	if len(request.PriorMessages) == 0 {
		return withSessionContext(scaffold, combined)
	}
	// scaffold ends with the new user message; insert prior messages before it.
	if len(scaffold) == 0 {
		return append(append([]model.Message(nil), request.PriorMessages...), model.TextMessage(model.RoleUser, request.UserRequest))
	}
	insertAt := len(scaffold) - 1
	if scaffold[insertAt].Role != model.RoleUser {
		insertAt = len(scaffold)
	}
	merged := make([]model.Message, 0, len(scaffold)+len(request.PriorMessages)+1)
	merged = append(merged, scaffold[:insertAt]...)
	merged = append(merged, request.PriorMessages...)
	merged = append(merged, scaffold[insertAt:]...)
	return withSessionContext(merged, combined)
}

func assistantMessagePayload(step int, status string, toolCalls []model.ToolCall) map[string]any {
	payload := map[string]any{"step": step}
	if status != "" {
		payload["status"] = status
	}
	if len(toolCalls) == 0 {
		return payload
	}
	calls := make([]map[string]any, 0, len(toolCalls))
	for _, call := range toolCalls {
		entry := map[string]any{
			"id":   call.ID,
			"name": call.Name,
		}
		if len(call.Arguments) > 0 {
			entry["arguments"] = string(call.Arguments)
		}
		calls = append(calls, entry)
	}
	payload["tool_calls"] = calls
	return payload
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
