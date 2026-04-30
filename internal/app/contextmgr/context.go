package contextmgr

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Luew2/FreeCode/internal/core/model"
)

const charsPerToken = 4

type Budget struct {
	ContextWindow   int
	MaxInputTokens  int
	MaxOutputTokens int
}

type Prepared struct {
	Messages        []model.Message
	InputTokens     int
	ToolTokens      int
	MaxInputTokens  int
	MaxOutputTokens int
	Compacted       bool
	Notice          string
}

func FromLimits(limits model.Limits) Budget {
	return Budget{
		ContextWindow:   limits.ContextWindow,
		MaxOutputTokens: limits.MaxOutputTokens,
	}
}

func Prepare(messages []model.Message, tools []model.ToolSpec, budget Budget) (Prepared, error) {
	copied := cloneMessages(messages)
	toolTokens := EstimateTools(tools)
	maxInput := effectiveMaxInput(budget)
	inputTokens := EstimateMessages(copied) + toolTokens

	prepared := Prepared{
		Messages:        copied,
		InputTokens:     inputTokens,
		ToolTokens:      toolTokens,
		MaxInputTokens:  maxInput,
		MaxOutputTokens: budget.MaxOutputTokens,
	}
	if maxInput <= 0 || inputTokens <= maxInput {
		return prepared, nil
	}

	protectedTokens := EstimateMessages(protectedMessages(copied)) + toolTokens
	if protectedTokens > maxInput {
		return Prepared{}, fmt.Errorf("protected prompt estimate %d tokens exceeds max input budget %d; increase context budget", protectedTokens, maxInput)
	}
	compacted, dropped := compactMessages(copied, maxInput-toolTokens)
	inputTokens = EstimateMessages(compacted) + toolTokens
	if inputTokens > maxInput {
		return Prepared{}, fmt.Errorf("prompt estimate %d tokens exceeds max input budget %d after compaction", inputTokens, maxInput)
	}
	prepared.Messages = compacted
	prepared.InputTokens = inputTokens
	prepared.Compacted = true
	prepared.Notice = fmt.Sprintf("compacted prompt context; omitted about %d tokens", dropped)
	return prepared, nil
}

func EstimateMessages(messages []model.Message) int {
	total := 0
	for _, message := range messages {
		total += 4
		total += EstimateText(string(message.Role))
		total += EstimateText(message.Name)
		total += EstimateText(message.ToolCallID)
		for _, part := range message.Content {
			total += EstimateText(part.Text)
		}
		for _, call := range message.ToolCalls {
			total += EstimateText(call.ID)
			total += EstimateText(call.Name)
			total += EstimateText(string(call.Arguments))
		}
	}
	return total
}

func EstimateTools(tools []model.ToolSpec) int {
	total := 0
	for _, tool := range tools {
		total += 8
		total += EstimateText(tool.Name)
		total += EstimateText(tool.Description)
		total += estimateSchema(tool.InputSchema)
	}
	return total
}

func EstimateText(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + charsPerToken - 1) / charsPerToken
}

func effectiveMaxInput(budget Budget) int {
	if budget.MaxInputTokens > 0 {
		return budget.MaxInputTokens
	}
	if budget.ContextWindow <= 0 {
		return 0
	}
	output := budget.MaxOutputTokens
	if output < 0 {
		output = 0
	}
	safety := budget.ContextWindow / 20
	if safety < 128 {
		safety = 0
	}
	if safety > 4096 {
		safety = 4096
	}
	limit := budget.ContextWindow - output - safety
	if limit < 1 {
		return 1
	}
	return limit
}

func compactMessages(messages []model.Message, maxMessageTokens int) ([]model.Message, int) {
	if maxMessageTokens <= 0 {
		return nil, 0
	}
	originalTokens := EstimateMessages(messages)
	if originalTokens <= maxMessageTokens {
		return cloneMessages(messages), 0
	}

	var protected []model.Message
	var tail []model.Message
	// Tail selection treats assistant-with-tool_calls and the
	// matching RoleTool followups as one atomic group. Dropping
	// only an assistant turn while keeping its tool_result(s)
	// produces orphan tool messages that fail OpenAI/Anthropic
	// schema validation ("tool_call_id has no preceding
	// assistant message"). Building tail as a list of
	// non-protected message groups and keeping the last few
	// guarantees the kept window is always self-consistent.
	groups := nonProtectedGroups(messages)
	keepGroups := 4
	if keepGroups > len(groups) {
		keepGroups = len(groups)
	}
	for _, message := range messages {
		if message.Role == model.RoleSystem || message.Role == model.RoleDeveloper {
			protected = append(protected, message)
		}
	}
	if keepGroups > 0 {
		for _, group := range groups[len(groups)-keepGroups:] {
			tail = append(tail, group...)
		}
	}
	if len(tail) == 0 && len(messages) > 0 {
		// Fall back to the very last message even if it's protected,
		// matching prior behavior for sequences with no non-system
		// content.
		tail = append(tail, messages[len(messages)-1])
	}

	omitted := len(messages) - len(protected) - len(tail)
	if omitted < 0 {
		omitted = 0
	}
	summary := model.TextMessage(model.RoleDeveloper, fmt.Sprintf("Context compacted: omitted %d earlier message(s) to fit the configured model budget.", omitted))
	candidate := append([]model.Message{}, protected...)
	candidate = append(candidate, summary)
	candidate = append(candidate, tail...)

	if EstimateMessages(candidate) <= maxMessageTokens {
		return candidate, originalTokens - EstimateMessages(candidate)
	}

	candidate = truncateLargestTextParts(candidate, maxMessageTokens)
	return candidate, originalTokens - EstimateMessages(candidate)
}

