package commands

import (
	"context"
	"fmt"
	"io"

	"github.com/Luew2/FreeCode/internal/app/contextmgr"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

type CompactOptions struct {
	SessionID string
	MaxTokens int
}

func Compact(ctx context.Context, w io.Writer, log ports.EventLog, opts CompactOptions) error {
	sessionID := session.ID(opts.SessionID)
	if sessionID == "" {
		sessionID = "default"
	}
	event, err := contextmgr.CompactSession(ctx, log, sessionID, opts.MaxTokens, nil)
	if err != nil {
		return err
	}
	tokens := 0
	if raw, ok := event.Payload["estimated_tokens"].(int); ok {
		tokens = raw
	}
	_, err = fmt.Fprintf(w, "compacted session %s ~%d tokens\n", sessionID, tokens)
	return err
}
