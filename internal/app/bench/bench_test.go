package bench

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunDefaultBenchmarksPass(t *testing.T) {
	results, err := Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(results) != len(DefaultTasks()) {
		t.Fatalf("results = %d, want %d", len(results), len(DefaultTasks()))
	}
	if !AllPassed(results) {
		t.Fatalf("results = %#v, want all pass", results)
	}
}

func TestRunSelectsTask(t *testing.T) {
	called := false
	results, err := Run(context.Background(), Options{
		Task: "selected",
		Tasks: []Task{
			{Name: "skip", Run: func(context.Context) error { t.Fatal("skip task ran"); return nil }},
			{Name: "selected", Run: func(context.Context) error {
				called = true
				return nil
			}},
		},
		Now: tickingClock(time.Unix(1, 0), time.Millisecond),
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatalf("selected task did not run")
	}
	if len(results) != 1 || results[0].TaskName != "selected" || results[0].Status != StatusPass {
		t.Fatalf("results = %#v, want selected pass", results)
	}
	if results[0].Duration != time.Millisecond {
		t.Fatalf("duration = %s, want 1ms", results[0].Duration)
	}
}

func TestRunCapturesFailuresAndContinues(t *testing.T) {
	results, err := Run(context.Background(), Options{
		Tasks: []Task{
			{Name: "fail", Run: func(context.Context) error { return errors.New("boom") }},
			{Name: "pass", Run: func(context.Context) error { return nil }},
		},
		Now: tickingClock(time.Unix(1, 0), time.Millisecond),
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Status != StatusFail || results[0].Error != "boom" {
		t.Fatalf("first result = %#v, want captured failure", results[0])
	}
	if results[1].Status != StatusPass {
		t.Fatalf("second result = %#v, want pass", results[1])
	}
	if AllPassed(results) {
		t.Fatalf("AllPassed returned true for failed results")
	}
}

func TestRunUnknownTaskErrors(t *testing.T) {
	_, err := Run(context.Background(), Options{
		Task:  "missing",
		Tasks: []Task{{Name: "known", Run: func(context.Context) error { return nil }}},
	})
	if err == nil || !strings.Contains(err.Error(), `unknown benchmark task "missing"`) {
		t.Fatalf("err = %v, want unknown task", err)
	}
}

func TestFormatResults(t *testing.T) {
	results := []Result{
		{TaskName: "pass-task", Status: StatusPass, Duration: time.Millisecond},
		{TaskName: "fail-task", Status: StatusFail, Duration: 2 * time.Millisecond, Error: "boom"},
	}
	var out bytes.Buffer
	if err := FormatResults(&out, results); err != nil {
		t.Fatalf("FormatResults returned error: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"freecode bench\n",
		"PASS pass-task 1ms\n",
		"FAIL fail-task 2ms boom\n",
		"FAIL 1/2\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
}

func tickingClock(start time.Time, step time.Duration) func() time.Time {
	current := start.Add(-step)
	return func() time.Time {
		current = current.Add(step)
		return current
	}
}
