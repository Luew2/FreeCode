package ports

import (
	"context"

	"github.com/Luew2/FreeCode/internal/core/session"
)

type EventLog interface {
	Append(ctx context.Context, event session.Event) error
	Stream(ctx context.Context, id session.ID) (<-chan session.Event, error)
}

type SessionStore interface {
	Create(ctx context.Context, session session.Session) (session.Session, error)
	Get(ctx context.Context, id session.ID) (session.Session, error)
	List(ctx context.Context) ([]session.Session, error)
	Update(ctx context.Context, session session.Session) error
}
