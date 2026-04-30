package transport

import (
	"bytes"
	"net/http"
	"testing"
	"time"
)

func TestDefaultRetryPolicyShape(t *testing.T) {
	policy := DefaultRetryPolicy()
	if policy.MaxAttempts < 2 {
		t.Fatalf("MaxAttempts = %d, want >= 2", policy.MaxAttempts)
	}
	if policy.InitialDelay <= 0 {
		t.Fatalf("InitialDelay = %v, want positive", policy.InitialDelay)
	}
	if policy.MaxDelay <= 0 || policy.MaxDelay < policy.InitialDelay {
		t.Fatalf("MaxDelay = %v, want >= initial delay (%v)", policy.MaxDelay, policy.InitialDelay)
	}
	if policy.Multiplier <= 1 {
		t.Fatalf("Multiplier = %v, want > 1", policy.Multiplier)
	}
}

func TestDrainBodyTrimsAndCaps(t *testing.T) {
	body := bytes.NewBufferString("   hello world   \n")
	got := DrainBody(body, 64)
	if got != "hello world" {
		t.Fatalf("DrainBody = %q, want %q", got, "hello world")
	}

	// Caps are honored — a body longer than the limit returns only the
	// prefix (still trimmed).
	long := bytes.NewBufferString(string(make([]byte, 10)) + "tail")
	if got := DrainBody(long, 4); len(got) > 4 {
		t.Fatalf("DrainBody returned %d bytes, want <= 4", len(got))
	}

	// A non-positive limit falls back to the default.
	if got := DrainBody(bytes.NewBufferString("x"), -1); got != "x" {
		t.Fatalf("DrainBody negative limit = %q, want fallback", got)
	}
}

// keep the time import live for future tests
var _ = time.Second

func TestDefaultHTTPClientHasReasonableTimeouts(t *testing.T) {
	client := DefaultHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.ResponseHeaderTimeout == 0 {
		t.Fatal("ResponseHeaderTimeout = 0, want non-zero")
	}
	if transport.TLSHandshakeTimeout == 0 {
		t.Fatal("TLSHandshakeTimeout = 0, want non-zero")
	}
	if client.Timeout != 0 {
		t.Fatalf("client.Timeout = %v, want 0 (streaming compatible)", client.Timeout)
	}
}
