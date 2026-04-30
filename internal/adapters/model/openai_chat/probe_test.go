package openai_chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/model"
)

func TestProbePostsChatCompletionsRequest(t *testing.T) {
	var gotPath string
	var gotAuthorization string
	var gotBody chatCompletionsProbeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test\",\"choices\":[{\"delta\":{\"content\":\"p\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	provider := model.Provider{
		ID:      "local",
		BaseURL: server.URL,
		Secret:  model.SecretRef{Name: "LOCAL_API_KEY", Source: "env"},
	}
	result, err := NewProbe(staticSecretStore{"LOCAL_API_KEY": "sk-test"}, server.Client()).Probe(context.Background(), provider, "coder")
	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuthorization != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want bearer token", gotAuthorization)
	}
	if gotBody.Model != "coder" {
		t.Fatalf("request model = %q, want coder", gotBody.Model)
	}
	if !gotBody.Stream {
		t.Fatal("request stream = false, want true")
	}
	if result.Protocol != Protocol {
		t.Fatalf("Protocol = %q, want %q", result.Protocol, Protocol)
	}
	if result.Endpoint != server.URL+"/v1/chat/completions" {
		t.Fatalf("Endpoint = %q, want %q", result.Endpoint, server.URL+"/v1/chat/completions")
	}
	if result.Model.Ref != model.NewRef("local", "coder") {
		t.Fatalf("Model.Ref = %q, want local/coder", result.Model.Ref.String())
	}
	if !result.Model.Capabilities.Streaming {
		t.Fatal("Model.Capabilities.Streaming = false, want true")
	}
}

func TestProbeDoesNotDuplicateV1Path(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{}}]}\n\n"))
	}))
	defer server.Close()

	provider := model.Provider{ID: "local", BaseURL: server.URL + "/v1"}
	if _, err := NewProbe(nil, server.Client()).Probe(context.Background(), provider, "coder"); err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
}

func TestChatCompletionsEndpointAcceptsExactEndpoint(t *testing.T) {
	endpoint, err := ChatCompletionsEndpoint("https://example.test/v1/chat/completions")
	if err != nil {
		t.Fatalf("ChatCompletionsEndpoint returned error: %v", err)
	}
	if endpoint != "https://example.test/v1/chat/completions" {
		t.Fatalf("endpoint = %q, want exact chat completions endpoint", endpoint)
	}
}

// TestChatCompletionsEndpointMatchesSDKBaseURL is the regression net for
// finding 6: the probe URL and the SDK-driven client URL MUST be the same
// for every reasonable provider base URL. Previously the two functions had
// independent path-collapse logic, which sent the probe to
// "/api/coding/paas/v4/v1/chat/completions" while the SDK client called
// "/api/coding/paas/v4/chat/completions" — making probe results disagree
// with live calls for Z.ai-style versioned bases.
func TestChatCompletionsEndpointMatchesSDKBaseURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{
			name: "host_only",
			base: "https://api.openai.com",
			want: "https://api.openai.com/v1/chat/completions",
		},
		{
			name: "host_with_trailing_slash",
			base: "https://api.openai.com/",
			want: "https://api.openai.com/v1/chat/completions",
		},
		{
			name: "v1_no_trailing_slash",
			base: "https://api.openai.com/v1",
			want: "https://api.openai.com/v1/chat/completions",
		},
		{
			name: "v1_with_trailing_slash",
			base: "https://api.openai.com/v1/",
			want: "https://api.openai.com/v1/chat/completions",
		},
		{
			name: "exact_endpoint",
			base: "https://api.openai.com/v1/chat/completions",
			want: "https://api.openai.com/v1/chat/completions",
		},
		{
			name: "versioned_v4_root",
			base: "https://api.foo.com/v4",
			want: "https://api.foo.com/v4/chat/completions",
		},
		{
			name: "versioned_v4_root_with_trailing_slash",
			base: "https://api.foo.com/v4/",
			want: "https://api.foo.com/v4/chat/completions",
		},
		{
			name: "z_ai_style_versioned_prefix",
			base: "https://api.z.ai/api/coding/paas/v4",
			want: "https://api.z.ai/api/coding/paas/v4/chat/completions",
		},
		{
			name: "z_ai_style_with_trailing_slash",
			base: "https://api.z.ai/api/coding/paas/v4/",
			want: "https://api.z.ai/api/coding/paas/v4/chat/completions",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			probeURL, err := ChatCompletionsEndpoint(tc.base)
			if err != nil {
				t.Fatalf("ChatCompletionsEndpoint(%q) returned error: %v", tc.base, err)
			}
			sdkBase, err := sdkBaseURL(tc.base)
			if err != nil {
				t.Fatalf("sdkBaseURL(%q) returned error: %v", tc.base, err)
			}
			// The SDK appends the relative path "chat/completions" to the
			// base URL it was configured with. The base must end with a
			// trailing slash for that concatenation to land on the right
			// endpoint.
			if !strings.HasSuffix(sdkBase, "/") {
				t.Fatalf("sdkBaseURL(%q) = %q, want trailing slash", tc.base, sdkBase)
			}
			sdkEffective := sdkBase + "chat/completions"
			if probeURL != sdkEffective {
				t.Fatalf("base %q: probe URL = %q, SDK-effective URL = %q (must match)", tc.base, probeURL, sdkEffective)
			}
			if probeURL != tc.want {
				t.Fatalf("base %q: probe URL = %q, want %q", tc.base, probeURL, tc.want)
			}
		})
	}
}

func TestProbeRejectsEmptySuccessBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(""))
	}))
	defer server.Close()

	provider := model.Provider{ID: "local", BaseURL: server.URL}
	_, err := NewProbe(nil, server.Client()).Probe(context.Background(), provider, "coder")
	if err == nil {
		t.Fatal("Probe returned nil error")
	}
	if !strings.Contains(err.Error(), "did not contain data frames") {
		t.Fatalf("error = %q, want missing data frame message", err.Error())
	}
}

func TestProbeRejectsNonStreamingSuccessBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{}]}`))
	}))
	defer server.Close()

	provider := model.Provider{ID: "local", BaseURL: server.URL}
	_, err := NewProbe(nil, server.Client()).Probe(context.Background(), provider, "coder")
	if err == nil {
		t.Fatal("Probe returned nil error")
	}
	if !strings.Contains(err.Error(), "expected streamed chat completion data") {
		t.Fatalf("error = %q, want streaming shape message", err.Error())
	}
}

func TestProbeStatusErrorIncludesProtocolEndpointAndStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	provider := model.Provider{ID: "local", BaseURL: server.URL}
	_, err := NewProbe(nil, server.Client()).Probe(context.Background(), provider, "coder")
	if err == nil {
		t.Fatal("Probe returned nil error")
	}
	message := err.Error()
	for _, want := range []string{"openai-chat", server.URL + "/v1/chat/completions", "status 401"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not contain %q", message, want)
		}
	}
}

type staticSecretStore map[string]string

func (s staticSecretStore) Get(ctx context.Context, name string) (string, error) {
	value, ok := s[name]
	if !ok {
		return "", errors.New("missing secret")
	}
	return value, nil
}

func (s staticSecretStore) Set(ctx context.Context, name string, value string) error {
	return errors.New("unsupported")
}

func (s staticSecretStore) Delete(ctx context.Context, name string) error {
	return errors.New("unsupported")
}
