package contextmgr

import (
	"context"
	"encoding/json"
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

const HandoffSnapshotVersion = 1

type HandoffSnapshot struct {
	SchemaVersion   int       `json:"schema_version"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
	Objective       string    `json:"objective,omitempty"`
	Constraints     []string  `json:"constraints,omitempty"`
	UserPreferences []string  `json:"user_preferences,omitempty"`
	FilesTouched    []string  `json:"files_touched,omitempty"`
	CommandsRun     []string  `json:"commands_run,omitempty"`
	Approvals       []string  `json:"approvals,omitempty"`
	ActiveAgents    []string  `json:"active_agents,omitempty"`
	PendingWork     []string  `json:"pending_work,omitempty"`
	UnresolvedRisks []string  `json:"unresolved_risks,omitempty"`
	RecentMessages  []string  `json:"recent_messages,omitempty"`
}

func CompactSession(ctx context.Context, log ports.EventLog, sessionID session.ID, maxTokens int, now func() time.Time) (session.Event, error) {
	if now == nil {
		now = time.Now
	}
	events, err := collectEvents(ctx, log, sessionID)
	if err != nil {
		return session.Event{}, err
	}
	snapshot := BuildHandoffSnapshot(events, now())
	summary := RenderHandoffSnapshot(snapshot)
	if maxTokens > 0 && EstimateText(summary) > maxTokens {
		summary = truncateApproxTokens(summary, maxTokens)
	}
	tokens := EstimateText(summary)
	event := session.Event{
		Version:   session.EventFormatVersion,
		ID:        session.EventID(fmt.Sprintf("compact-%d", now().UnixNano())),
		SessionID: sessionID,
		Type:      session.EventContextCompacted,
		At:        now(),
		Actor:     "summarizer",
		Text:      summary,
		Payload: map[string]any{
			"schema_version":   HandoffSnapshotVersion,
			"handoff_snapshot": snapshot,
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

func BuildHandoffSnapshot(events []session.Event, updatedAt time.Time) HandoffSnapshot {
	snapshot := HandoffSnapshot{
		SchemaVersion: HandoffSnapshotVersion,
		UpdatedAt:     updatedAt,
	}
	for _, event := range events {
		text := strings.TrimSpace(event.Text)
		switch event.Type {
		case session.EventUserMessage:
			if text != "" {
				snapshot.Objective = text
				snapshot.RecentMessages = appendBounded(snapshot.RecentMessages, "user: "+oneLine(text), 8)
				if looksLikePreference(text) {
					snapshot.UserPreferences = appendUniqueBounded(snapshot.UserPreferences, oneLine(text), 8)
				}
			}
		case session.EventAssistantMessage:
			if text != "" {
				snapshot.RecentMessages = appendBounded(snapshot.RecentMessages, "assistant: "+oneLine(text), 8)
			}
		case session.EventTool:
			name := strings.TrimSpace(fmt.Sprint(event.Payload["name"]))
			args := strings.TrimSpace(fmt.Sprint(event.Payload["arguments"]))
			if name != "" && name != "<nil>" {
				line := name
				if args != "" && args != "<nil>" {
					line += " " + oneLine(args)
				}
				if command := commandFromTool(name, args); command != "" {
					snapshot.CommandsRun = appendUniqueBounded(snapshot.CommandsRun, command, 12)
				}
				snapshot.RecentMessages = appendBounded(snapshot.RecentMessages, "tool: "+line, 8)
			}
			for _, file := range filesFromToolArgs(args) {
				snapshot.FilesTouched = appendUniqueBounded(snapshot.FilesTouched, file, 24)
			}
		case session.EventArtifact:
			if artifact, ok := artifactPayloadMap(event); ok {
				kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(artifact["kind"])))
				metadata, _ := artifact["metadata"].(map[string]any)
				for _, file := range splitList(metadataString(metadata, "changed_files")) {
					snapshot.FilesTouched = appendUniqueBounded(snapshot.FilesTouched, file, 24)
				}
				if kind == "shell" {
					if command := metadataString(metadata, "command"); command != "" {
						snapshot.CommandsRun = appendUniqueBounded(snapshot.CommandsRun, command, 12)
					}
				}
				if kind == "patch" && metadataString(metadata, "state") == "preview" {
					title := artifactString(artifact, "title")
					if title == "" {
						title = "patch preview"
					}
					snapshot.PendingWork = appendUniqueBounded(snapshot.PendingWork, "review/apply "+title, 12)
				}
			}
		case session.EventPermissionRequested:
			action := strings.TrimSpace(fmt.Sprint(event.Payload["action"]))
			subject := strings.TrimSpace(fmt.Sprint(event.Payload["subject"]))
			decision := strings.TrimSpace(fmt.Sprint(event.Payload["approval"]))
			mode := strings.TrimSpace(fmt.Sprint(event.Payload["approval_mode"]))
			switch {
			case mode != "" && mode != "<nil>":
				snapshot.Constraints = appendUniqueBounded(snapshot.Constraints, "approval mode "+mode, 10)
			case decision != "" && decision != "<nil>":
				line := decision
				if action != "" && action != "<nil>" {
					line += " " + action
				}
				if subject != "" && subject != "<nil>" {
					line += " " + subject
				}
				snapshot.Approvals = appendUniqueBounded(snapshot.Approvals, line, 12)
				if decision == "rejected" {
					snapshot.UnresolvedRisks = appendUniqueBounded(snapshot.UnresolvedRisks, line, 12)
				}
			}
		case session.EventAgent:
			agentLine := agentSnapshotLine(event)
			if agentLine != "" {
				snapshot.ActiveAgents = appendUniqueBounded(snapshot.ActiveAgents, agentLine, 16)
				status := strings.ToLower(strings.TrimSpace(fmt.Sprint(event.Payload["status"])))
				if status == "running" || status == "blocked" {
					snapshot.PendingWork = appendUniqueBounded(snapshot.PendingWork, agentLine, 12)
				}
				if status == "failed" || status == "blocked" {
					snapshot.UnresolvedRisks = appendUniqueBounded(snapshot.UnresolvedRisks, agentLine, 12)
				}
			}
		case session.EventError:
			if text != "" {
				snapshot.UnresolvedRisks = appendUniqueBounded(snapshot.UnresolvedRisks, oneLine(text), 12)
			}
		}
	}
	return snapshot
}

func RenderHandoffSnapshot(snapshot HandoffSnapshot) string {
	var lines []string
	lines = append(lines, "Structured handoff snapshot:")
	if snapshot.Objective != "" {
		lines = append(lines, "- active objective: "+oneLine(snapshot.Objective))
	}
	appendSection := func(title string, values []string) {
		if len(values) == 0 {
			return
		}
		lines = append(lines, "- "+title+":")
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				lines = append(lines, "  - "+oneLine(value))
			}
		}
	}
	appendSection("constraints", snapshot.Constraints)
	appendSection("user preferences", snapshot.UserPreferences)
	appendSection("files touched", snapshot.FilesTouched)
	appendSection("commands run", snapshot.CommandsRun)
	appendSection("approvals", snapshot.Approvals)
	appendSection("active agents", snapshot.ActiveAgents)
	appendSection("pending work", snapshot.PendingWork)
	appendSection("unresolved risks", snapshot.UnresolvedRisks)
	appendSection("recent messages", snapshot.RecentMessages)
	if len(lines) == 1 {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
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
		if snapshot, ok := snapshotFromEvent(events[latestCompact]); ok {
			lines = appendTrimmed(lines, RenderHandoffSnapshot(snapshot), maxTokens/2)
		} else {
			lines = append(lines, "Previous compacted summary:")
			lines = appendTrimmed(lines, events[latestCompact].Text, maxTokens/2)
		}
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

func snapshotFromEvent(event session.Event) (HandoffSnapshot, bool) {
	if event.Type != session.EventContextCompacted || len(event.Payload) == 0 {
		return HandoffSnapshot{}, false
	}
	raw, ok := event.Payload["handoff_snapshot"]
	if !ok || raw == nil {
		return HandoffSnapshot{}, false
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return HandoffSnapshot{}, false
	}
	var snapshot HandoffSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return HandoffSnapshot{}, false
	}
	if snapshot.SchemaVersion == 0 {
		snapshot.SchemaVersion = HandoffSnapshotVersion
	}
	return snapshot, snapshot.SchemaVersion == HandoffSnapshotVersion
}

func artifactPayloadMap(event session.Event) (map[string]any, bool) {
	if len(event.Payload) == 0 {
		return nil, false
	}
	artifact, ok := event.Payload["artifact"].(map[string]any)
	return artifact, ok
}

func appendBounded(values []string, value string, limit int) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	values = append(values, value)
	if limit > 0 && len(values) > limit {
		return append([]string(nil), values[len(values)-limit:]...)
	}
	return values
}

func appendUniqueBounded(values []string, value string, limit int) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return appendBounded(values, value, limit)
}

func looksLikePreference(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "i want") ||
		strings.Contains(lower, "i wish") ||
		strings.Contains(lower, "please") ||
		strings.Contains(lower, "should") ||
		strings.Contains(lower, "need")
}

func commandFromTool(name string, args string) string {
	if name != "terminal_write" && name != "run_check" {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(args), &decoded); err != nil {
		return ""
	}
	for _, key := range []string{"command", "text"} {
		if value := strings.TrimSpace(fmt.Sprint(decoded[key])); value != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}

func filesFromToolArgs(args string) []string {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(args), &decoded); err != nil {
		return nil
	}
	var out []string
	if path := strings.TrimSpace(fmt.Sprint(decoded["path"])); path != "" && path != "<nil>" {
		out = append(out, path)
	}
	if changes, ok := decoded["changes"].([]any); ok {
		for _, raw := range changes {
			change, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			path := strings.TrimSpace(fmt.Sprint(change["path"]))
			if path != "" && path != "<nil>" {
				out = append(out, path)
			}
		}
	}
	return out
}

func splitList(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

func agentSnapshotLine(event session.Event) string {
	payload := event.Payload
	id := strings.TrimSpace(fmt.Sprint(payload["agent_id"]))
	role := strings.TrimSpace(fmt.Sprint(payload["role"]))
	status := strings.TrimSpace(fmt.Sprint(payload["status"]))
	summary := strings.TrimSpace(fmt.Sprint(payload["summary"]))
	if id == "<nil>" {
		id = ""
	}
	if role == "<nil>" {
		role = ""
	}
	if status == "<nil>" {
		status = ""
	}
	if summary == "<nil>" {
		summary = ""
	}
	parts := []string{}
	for _, part := range []string{id, role, status} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if summary != "" {
		parts = append(parts, oneLine(summary))
	}
	if len(parts) == 0 {
		return oneLine(event.Text)
	}
	return strings.Join(parts, " ")
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
