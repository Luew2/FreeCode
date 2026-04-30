package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	coremodel "github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/session"
)

func TestProviderAddSkipProbeWritesConfigWithoutAPIKeyValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".freecode", "config.toml")
	t.Setenv("LOCAL_API_KEY", "sk-test-secret")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{
		"provider", "add",
		"--name", "local",
		"--base-url", "http://example.test/v1",
		"--api-key-env", "LOCAL_API_KEY",
		"--model", "coder",
		"--protocol", "openai-chat",
		"--context-window", "128000",
		"--max-output-tokens", "8192",
		"--config", path,
		"--skip-probe",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run exit = %d, stderr = %q", code, stderr.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "LOCAL_API_KEY") {
		t.Fatalf("config content does not contain env var name: %s", content)
	}
	if strings.Contains(content, "sk-test-secret") {
		t.Fatalf("config content contains API key value: %s", content)
	}
	if !strings.Contains(content, "context_window = 128000") || !strings.Contains(content, "max_output_tokens = 8192") {
		t.Fatalf("config content = %s, want model limits", content)
	}
}

func TestMetadataEventLogTagsDirectedAgentEvents(t *testing.T) {
	inner := &recordingEventLog{}
	log := metadataEventLog{
		inner: inner,
		metadata: map[string]any{
			"agent_id":     "a1",
			"task_session": "task-1",
			"role":         "worker",
		},
	}

	err := log.Append(context.Background(), session.Event{
		ID:      "e1",
		Type:    session.EventUserMessage,
		Actor:   "user",
		Text:    "continue",
		Payload: map[string]any{"step": 1},
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if len(inner.events) != 1 {
		t.Fatalf("events = %#v, want one event", inner.events)
	}
	payload := inner.events[0].Payload
	if payload["agent_id"] != "a1" || payload["task_session"] != "task-1" || payload["role"] != "worker" || payload["step"] != 1 {
		t.Fatalf("payload = %#v, want directed metadata plus original step", payload)
	}
}

func TestDelegatingToolsAdvertisesSpawnAgent(t *testing.T) {
	tools := delegatingTools{}
	specs := tools.Tools()
	found := false
	for _, spec := range specs {
		if spec.Name == "spawn_agent" {
			found = true
			if !strings.Contains(spec.Description, "subagent") {
				t.Fatalf("spawn_agent description = %q, want subagent language", spec.Description)
			}
		}
	}
	if !found {
		t.Fatalf("tools = %#v, want spawn_agent", specs)
	}
}

func TestDelegatingToolsHideSpawnAgentAtDepthLimit(t *testing.T) {
	tools := delegatingTools{depth: 2, maxDepth: 2}
	for _, spec := range tools.Tools() {
		if spec.Name == "spawn_agent" {
			t.Fatalf("tools = %#v, want no spawn_agent at depth limit", tools.Tools())
		}
	}
	_, err := tools.RunTool(context.Background(), coremodel.ToolCall{ID: "call_1", Name: "spawn_agent", Arguments: []byte(`{"role":"explorer","task":"read"}`)})
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("RunTool error = %v, want depth limit", err)
	}
}

func TestSwarmDelegationContextAsksMainToSpawnDynamicAgents(t *testing.T) {
	prompt := swarmDelegationContext("auto", nil)
	if !strings.Contains(prompt, "use spawn_agent") ||
		!strings.Contains(prompt, "Do not use a fixed agent count") ||
		!strings.Contains(prompt, "as many bounded tasks as the request actually needs") ||
		!strings.Contains(prompt, "orchestrator child agents") {
		t.Fatalf("context = %q, want dynamic main-owned swarm instructions", prompt)
	}
	// The user goal must NOT appear here — that ships separately as the
	// user message. Folding it in was the bug that made the full
	// scaffolding render as a user prompt in chat.
	if strings.Contains(prompt, "ship feature") {
		t.Fatalf("context contains user goal verbatim; should be turn-context only:\n%s", prompt)
	}
}

func TestAskUsesConfiguredOpenAICompatibleProvider(t *testing.T) {
	var gotAuthorization string
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode request body: %v", err)
		}
		if body["stream"] != true {
			t.Fatalf("stream = %#v, want true", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), ".freecode", "config.toml")
	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	t.Setenv("LOCAL_API_KEY", "sk-test")

	var addOut bytes.Buffer
	var addErr bytes.Buffer
	code := Run([]string{
		"provider", "add",
		"--name", "local",
		"--base-url", server.URL,
		"--api-key-env", "LOCAL_API_KEY",
		"--model", "coder",
		"--protocol", "openai-chat",
		"--config", path,
		"--skip-probe",
	}, &addOut, &addErr)
	if code != 0 {
		t.Fatalf("provider add exit = %d, stderr = %q", code, addErr.String())
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code = Run([]string{"ask", "--config", path, "--session", sessionPath, "say", "hi"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ask exit = %d, stderr = %q", code, stderr.String())
	}
	if stdout.String() != "hello\n" {
		t.Fatalf("stdout = %q, want hello", stdout.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuthorization != "Bearer sk-test" {
		t.Fatalf("authorization = %q, want bearer token", gotAuthorization)
	}
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session log: %v", err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Fatalf("session log = %q, want model event", string(data))
	}
}

func TestAskUsesConfiguredAnthropicCompatibleProvider(t *testing.T) {
	var gotKey string
	var gotPath string
	var gotBody struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Api-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		writeSSEEvent(t, w, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "anthropic ok"}})
		writeSSEEvent(t, w, map[string]any{"type": "content_block_stop", "index": 0})
		writeSSEEvent(t, w, map[string]any{"type": "message_stop"})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), ".freecode", "config.toml")
	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	t.Setenv("ANTHROPIC_TEST_KEY", "anthropic-secret")

	var addOut bytes.Buffer
	var addErr bytes.Buffer
	code := Run([]string{
		"provider", "add",
		"--name", "anthropic",
		"--base-url", server.URL,
		"--api-key-env", "ANTHROPIC_TEST_KEY",
		"--model", "claude",
		"--protocol", "anthropic-messages",
		"--config", path,
		"--max-output-tokens", "9",
		"--skip-probe",
	}, &addOut, &addErr)
	if code != 0 {
		t.Fatalf("provider add exit = %d, stderr = %q", code, addErr.String())
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code = Run([]string{"ask", "--config", path, "--session", sessionPath, "say", "hi"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ask exit = %d, stderr = %q", code, stderr.String())
	}
	if stdout.String() != "anthropic ok\n" {
		t.Fatalf("stdout = %q, want anthropic ok", stdout.String())
	}
	if gotPath != "/v1/messages" || gotKey != "anthropic-secret" {
		t.Fatalf("path/key = %q/%q, want Anthropic-compatible request", gotPath, gotKey)
	}
	if gotBody.Model != "claude" || gotBody.MaxTokens != 9 {
		t.Fatalf("body = %#v, want configured model and max tokens", gotBody)
	}
}

func TestProviderAutoProbeFallsBackToAnthropicMessages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"claude"}]}`)
		case "/v1/chat/completions":
			http.Error(w, "not here", http.StatusNotFound)
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"pong"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), ".freecode", "config.toml")
	t.Setenv("LOCAL_API_KEY", "secret")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{
		"provider", "add",
		"--name", "auto",
		"--base-url", server.URL,
		"--api-key-env", "LOCAL_API_KEY",
		"--model", "claude",
		"--protocol", "auto",
		"--config", path,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provider add exit = %d, stderr = %q", code, stderr.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile config: %v", err)
	}
	if !strings.Contains(string(data), "protocol = 'anthropic-messages'") {
		t.Fatalf("config = %q, want anthropic protocol", string(data))
	}
	if !strings.Contains(string(data), "models_endpoint") || !strings.Contains(string(data), "models = 'claude'") {
		t.Fatalf("config = %q, want /models discovery metadata", string(data))
	}
}

func TestAskCanRunReadToolLoop(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("FreeCode repo\n"), 0o600); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		switch requests {
		case 1:
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"README.md\\\"}\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case 2:
			var body struct {
				Messages []struct {
					Role       string `json:"role"`
					Content    string `json:"content"`
					ToolCallID string `json:"tool_call_id"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("Decode second request: %v", err)
			}
			last := body.Messages[len(body.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "call_1" || !strings.Contains(last.Content, "FreeCode repo") {
				t.Fatalf("last message = %#v, want read_file tool result", last)
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"read it\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	path := filepath.Join(root, ".freecode", "config.toml")
	sessionPath := filepath.Join(root, "session.jsonl")
	t.Setenv("LOCAL_API_KEY", "sk-test")

	var addOut bytes.Buffer
	var addErr bytes.Buffer
	code := Run([]string{
		"provider", "add",
		"--name", "local",
		"--base-url", server.URL,
		"--api-key-env", "LOCAL_API_KEY",
		"--model", "coder",
		"--protocol", "openai-chat",
		"--config", path,
		"--skip-probe",
	}, &addOut, &addErr)
	if code != 0 {
		t.Fatalf("provider add exit = %d, stderr = %q", code, addErr.String())
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code = Run([]string{"ask", "--config", path, "--session", sessionPath, "read", "README"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ask exit = %d, stderr = %q", code, stderr.String())
	}
	if stdout.String() != "read it\n" {
		t.Fatalf("stdout = %q, want read it", stdout.String())
	}
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "read_file") || !strings.Contains(log, "FreeCode repo") {
		t.Fatalf("session log = %q, want tool call and result", log)
	}
}

func TestAskDefaultRejectsWriteToolCalls(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello repo\n"), 0o600); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		switch requests {
		case 1:
			var body struct {
				Tools []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("Decode first request: %v", err)
			}
			for _, tool := range body.Tools {
				if tool.Function.Name == "apply_patch" {
					t.Fatalf("read-only ask advertised apply_patch")
				}
			}
			writeSSEEvent(t, w, map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": 0,
							"id":    "call_1",
							"type":  "function",
							"function": map[string]any{
								"name":      "apply_patch",
								"arguments": `{"changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`,
							},
						}},
					},
				}},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case 2:
			var body struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("Decode second request: %v", err)
			}
			last := body.Messages[len(body.Messages)-1]
			if last.Role != "tool" || !strings.Contains(last.Content, "unknown tool") {
				t.Fatalf("last message = %#v, want unknown tool error", last)
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"blocked\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	path := filepath.Join(root, ".freecode", "config.toml")
	sessionPath := filepath.Join(root, "session.jsonl")
	t.Setenv("LOCAL_API_KEY", "sk-test")
	addProviderForTest(t, server.URL, path)

	withChdir(t, root)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"ask", "--config", path, "--session", sessionPath, "patch", "README"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ask exit = %d, stderr = %q", code, stderr.String())
	}
	if stdout.String() != "blocked\n" {
		t.Fatalf("stdout = %q, want blocked", stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile README: %v", err)
	}
	if string(data) != "hello repo\n" {
		t.Fatalf("content = %q, read-only ask overwrote file", string(data))
	}
}