// nonProtectedGroups partitions the non-system/non-developer messages
// into atomic groups so compaction never drops half of a tool_call
// pair. A group is one of:
//   - one user message
//   - one assistant message (with no tool_calls)
//   - one assistant-with-tool_calls plus all immediately-following
//     RoleTool messages that carry a tool_call_id matching one of the
//     assistant's tool_calls
//   - one orphan RoleTool message (only happens with malformed input;
//     compaction won't manufacture it but we still group defensively)
//
// The order of groups is the original source order. Protected messages
// (system/developer) are not included; they're handled separately.
func nonProtectedGroups(messages []model.Message) [][]model.Message {
	var groups [][]model.Message
	// Track tool_call_ids the most recent assistant turn produced so
	// we can decide whether a following RoleTool message belongs to
	// it. Once we open a new group (user or non-tool-call assistant)
	// we drop the pending IDs.
	pendingToolIDs := map[string]bool{}
	currentGroup := -1
	for _, message := range messages {
		if message.Role == model.RoleSystem || message.Role == model.RoleDeveloper {
			continue
		}
		switch message.Role {
		case model.RoleAssistant:
			if len(message.ToolCalls) > 0 {
				pendingToolIDs = map[string]bool{}
				for _, call := range message.ToolCalls {
					if call.ID != "" {
						pendingToolIDs[call.ID] = true
					}
				}
			} else {
				pendingToolIDs = map[string]bool{}
			}
			groups = append(groups, []model.Message{message})
			currentGroup = len(groups) - 1
		case model.RoleTool:
			// Attach to the open assistant-with-tool_calls group when
			// the tool_call_id matches one we expect; otherwise treat
			// it as its own (orphan) group so we never silently merge.
			if currentGroup >= 0 && (message.ToolCallID == "" || pendingToolIDs[message.ToolCallID]) && len(groups[currentGroup]) > 0 && groups[currentGroup][0].Role == model.RoleAssistant && len(groups[currentGroup][0].ToolCalls) > 0 {
				groups[currentGroup] = append(groups[currentGroup], message)
				delete(pendingToolIDs, message.ToolCallID)
			} else {
				groups = append(groups, []model.Message{message})
				currentGroup = len(groups) - 1
				pendingToolIDs = map[string]bool{}
			}
		default:
			pendingToolIDs = map[string]bool{}
			groups = append(groups, []model.Message{message})
			currentGroup = len(groups) - 1
		}
	}
	return groups
}

func truncateLargestTextParts(messages []model.Message, maxTokens int) []model.Message {
	copied := cloneMessages(messages)
	for EstimateMessages(copied) > maxTokens {
		index, partIndex, size := largestTextPart(copied)
		if index < 0 || partIndex < 0 || size <= 1 {
			break
		}
		over := EstimateMessages(copied) - maxTokens
		keepTokens := size - over - 16
		if keepTokens < size/2 {
			keepTokens = size / 2
		}
		if keepTokens < 1 {
			keepTokens = 1
		}
		copied[index].Content[partIndex].Text = truncateApproxTokens(copied[index].Content[partIndex].Text, keepTokens)
	}
	return copied
}

func largestTextPart(messages []model.Message) (int, int, int) {
	bestMessage := -1
	bestPart := -1
	bestSize := 0
	for i, message := range messages {
		if isProtected(message) {
			continue
		}
		for j, part := range message.Content {
			size := EstimateText(part.Text)
			if size > bestSize {
				bestMessage = i
				bestPart = j
				bestSize = size
			}
		}
	}
	return bestMessage, bestPart, bestSize
}

func protectedMessages(messages []model.Message) []model.Message {
	var protected []model.Message
	for i, message := range messages {
		if isProtected(message) || i == len(messages)-1 {
			protected = append(protected, message)
		}
	}
	return protected
}

func isProtected(message model.Message) bool {
	return message.Role == model.RoleSystem || message.Role == model.RoleDeveloper
}

func truncateApproxTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return "[truncated]"
	}
	maxChars := maxTokens * charsPerToken
	if len(text) <= maxChars {
		return text
	}
	if maxChars < 32 {
		return "..."
	}
	return strings.TrimSpace(text[:maxChars-16]) + "\n[truncated]"
}

func cloneMessages(messages []model.Message) []model.Message {
	copied := make([]model.Message, len(messages))
	for i, message := range messages {
		copied[i] = message
		copied[i].Content = append([]model.ContentPart(nil), message.Content...)
		copied[i].ToolCalls = append([]model.ToolCall(nil), message.ToolCalls...)
		for j, call := range copied[i].ToolCalls {
			copied[i].ToolCalls[j].Arguments = append([]byte(nil), call.Arguments...)
		}
	}
	return copied
}

func estimateSchema(value any) int {
	if value == nil {
		return 0
	}
	return EstimateText(fmt.Sprintf("%v", value))
}

func ValidateBudget(budget Budget) error {
	if budget.ContextWindow < 0 || budget.MaxInputTokens < 0 || budget.MaxOutputTokens < 0 {
		return errors.New("context budgets must not be negative")
	}
	return nil
}
