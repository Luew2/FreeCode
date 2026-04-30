package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/Luew2/FreeCode/internal/core/artifact"
	"github.com/Luew2/FreeCode/internal/core/model"
)

type ID string
type EventID string

const EventFormatVersion = 1

type Status string

const (
	StatusActive   Status = "active"
	StatusArchived Status = "archived"
)

type Session struct {
	ID          ID
	Title       string
	Status      Status
	ActiveModel model.Ref
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func New(id ID, title string, at time.Time) Session {
	return Session{
		ID:        id,
		Title:     title,
		Status:    StatusActive,
		CreatedAt: at,
		UpdatedAt: at,
	}
}

type EventType string

const (
	EventSessionStarted      EventType = "session_started"
	EventUserMessage         EventType = "user_message"
	EventAssistantMessage    EventType = "assistant_message"
	EventModel               EventType = "model"
	EventTool                EventType = "tool"
	EventArtifact            EventType = "artifact"
	EventContextCompacted    EventType = "context_compacted"
	EventPermissionRequested EventType = "permission_requested"
	EventAgent               EventType = "agent"
	EventError               EventType = "error"
)

type Event struct {
	Version   int
	ID        EventID
	SessionID ID
	Type      EventType
	At        time.Time
	Actor     string
	Artifact  artifact.ID
	Text      string
	Payload   map[string]any
}

type RecoveryReport struct {
	Events         []Event
	MalformedLines int
	TruncatedTail  bool
	Errors         []string
}

func RecoverLog(r io.Reader, id ID) (RecoveryReport, error) {
	if r == nil {
		return RecoveryReport{}, nil
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return RecoveryReport{}, err
	}
	report := RecoveryReport{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := append([]byte(nil), scanner.Bytes()...)
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(raw, &event); err != nil {
			if !bytes.HasSuffix(data, []byte("\n")) && scanner.Err() == nil && isLastLine(data, raw) {
				report.TruncatedTail = true
			} else {
				report.MalformedLines++
				report.Errors = append(report.Errors, fmt.Sprintf("line %d: %v", line, err))
			}
			continue
		}
		if event.Version == 0 {
			event.Version = EventFormatVersion
		}
		if id == "" || event.SessionID == id {
			report.Events = append(report.Events, event)
		}
	}
	if err := scanner.Err(); err != nil {
		return report, err
	}
	return report, nil
}

func isLastLine(data []byte, raw []byte) bool {
	trimmed := bytes.TrimRight(data, "\n")
	lastBreak := bytes.LastIndexByte(trimmed, '\n')
	if lastBreak >= 0 {
		trimmed = trimmed[lastBreak+1:]
	}
	return bytes.Equal(bytes.TrimSpace(trimmed), bytes.TrimSpace(raw))
}

func RebuildIndex(events []Event) []Session {
	byID := map[ID]Session{}
	for _, event := range events {
		if event.SessionID == "" {
			continue
		}
		current := byID[event.SessionID]
		if current.ID == "" {
			title := "Session " + string(event.SessionID)
			if event.Type == EventUserMessage && event.Text != "" {
				title = event.Text
			}
			current = New(event.SessionID, title, event.At)
			current.ActiveModel = eventModel(event)
		}
		if current.CreatedAt.IsZero() || (!event.At.IsZero() && event.At.Before(current.CreatedAt)) {
			current.CreatedAt = event.At
		}
		if event.At.After(current.UpdatedAt) {
			current.UpdatedAt = event.At
		}
		if current.Title == "" && event.Text != "" {
			current.Title = event.Text
		}
		if current.ActiveModel == (model.Ref{}) {
			current.ActiveModel = eventModel(event)
		}
		byID[event.SessionID] = current
	}
	sessions := make([]Session, 0, len(byID))
	for _, item := range byID {
		if item.CreatedAt.IsZero() {
			item.CreatedAt = item.UpdatedAt
		}
		if item.UpdatedAt.IsZero() {
			item.UpdatedAt = item.CreatedAt
		}
		if item.Status == "" {
			item.Status = StatusActive
		}
		sessions = append(sessions, item)
	}
	return sessions
}

func eventModel(event Event) model.Ref {
	if event.Payload == nil {
		return model.Ref{}
	}
	raw, _ := event.Payload["model"].(string)
	if raw == "" {
		return model.Ref{}
	}
	ref, err := model.ParseRef(raw)
	if err != nil {
		return model.Ref{}
	}
	return ref
}