func TestAskAllowWritesCanApplyPatchAndLogResult(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello repo\n"), 0o600); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}

	requests := 0
	previewToken := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		switch requests {
		case 1:
			var body struct {
				Tools []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("Decode first request: %v", err)
			}
			foundApplyPatch := false
			for _, tool := range body.Tools {
				if tool.Function.Name == "apply_patch" {
					foundApplyPatch = true
				}
			}
			if !foundApplyPatch {
				t.Fatalf("allow-writes ask did not advertise apply_patch: %#v", body.Tools)
			}
			writeSSEEvent(t, w, map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": 0,
							"id":    "call_1",
							"type":  "function",
							"function": map[string]any{
								"name":      "apply_patch",
								"arguments": `{"summary":"update readme","changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`,
							},
						}},
					},
				}},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case 2:
			var body struct {
				Messages []struct {
					Role       string `json:"role"`
					Content    string `json:"content"`
					ToolCallID string `json:"tool_call_id"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("Decode second request: %v", err)
			}
			last := body.Messages[len(body.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "call_1" || !strings.Contains(last.Content, "preview patch p1") {
				t.Fatalf("last message = %#v, want apply_patch preview", last)
			}
			previewToken = extractPreviewToken(t, last.Content)
			writeSSEEvent(t, w, map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": 0,
							"id":    "call_2",
							"type":  "function",
							"function": map[string]any{
								"name":      "apply_patch",
								"arguments": `{"summary":"update readme","accepted":true,"preview_token":"` + previewToken + `","changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`,
							},
						}},
					},
				}},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case 3:
			var body struct {
				Messages []struct {
					Role       string `json:"role"`
					Content    string `json:"content"`
					ToolCallID string `json:"tool_call_id"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("Decode third request: %v", err)
			}
			last := body.Messages[len(body.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "call_2" || !strings.Contains(last.Content, "applied patch p2") {
				t.Fatalf("last message = %#v, want accepted apply_patch result", last)
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	path := filepath.Join(root, ".freecode", "config.toml")
	sessionPath := filepath.Join(root, "session.jsonl")
	t.Setenv("LOCAL_API_KEY", "sk-test")
	addProviderForTest(t, server.URL, path)

	withChdir(t, root)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"ask", "--config", path, "--session", sessionPath, "--allow-writes", "patch", "README"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ask exit = %d, stderr = %q", code, stderr.String())
	}
	if stdout.String() != "done\n" {
		t.Fatalf("stdout = %q, want done", stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile README: %v", err)
	}
	if string(data) != "hello FreeCode\n" {
		t.Fatalf("content = %q, want patched README", string(data))
	}
	logData, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session log: %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "apply_patch") || !strings.Contains(log, "preview patch p1") || !strings.Contains(log, "applied patch p2") || !strings.Contains(log, "--- a/README.md") {
		t.Fatalf("session log = %q, want preview and applied patch tool events with diff body", log)
	}
}

func TestProviderListShowsConfiguredProviderAndModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".freecode", "config.toml")

	var addOut bytes.Buffer
	var addErr bytes.Buffer
	code := Run([]string{
		"provider", "add",
		"--name", "local",
		"--base-url", "http://example.test/v1",
		"--api-key-env", "LOCAL_API_KEY",
		"--model", "coder",
		"--protocol", "openai-chat",
		"--config", path,
		"--skip-probe",
	}, &addOut, &addErr)
	if code != 0 {
		t.Fatalf("provider add exit = %d, stderr = %q", code, addErr.String())
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code = Run([]string{"provider", "list", "--config", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provider list exit = %d, stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "local") || !strings.Contains(output, "coder") {
		t.Fatalf("output = %q, want provider and model", output)
	}
}

func TestTUICommandRendersWorkbenchAndApprovalMode(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".freecode", "config.toml")
	addProviderForTest(t, "http://example.test/v1", configPath)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := RunWithIO(
		[]string{"tui", "--approval", "read-only", "--config", configPath, "--session", filepath.Join(root, "session.jsonl")},
		strings.NewReader("ctrl-a\nq\n"),
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("tui exit = %d, stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "Sessions / Agents") || !strings.Contains(output, "Context / Artifacts") {
		t.Fatalf("output = %q, want workbench panes", output)
	}
	if !strings.Contains(output, "approval:ask") {
		t.Fatalf("output = %q, want cycled approval mode", output)
	}
}

func TestTUIDefaultLaunchCreatesIndexedSession(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".freecode", "config.toml")
	addProviderForTest(t, "http://example.test/v1", configPath)
	withChdir(t, root)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := RunWithIO(
		[]string{"tui", "--plain", "--approval", "read-only", "--config", configPath},
		strings.NewReader("q\n"),
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("tui exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Sessions / Agents") {
		t.Fatalf("stdout = %q, want plain workbench output", stdout.String())
	}

	indexPath := filepath.Join(root, ".freecode", "sessions", "index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("ReadFile index: %v", err)
	}
	var summaries []struct {
		ID            string `json:"id"`
		WorkspaceRoot string `json:"workspace_root"`
		LogPath       string `json:"log_path"`
	}
	if err := json.Unmarshal(data, &summaries); err != nil {
		t.Fatalf("Unmarshal index: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %#v, want one session", summaries)
	}
	if summaries[0].ID == "" || summaries[0].ID == "default" {
		t.Fatalf("session id = %q, want generated non-default id", summaries[0].ID)
	}
	workspaceRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks root: %v", err)
	}
	if summaries[0].WorkspaceRoot != workspaceRoot {
		t.Fatalf("workspace root = %q, want %q", summaries[0].WorkspaceRoot, workspaceRoot)
	}
	if filepath.Base(summaries[0].LogPath) != summaries[0].ID+".jsonl" {
		t.Fatalf("log path = %q, want id-based jsonl path", summaries[0].LogPath)
	}
	logPath := summaries[0].LogPath
	if !filepath.IsAbs(logPath) {
		logPath = filepath.Join(root, logPath)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("Stat session log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".freecode", "sessions", "latest.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("latest.jsonl exists or stat failed unexpectedly: %v", err)
	}
}

func TestTopLevelPlainFlagLaunchesFallbackTUI(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".freecode", "config.toml")
	addProviderForTest(t, "http://example.test/v1", configPath)
	withChdir(t, root)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := RunWithIO(
		[]string{"--plain", "--approval", "read-only", "--config", configPath},
		strings.NewReader("q\n"),
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("plain exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Sessions / Agents") {
		t.Fatalf("stdout = %q, want plain workbench output", stdout.String())
	}
}

func TestStatusAndDiffCommandsInspectGitChanges(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "branch", "-M", "main")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello repo\n"), 0o600); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	runGit(t, root, "add", "README.md")
	runGit(t, root, "-c", "user.name=FreeCode", "-c", "user.email=freecode@example.test", "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello FreeCode\n"), 0o600); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	withChdir(t, root)

	var statusOut bytes.Buffer
	var statusErr bytes.Buffer
	code := Run([]string{"status"}, &statusOut, &statusErr)
	if code != 0 {
		t.Fatalf("status exit = %d, stderr = %q", code, statusErr.String())
	}
	if !strings.Contains(statusOut.String(), "branch: main") || !strings.Contains(statusOut.String(), "README.md") {
		t.Fatalf("status output = %q, want branch and changed README", statusOut.String())
	}

	var diffOut bytes.Buffer
	var diffErr bytes.Buffer
	code = Run([]string{"diff", "README.md"}, &diffOut, &diffErr)
	if code != 0 {
		t.Fatalf("diff exit = %d, stderr = %q", code, diffErr.String())
	}
	if !strings.Contains(diffOut.String(), "-hello repo") || !strings.Contains(diffOut.String(), "+hello FreeCode") {
		t.Fatalf("diff output = %q, want README diff", diffOut.String())
	}
}

func TestBenchCommandRunsCIWorkflowBenchmarks(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"bench"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("bench exit = %d, stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"freecode bench",
		"PASS add-provider",
		"PASS copy-code-block",
		"PASS open-changed-file",
		"PASS review-patch",
		"PASS toggle-auto-approval",
		"PASS swarm-mocked-agents",
		"PASS mocked-openai-provider",
		"PASS recover-failed-edit",
		"PASS 8/8",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestBenchHelpExitsZero(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"bench", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("bench help exit = %d, stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"usage: freecode bench",
		"--task",
		"mocked-openai-provider",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestBenchCommandCanRunSingleTaskAndRejectsUnknownTask(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"bench", "--task", "copy-code-block"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("bench task exit = %d, stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "PASS copy-code-block") || !strings.Contains(output, "PASS 1/1") {
		t.Fatalf("output = %q, want selected task only", output)
	}
	if strings.Contains(output, "add-provider") {
		t.Fatalf("output = %q, did not expect unselected benchmark", output)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"bench", "--task", "missing"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("unknown bench exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown benchmark task "missing"`) {
		t.Fatalf("stderr = %q, want unknown task", stderr.String())
	}
}

