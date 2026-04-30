package jsonl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				return
			}
			var event session.Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				continue
			}
			if id == "" || event.SessionID == id {
				ch <- event
			}
		}
	}()
	return ch, nil
}
