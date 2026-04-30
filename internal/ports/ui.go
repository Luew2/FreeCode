package ports

import (
	"context"
	"time"

	"github.com/Luew2/FreeCode/internal/core/session"
)

type UIEvent struct {
	Type      string
	SessionID session.ID
	At        time.Time
	Payload   any
}

type UIEventSink interface {
	Publish(ctx context.Context, event UIEvent) error
}
