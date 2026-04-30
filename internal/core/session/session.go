package session

import (
	"time"

	"github.com/Luew2/FreeCode/internal/core/artifact"
	"github.com/Luew2/FreeCode/internal/core/model"
)

type ID string
type EventID string

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
	ID        EventID
	SessionID ID
	Type      EventType
	At        time.Time
	Actor     string
	Artifact  artifact.ID
	Text      string
	Payload   map[string]any
}
