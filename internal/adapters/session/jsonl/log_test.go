package jsonl

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Luew2/FreeCode/internal/core/session"
)

func TestLogAppendAndStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	log := New(path)
	ctx := context.Background()

	events := []session.Event{
		{ID: "e1", SessionID: "s1", Type: session.EventUserMessage, At: time.Unix(1, 0).UTC(), Actor: "user", Text: "hello"},
		{ID: "e2", SessionID: "s2", Type: session.EventUserMessage, At: time.Unix(2, 0).UTC(), Actor: "user", Text: "other"},
		{ID: "e3", SessionID: "s1", Type: session.EventAssistantMessage, At: time.Unix(3, 0).UTC(), Actor: "assistant", Text: "done"},
	}
	for _, event := range events {
		if err := log.Append(ctx, event); err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
	}

	stream, err := log.Stream(ctx, "s1")
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	var got []session.Event
	for event := range stream {
		got = append(got, event)
	}
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
	if got[0].Text != "hello" || got[1].Text != "done" {
		t.Fatalf("events = %#v, want s1 events", got)
	}
}
