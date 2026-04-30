package jsonl

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luew2/FreeCode/internal/app/workbench"
	"github.com/Luew2/FreeCode/internal/core/session"
)

func TestIndexCreateListFiltersAndOrders(t *testing.T) {
	index := NewIndex(filepath.Join(t.TempDir(), ".freecode", "sessions", "index.json"))
	ctx := context.Background()
	base := time.Unix(100, 0).UTC()

	summaries := []workbench.SessionSummary{
		{
			ID:            "older",
			Title:         "Older",
			WorkspaceRoot: "/workspace/a",
			CreatedAt:     base,
			UpdatedAt:     base.Add(time.Minute),
		},
		{
			ID:            "other-workspace",
			Title:         "Other Workspace",
			WorkspaceRoot: "/workspace/b",
			CreatedAt:     base,
			UpdatedAt:     base.Add(3 * time.Minute),
		},
		{
			ID:            "newer",
			Title:         "Newer",
			WorkspaceRoot: "/workspace/a",
			CreatedAt:     base,
			UpdatedAt:     base.Add(2 * time.Minute),
		},
	}
	for _, summary := range summaries {
		if err := index.Create(ctx, summary); err != nil {
			t.Fatalf("Create returned error: %v", err)
		}
	}

	got, err := index.List(ctx, "/workspace/a")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("sessions = %d, want 2", len(got))
	}
	if got[0].ID != "newer" || got[1].ID != "older" {
		t.Fatalf("sessions ordered as [%s %s], want [newer older]", got[0].ID, got[1].ID)
	}

	all, err := index.List(ctx, "")
	if err != nil {
		t.Fatalf("List all returned error: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all sessions = %d, want 3", len(all))
	}
	if all[0].ID != "other-workspace" || all[1].ID != "newer" || all[2].ID != "older" {
		t.Fatalf("all sessions ordered as [%s %s %s], want [other-workspace newer older]", all[0].ID, all[1].ID, all[2].ID)
	}

	if err := index.Create(ctx, workbench.SessionSummary{
		ID:            "older",
		Title:         "Older Updated",
		WorkspaceRoot: "/workspace/a",
		CreatedAt:     base.Add(10 * time.Minute),
		UpdatedAt:     base.Add(4 * time.Minute),
	}); err != nil {
		t.Fatalf("upsert Create returned error: %v", err)
	}

	updated, err := index.List(ctx, "/workspace/a")
	if err != nil {
		t.Fatalf("List after upsert returned error: %v", err)
	}
	if len(updated) != 2 {
		t.Fatalf("updated sessions = %d, want 2", len(updated))
	}
	if updated[0].ID != "older" {
		t.Fatalf("first session = %s, want older", updated[0].ID)
	}
	if updated[0].Title != "Older Updated" {
		t.Fatalf("updated title = %q, want %q", updated[0].Title, "Older Updated")
	}
	if !updated[0].CreatedAt.Equal(base) {
		t.Fatalf("created_at = %s, want preserved %s", updated[0].CreatedAt, base)
	}
}

func TestIndexRenameUnknown(t *testing.T) {
	index := NewIndex(filepath.Join(t.TempDir(), ".freecode", "sessions", "index.json"))

	err := index.Rename(context.Background(), session.ID("missing"), "Missing")
	if err == nil {
		t.Fatal("Rename returned nil error, want unknown session error")
	}
	if !strings.Contains(err.Error(), `unknown session "missing"`) {
		t.Fatalf("Rename error = %q, want unknown session error", err)
	}
}

func TestIndexRenameSuccess(t *testing.T) {
	index := NewIndex(filepath.Join(t.TempDir(), ".freecode", "sessions", "index.json"))
	ctx := context.Background()
	created := time.Unix(100, 0).UTC()
	updated := created.Add(time.Minute)

	if err := index.Create(ctx, workbench.SessionSummary{
		ID:            "session-1",
		Title:         "Original",
		WorkspaceRoot: "/workspace/a",
		CreatedAt:     created,
		UpdatedAt:     updated,
	}); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	if err := index.Rename(ctx, "session-1", "Renamed"); err != nil {
		t.Fatalf("Rename returned error: %v", err)
	}

	got, err := index.List(ctx, "/workspace/a")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("sessions = %d, want 1", len(got))
	}
	if got[0].Title != "Renamed" {
		t.Fatalf("title = %q, want %q", got[0].Title, "Renamed")
	}
	if !got[0].CreatedAt.Equal(created) {
		t.Fatalf("created_at = %s, want preserved %s", got[0].CreatedAt, created)
	}
	if !got[0].UpdatedAt.After(updated) {
		t.Fatalf("updated_at = %s, want after %s", got[0].UpdatedAt, updated)
	}
}
