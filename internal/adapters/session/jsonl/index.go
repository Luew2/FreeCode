package jsonl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Luew2/FreeCode/internal/app/workbench"
	"github.com/Luew2/FreeCode/internal/core/session"
)

var _ workbench.SessionIndex = (*Index)(nil)

type Index struct {
	path string
	mu   sync.Mutex
}

func NewIndex(path string) *Index {
	return &Index{path: path}
}

func (i *Index) List(ctx context.Context, workspaceRoot string) ([]workbench.SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i == nil || i.path == "" {
		return nil, errors.New("session index path is not configured")
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	sessions, err := i.read()
	if err != nil {
		return nil, err
	}

	filtered := make([]workbench.SessionSummary, 0, len(sessions))
	for _, summary := range sessions {
		if workspaceRoot != "" && summary.WorkspaceRoot != workspaceRoot {
			continue
		}
		filtered = append(filtered, summary)
	}
	sortSummaries(filtered)
	return filtered, nil
}

func (i *Index) Create(ctx context.Context, summary workbench.SessionSummary) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if i == nil || i.path == "" {
		return errors.New("session index path is not configured")
	}
	if summary.ID == "" {
		return errors.New("session id is required")
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	sessions, err := i.read()
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for idx, existing := range sessions {
		if existing.ID != summary.ID {
			continue
		}
		summary.CreatedAt = existing.CreatedAt
		if summary.CreatedAt.IsZero() {
			summary.CreatedAt = now
		}
		summary.UpdatedAt = nextUpdatedAt(summary.UpdatedAt, existing.UpdatedAt, now)
		sessions[idx] = summary
		sortSummaries(sessions)
		return i.write(sessions)
	}

	if summary.CreatedAt.IsZero() {
		summary.CreatedAt = now
	}
	if summary.UpdatedAt.IsZero() {
		summary.UpdatedAt = summary.CreatedAt
	}
	sessions = append(sessions, summary)
	sortSummaries(sessions)
	return i.write(sessions)
}

func (i *Index) Rename(ctx context.Context, id session.ID, title string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if i == nil || i.path == "" {
		return errors.New("session index path is not configured")
	}
	if id == "" {
		return errors.New("session id is required")
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	sessions, err := i.read()
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for idx := range sessions {
		if sessions[idx].ID != id {
			continue
		}
		sessions[idx].Title = title
		sessions[idx].UpdatedAt = nextUpdatedAt(time.Time{}, sessions[idx].UpdatedAt, now)
		sortSummaries(sessions)
		return i.write(sessions)
	}
	return fmt.Errorf("unknown session %q", id)
}

func (i *Index) read() ([]workbench.SessionSummary, error) {
	data, err := os.ReadFile(i.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}

	var sessions []workbench.SessionSummary
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func (i *Index) write(sessions []workbench.SessionSummary) error {
	if err := os.MkdirAll(filepath.Dir(i.path), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(i.path), ".index-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()

	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(sessions); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, i.path); err != nil {
		return err
	}
	removeTmp = false
	return nil
}

func nextUpdatedAt(candidate, previous, now time.Time) time.Time {
	if candidate.IsZero() {
		candidate = now
	}
	if !previous.IsZero() && !candidate.After(previous) {
		return previous.Add(time.Nanosecond)
	}
	return candidate
}

func sortSummaries(sessions []workbench.SessionSummary) {
	sort.SliceStable(sessions, func(i, j int) bool {
		if !sessions[i].UpdatedAt.Equal(sessions[j].UpdatedAt) {
			return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
		}
		if !sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
		}
		return sessions[i].ID > sessions[j].ID
	})
}
