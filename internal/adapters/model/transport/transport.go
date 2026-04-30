// Package transport provides shared HTTP helpers used by every model
// client adapter: a default HTTP client tuned for streaming workloads
// and a tiny RetryPolicy struct that the openai-go and anthropic-sdk-go
// SDKs do not expose verbatim but the rest of the codebase still relies
// on for configuration.
//
// The retry-orchestrator code that used to live here was removed when
// we migrated to the official SDKs; the retain-only-what-you-need slice
// kept here is what the model-client adapters and probes still call.
package transport

import (
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// RetryPolicy controls how aggressively the model clients retry. Only
// MaxAttempts is honored by the official SDKs (via WithMaxRetries); the
// other knobs are kept for source-compatibility with tests that still
// construct a RetryPolicy literally.
type RetryPolicy struct {
	MaxAttempts  int
	InitialDelay time.Duration
	MaxDelay     time.Duration
	Multiplier   float64
}

// DefaultRetryPolicy returns the policy used when callers do not supply
// one. Four attempts is a tradeoff between user patience and the long
// tail of provider blips that resolve in seconds.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:  4,
		InitialDelay: 500 * time.Millisecond,
		MaxDelay:     8 * time.Second,
		Multiplier:   2.0,
	}
}

// DefaultHTTPClient returns an http.Client suitable for streaming chat
// completions. It avoids a global response timeout (streams can run
// minutes) but still bounds the connection / TLS / response-header
// phases so a stalled server cannot hang the agent forever.
func DefaultHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	return &http.Client{Transport: transport}
}

// DrainBody reads up to limit bytes of an HTTP error body, trimming
// trailing whitespace. Probes use this to surface a useful error
// message without leaking arbitrary payload sizes.
func DrainBody(r io.Reader, limit int64) string {
	if limit <= 0 {
		limit = 4096
	}
	data, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
