package contextmgr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

func BuildSessionContext(ctx context.Context, log ports.EventLog, sessionID session.ID, maxTokens int) (string, int, error) {
	events, err := collectEvents(ctx, log, sessionID)
	if err != nil {
		return "", 0, err
	}
	summary, tokens := summarizeEvents(events, maxTokens)
	return summary, tokens, nil
}

func CompactSession(ctx context.Context, log ports.EventLog, sessionID session.ID, maxTokens int, now func() time.Time) (session.Event, error) {
	if now == nil {
		now = time.Now
	}
	events, err := collectEvents(ctx, log, sessionID)
	if err != nil {
		return session.Event{}, err
	}
	summary, tokens := summarizeEvents(events, maxTokens)
	event := session.Event{
		ID:        session.EventID(fmt.Sprintf("compact-%d", now().UnixNano())),
		SessionID: sessionID,
		Type:      session.EventContextCompacted,
		At:        now(),
		Actor:     "summarizer",
		Text:      summary,
		Payload: map[string]any{
			"estimated_tokens": tokens,
			"source_events":    len(events),
		},
	}
	if log != nil {
		if err := log.Append(ctx, event); err != nil {
			return session.Event{}, err
		}
	}
	return event, nil
}

func collectEvents(ctx context.Context, log ports.EventLog, sessionID session.ID) ([]session.Event, error) {
	if log == nil {
		return nil, nil
	}
	stream, err := log.Stream(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var events []session.Event
	for event := range stream {
		if event.Type == session.EventModel {
			continue
		}
		if isLocalOnlyEvent(event) {
			continue
		}
		events = append(events, event)
	}
	return events, nil
}

func isLocalOnlyEvent(event session.Event) bool {
	if len(event.Payload) == 0 {
		return false
	}
	artifact, ok := event.Payload["artifact"].(map[string]any)
	if !ok {
		return false
	}
	metadata, ok := artifact["metadata"].(map[string]any)
	if !ok {
		return false
	}
	return metadataFlag(metadata, "local_only") || metadataFlag(metadata, "private") || metadataString(metadata, "share_with_model") == "false"
}

func metadataFlag(metadata map[string]any, key string) bool {
	value := metadataString(metadata, key)
	return value == "true" || value == "1" || value == "yes"
}

func metadataString(metadata map[string]any, key string) string {
	switch value := metadata[key].(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(value))
	case bool:
		return strconv.FormatBool(value)
	case fmt.Stringer:
		return strings.ToLower(strings.TrimSpace(value.String()))
	default:
		return ""
	}
}

func summarizeEvents(events []session.Event, maxTokens int) (string, int) {
	if len(events) == 0 {
		return "", 0
	}
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	latestCompact := -1
	for i, event := range events {
		if event.Type == session.EventContextCompacted && strings.TrimSpace(event.Text) != "" {
			latestCompact = i
		}
	}

	var lines []string
	if latestCompact >= 0 {
		lines = append(lines, "Previous compacted summary:")
		lines = appendTrimmed(lines, events[latestCompact].Text, maxTokens/2)
		lines = append(lines, "", "Recent events since compact:")
		events = events[latestCompact+1:]
	} else {
		lines = append(lines, "Recent session events:")
	}

	recent := eventSummaryLines(events)
	selectedStart := len(recent)
	for i := len(recent) - 1; i >= 0; i-- {
		candidate := append([]string(nil), lines...)
		candidate = append(candidate, recent[i:]...)
		text := strings.TrimSpace(strings.Join(candidate, "\n"))
		if EstimateText(text) <= maxTokens {
			selectedStart = i
			continue
		}
		break
	}
	if selectedStart < len(recent) {
		lines = append(lines, recent[selectedStart:]...)
	}
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if EstimateText(text) > maxTokens {
		text = truncateApproxTokens(text, maxTokens)
	}
	return text, EstimateText(text)
}

func eventSummaryLines(events []session.Event) []string {
	lines := make([]string, 0, len(events))
	for _, event := range events {
		if terminal, ok := sharedTerminalArtifact(event); ok {
			body := strings.TrimSpace(terminal.body)
			if body == "" {
				body = strings.TrimSpace(event.Text)
			}
			if body == "" {
				continue
			}
			if EstimateText(body) > 768 {
				body = truncateApproxTokens(body, 768)
			}
			title := strings.TrimSpace(terminal.title)
			if title == "" {
				title = "shared terminal output"
			}
			block := []string{fmt.Sprintf("- shared_terminal %s: %s", nonEmpty(event.Actor, "user"), title)}
			for _, line := range strings.Split(body, "\n") {
				block = append(block, "  "+strings.TrimRight(line, "\r"))
			}
			lines = append(lines, strings.Join(block, "\n"))
			continue
		}
		text := strings.TrimSpace(event.Text)
		if text == "" && len(event.Payload) > 0 {
			text = fmt.Sprintf("%v", event.Payload)
		}
		if text == "" {
			continue
		}
		if EstimateText(text) > 256 {
			text = truncateApproxTokens(text, 256)
		}
		if event.Type == session.EventTool {
			name := strings.TrimSpace(fmt.Sprint(event.Payload["name"]))
			args := strings.TrimSpace(fmt.Sprint(event.Payload["arguments"]))
			if args != "" && args != "<nil>" && EstimateText(args) > 128 {
				args = truncateApproxTokens(args, 128)
			}
			label := "tool"
			if name != "" && name != "<nil>" {
				label += " " + name
			}
			if args != "" && args != "<nil>" {
				label += " " + oneLine(args)
			}
			lines = append(lines, fmt.Sprintf("- %s %s: %s", label, nonEmpty(event.Actor, "unknown"), oneLine(text)))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s %s: %s", event.Type, nonEmpty(event.Actor, "unknown"), oneLine(text)))
	}
	return lines
}

type terminalArtifact struct {
	title string
	body  string
}

func sharedTerminalArtifact(event session.Event) (terminalArtifact, bool) {
	if event.Type != session.EventArtifact || len(event.Payload) == 0 {
		return terminalArtifact{}, false
	}
	artifact, ok := event.Payload["artifact"].(map[string]any)
	if !ok {
		return terminalArtifact{}, false
	}
	if strings.ToLower(strings.TrimSpace(fmt.Sprint(artifact["kind"]))) != "terminal" {
		return terminalArtifact{}, false
	}
	metadata, _ := artifact["metadata"].(map[string]any)
	if metadataString(metadata, "share_with_model") == "false" {
		return terminalArtifact{}, false
	}
	return terminalArtifact{
		title: artifactString(artifact, "title"),
		body:  artifactString(artifact, "body"),
	}, true
}

func artifactString(artifact map[string]any, key string) string {
	value, ok := artifact[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func appendTrimmed(lines []string, text string, maxTokens int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return lines
	}
	if maxTokens > 0 && EstimateText(text) > maxTokens {
		text = truncateApproxTokens(text, maxTokens)
	}
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, line)
	}
	return lines
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func nonEmpty(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
