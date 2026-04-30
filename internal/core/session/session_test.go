package session

import (
	"strings"
	"testing"
	"time"
)

func TestRecoverLogSkipsTruncatedTrailingRecord(t *testing.T) {
	input := `{"Version":1,"ID":"e1","SessionID":"s1","Type":"user_message","At":"2026-01-01T00:00:00Z","Actor":"user","Text":"hello"}` + "\n" +
		`{"Version":1,"ID":"e2","SessionID":"s1","Type":"assistant_message"`
	report, err := RecoverLog(strings.NewReader(input), "s1")
	if err != nil {
		t.Fatalf("RecoverLog returned error: %v", err)
	}
	if len(report.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(report.Events))
	}
	if !report.TruncatedTail || report.MalformedLines != 0 {
		t.Fatalf("report = %#v, want truncated tail only", report)
	}
}

func TestRebuildIndexBuildsSessionsFromEvents(t *testing.T) {
	events := []Event{
		{SessionID: "s1", Type: EventUserMessage, Text: "first task", At: time.Unix(1, 0).UTC()},
		{SessionID: "s1", Type: EventAssistantMessage, Text: "done", At: time.Unix(3, 0).UTC()},
		{SessionID: "s2", Type: EventUserMessage, Text: "other task", At: time.Unix(2, 0).UTC()},
	}
	sessions := RebuildIndex(events)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %#v, want 2", sessions)
	}
	byID := map[ID]Session{}
	for _, item := range sessions {
		byID[item.ID] = item
	}
	if byID["s1"].Title != "first task" || !byID["s1"].UpdatedAt.Equal(time.Unix(3, 0).UTC()) {
		t.Fatalf("s1 = %#v, want title and latest update", byID["s1"])
	}
}
