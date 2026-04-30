package jsonl

import (
	"bufio"
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

	file, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		ch := make(chan session.Event)
		close(ch)
		return ch, nil
	}
	if err != nil {
		return nil, err
	}

	ch := make(chan session.Event)
	go func() {
		defer close(ch)
		defer file.Close()

		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		var malformed int
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				return
			}
			var event session.Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				// Real corruption (truncated writes, partial flushes,
				// disk damage) used to be invisible — the loop just
				// skipped the line. Count and surface it at the end so
				// readers can act on it instead of seeing a silent gap.
				malformed++
				continue
			}
			if id == "" || event.SessionID == id {
				ch <- event
			}
		}
		// Surface scanner errors and corruption inline as a final
		// EventError. We pick a synthetic event over a separate channel
		// because every existing reader already drains the stream channel
		// and would otherwise have to grow new wiring to learn about
		// errors.
		if err := scanner.Err(); err != nil {
			ch <- session.Event{
				Type:      session.EventError,
				SessionID: id,
				Actor:     "session.log",
				Text:      "session log scan error: " + err.Error(),
				Payload: map[string]any{
					"path":            l.path,
					"malformed_lines": malformed,
				},
			}
			return
		}
		if malformed > 0 {
			ch <- session.Event{
				Type:      session.EventError,
				SessionID: id,
				Actor:     "session.log",
				Text:      fmt.Sprintf("session log contains %d malformed line(s)", malformed),
				Payload: map[string]any{
					"path":            l.path,
					"malformed_lines": malformed,
				},
			}
		}
	}()
	return ch, nil
}