func TestCompactCommandAppendsSessionCheckpoint(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	initial := `{"ID":"e1","SessionID":"default","Type":"user_message","Actor":"user","Text":"hello compact"}` + "\n"
	if err := os.WriteFile(sessionPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("WriteFile session: %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"compact", "--session", sessionPath, "--session-id", "default", "--max-tokens", "128"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("compact exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "compacted session default") {
		t.Fatalf("stdout = %q, want compact message", stdout.String())
	}
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}
	if !strings.Contains(string(data), "context_compacted") || !strings.Contains(string(data), "hello compact") {
		t.Fatalf("session log = %q, want compact checkpoint", string(data))
	}
}

func TestSwarmAutoCanApplyWorkerPatchThroughFakeProvider(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("old\n"), 0o600); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module swarmtest\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatalf("WriteFile go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "swarmtest.go"), []byte("package swarmtest\n"), 0o600); err != nil {
		t.Fatalf("WriteFile swarmtest.go: %v", err)
	}
	configPath := filepath.Join(root, ".freecode", "config.toml")
	sessionPath := filepath.Join(root, "session.jsonl")
	t.Setenv("LOCAL_API_KEY", "sk-test")

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode request %d: %v", requests, err)
		}
		requestText := ""
		for _, message := range body.Messages {
			requestText += "\n" + message.Content
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch {
		case strings.Contains(requestText, `"role": "orchestrator"`):
			writeTextSSE(t, w, `{"status":"completed","summary":"plan ready"}`)
		case strings.Contains(requestText, `"role": "explorer"`):
			writeTextSSE(t, w, `{"status":"completed","summary":"context ready"}`)
		case strings.Contains(requestText, `"role": "worker"`) && !strings.Contains(requestText, "preview patch"):
			if !strings.Contains(requestText, `"allowed_paths"`) || strings.Contains(requestText, "Do not implement writes") {
				t.Fatalf("worker request missing task packet or has read-only prompt: %s", requestText)
			}
			writeSSEEvent(t, w, map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": 0,
							"id":    "call_preview",
							"type":  "function",
							"function": map[string]any{
								"name":      "apply_patch",
								"arguments": `{"summary":"edit","changes":[{"path":"README.md","old_text":"old\n","new_text":"new\n"}]}`,
							},
						}},
					},
				}},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case strings.Contains(requestText, `"role": "worker"`) && strings.Contains(requestText, "applied patch"):
			writeTextSSE(t, w, `{"status":"completed","summary":"changed README","changed_files":["README.md"]}`)
		case strings.Contains(requestText, `"role": "worker"`) && strings.Contains(requestText, "preview patch"):
			previewToken := extractPreviewToken(t, requestText)
			writeSSEEvent(t, w, map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": 0,
							"id":    "call_apply",
							"type":  "function",
							"function": map[string]any{
								"name":      "apply_patch",
								"arguments": `{"summary":"edit","accepted":true,"preview_token":"` + previewToken + `","changes":[{"path":"README.md","old_text":"old\n","new_text":"new\n"}]}`,
							},
						}},
					},
				}},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case strings.Contains(requestText, `"role": "verifier"`) && strings.Contains(requestText, "check passed: go test ./..."):
			writeTextSSE(t, w, `{"status":"completed","summary":"verified","tests_run":["go test ./..."]}`)
		case strings.Contains(requestText, `"role": "verifier"`):
			if !strings.Contains(requestText, "changed README") || !strings.Contains(requestText, "run_check") {
				t.Fatalf("verifier request missing worker handoff or run_check tool: %s", requestText)
			}
			writeSSEEvent(t, w, map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": 0,
							"id":    "call_check",
							"type":  "function",
							"function": map[string]any{
								"name":      "run_check",
								"arguments": `{"command":"go test ./..."}`,
							},
						}},
					},
				}},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case strings.Contains(requestText, `"role": "reviewer"`):
			if !strings.Contains(requestText, "go test ./...") || !strings.Contains(requestText, "README.md") {
				t.Fatalf("reviewer request missing verifier/worker handoff: %s", requestText)
			}
			writeTextSSE(t, w, `{"status":"completed","summary":"review ok","findings":[]}`)
		default:
			t.Fatalf("unexpected request %d: %s", requests, requestText)
		}
	}))
	defer server.Close()
	addProviderForTest(t, server.URL, configPath)
	withChdir(t, root)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"swarm", "--config", configPath, "--session", sessionPath, "--approval", "auto", "edit README"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("swarm exit = %d, stderr = %q", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile README: %v", err)
	}
	if string(data) != "new\n" {
		t.Fatalf("README = %q, want patched content", string(data))
	}
	logData, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session log: %v", err)
	}
	if !strings.Contains(string(logData), `"agent":"worker"`) || !strings.Contains(string(logData), "applied patch") {
		t.Fatalf("session log = %q, want worker patch trace", string(logData))
	}
	if !strings.Contains(stdout.String(), "swarm completed") {
		t.Fatalf("stdout = %q, want completed swarm", stdout.String())
	}
}

