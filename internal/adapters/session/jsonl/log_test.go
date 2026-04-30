package jsonl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luew2/FreeCode/internal/core/session"
)

func TestLogStreamSurfacesMalformedLineCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	// Write one valid line, one malformed line, and one more valid line
	// for the same session id. The Stream should emit the valid events
	// and a final EventError announcing the malformed count.
	contents := `{"ID":"e1","SessionID":"s1","Type":"user_message","Actor":"user","Text":"hello"}
{this is not json
{"ID":"e2","SessionID":"s1","Type":"assistant_message","Actor":"assistant","Text":"reply"}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	log := New(path)
	stream, err := log.Stream(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	var got []session.Event
	for event := range stream {
		got = append(got, event)
	}
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3 (2 valid + 1 synthetic error): %#v", len(got), got)
	}
	if got[0].Text != "hello" || got[1].Text != "reply" {
		t.Fatalf("first events = %#v, want hello and reply", got[:2])
	}
	last := got[2]
	if last.Type != session.EventError {
		t.Fatalf("final event type = %q, want %q", last.Type, session.EventError)
	}
	if !strings.Contains(last.Text, "malformed") {
		t.Fatalf("final event text = %q, want mention of malformed lines", last.Text)
	}
	if got, want := last.Payload["malformed_lines"], 1; got != want {
		t.Fatalf("malformed_lines payload = %v, want %v", got, want)
	}
}

func TestLogStreamRecoversTruncatedTrailingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	contents := `{"ID":"e1","SessionID":"s1","Type":"user_message","Actor":"user","Text":"hello"}
{"ID":"e2","SessionID":"s1","Type":"assistant_message"`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	stream, err := New(path).Stream(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	var got []session.Event
	for event := range stream {
		got = append(got, event)
	}
	if len(got) != 2 || got[0].Text != "hello" || got[1].Type != session.EventError {
		t.Fatalf("events = %#v, want valid event plus recovery warning", got)
	}
	if got[1].Payload["truncated_tail"] != true {
		t.Fatalf("payload = %#v, want truncated_tail true", got[1].Payload)
	}
}

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
	if got[0].Version != session.EventFormatVersion || got[1].Version != session.EventFormatVersion {
		t.Fatalf("events = %#v, want current event version", got)
	}
}
