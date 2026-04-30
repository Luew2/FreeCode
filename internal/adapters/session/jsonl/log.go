package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

var _ ports.EventLog = (*Log)(nil)

type Log struct {
	path string
	mu   sync.Mutex
}

func New(path string) *Log {
	return &Log{path: path}
}

func (l *Log) Append(ctx context.Context, event session.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l == nil || l.path == "" {
		return errors.New("event log path is not configured")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if event.Version == 0 {
		event.Version = session.EventFormatVersion
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()

	return json.NewEncoder(file).Encode(event)
}

func (l *Log) Stream(ctx context.Context, id session.ID) (<-chan session.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l == nil || l.path == "" {
		return nil, errors.New("event log path is not configured")
	}

	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		ch := make(chan session.Event)
		close(ch)
		return ch, nil
	}
	if err != nil {
		return nil, err
	}

	report, err := session.RecoverLog(bytes.NewReader(data), id)
	if err != nil {
		return nil, err
	}
	out := append([]session.Event(nil), report.Events...)
	if report.MalformedLines > 0 || report.TruncatedTail {
		text := fmt.Sprintf("session log contains %d malformed line(s)", report.MalformedLines)
		if report.TruncatedTail {
			text += " and a truncated trailing record"
		}
		out = append(out, session.Event{
			Version:   session.EventFormatVersion,
			Type:      session.EventError,
			SessionID: id,
			Actor:     "session.log",
			Text:      text,
			Payload: map[string]any{
				"path":            l.path,
				"malformed_lines": report.MalformedLines,
				"truncated_tail":  report.TruncatedTail,
				"errors":          report.Errors,
			},
		})
	}
	ch := make(chan session.Event, len(out))
	for _, event := range out {
		if err := ctx.Err(); err != nil {
			close(ch)
			return ch, nil
		}
		ch <- event
	}
	close(ch)
	return ch, nil
}