func addProviderForTest(t *testing.T, serverURL string, path string) {
	t.Helper()
	var addOut bytes.Buffer
	var addErr bytes.Buffer
	code := Run([]string{
		"provider", "add",
		"--name", "local",
		"--base-url", serverURL,
		"--api-key-env", "LOCAL_API_KEY",
		"--model", "coder",
		"--protocol", "openai-chat",
		"--config", path,
		"--skip-probe",
	}, &addOut, &addErr)
	if code != 0 {
		t.Fatalf("provider add exit = %d, stderr = %q", code, addErr.String())
	}
}

func withChdir(t *testing.T, dir string) {
	t.Helper()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
}

// writeSSEEvent emits a server-sent event. When the value carries a
// top-level "type" field (Anthropic stream events), we mirror it as the
// SSE `event:` field so the SDK's typed dispatcher can route it.
func writeSSEEvent(t *testing.T, w io.Writer, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal SSE event: %v", err)
	}
	if eventType, ok := extractSSEEventType(value); ok {
		_, _ = io.WriteString(w, "event: ")
		_, _ = io.WriteString(w, eventType)
		_, _ = io.WriteString(w, "\n")
	}
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(data)
	_, _ = io.WriteString(w, "\n\n")
}

func extractSSEEventType(value any) (string, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return "", false
	}
	t, ok := m["type"].(string)
	if !ok || t == "" {
		return "", false
	}
	switch t {
	case "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop", "ping", "completion":
		return t, true
	}
	return "", false
}

func writeTextSSE(t *testing.T, w io.Writer, text string) {
	t.Helper()
	writeSSEEvent(t, w, map[string]any{
		"choices": []map[string]any{{
			"delta": map[string]any{"content": text},
		}},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func extractPreviewToken(t *testing.T, content string) string {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if token, ok := strings.CutPrefix(strings.TrimSpace(line), "preview_token: "); ok {
			if token == "" {
				t.Fatalf("empty preview token in content %q", content)
			}
			return token
		}
	}
	t.Fatalf("content %q does not contain preview token", content)
	return ""
}

type recordingEventLog struct {
	events []session.Event
}

func (l *recordingEventLog) Append(ctx context.Context, event session.Event) error {
	l.events = append(l.events, event)
	return nil
}

func (l *recordingEventLog) Stream(ctx context.Context, id session.ID) (<-chan session.Event, error) {
	ch := make(chan session.Event)
	close(ch)
	return ch, nil
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
