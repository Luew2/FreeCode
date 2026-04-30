package env

import (
	"context"
	"errors"
	"testing"
)

func TestGetReadsEnvironment(t *testing.T) {
	t.Setenv("FREECODE_TEST_SECRET", "secret-value")

	got, err := New().Get(context.Background(), "FREECODE_TEST_SECRET")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got != "secret-value" {
		t.Fatalf("Get = %q, want secret-value", got)
	}
}

func TestGetMissingEnvironmentReturnsError(t *testing.T) {
	_, err := New().Get(context.Background(), "FREECODE_TEST_MISSING_SECRET")
	if err == nil {
		t.Fatal("Get returned nil error")
	}
}

func TestMutationUnsupported(t *testing.T) {
	store := New()
	if err := store.Set(context.Background(), "KEY", "value"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Set error = %v, want ErrUnsupported", err)
	}
	if err := store.Delete(context.Background(), "KEY"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Delete error = %v, want ErrUnsupported", err)
	}
}
