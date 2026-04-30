package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"

	openai_chat "github.com/Luew2/FreeCode/internal/adapters/model/openai_chat"
	envsecrets "github.com/Luew2/FreeCode/internal/adapters/secrets/env"
	"github.com/Luew2/FreeCode/internal/app/bench"
	"github.com/Luew2/FreeCode/internal/core/model"
)

func benchTasks() []bench.Task {
	tasks := bench.DefaultTasks()
	tasks = append(tasks, bench.Task{Name: "mocked-openai-provider", Run: benchmarkMockedOpenAIProvider})
	return tasks
}

func benchTaskNames() []string {
	tasks := benchTasks()
	names := make([]string, 0, len(tasks))
	for _, task := range tasks {
		names = append(names, task.Name)
	}
	return names
}

func benchmarkMockedOpenAIProvider(ctx context.Context) error {
	const envName = "FREECODE_BENCH_FAKE_OPENAI_API_KEY"
	const fakeKey = "bench-fake-key"

	previous, hadPrevious := os.LookupEnv(envName)
	if err := os.Setenv(envName, fakeKey); err != nil {
		return err
	}
	defer func() {
		if hadPrevious {
			_ = os.Setenv(envName, previous)
			return
		}
		_ = os.Unsetenv(envName)
	}()

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost {
			http.Error(w, "method must be POST", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+fakeKey {
			http.Error(w, "unexpected authorization header", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			http.Error(w, "unexpected accept header", http.StatusBadRequest)
			return
		}

		var body struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		if body.Model != "coder" || !body.Stream {
			http.Error(w, "unexpected model request", http.StatusBadRequest)
			return
		}
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.Messages[0].Content != "Say bench." {
			http.Error(w, "unexpected messages", http.StatusBadRequest)
			return
		}
		if len(body.Tools) != 1 || body.Tools[0].Type != "function" || body.Tools[0].Function.Name != "record_benchmark" {
			http.Error(w, "unexpected tools", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"bench ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_bench\",\"type\":\"function\",\"function\":{\"name\":\"record_benchmark\",\"arguments\":\"{\\\"status\\\":\\\"pass\\\"}\"}}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client, err := openai_chat.NewClient(model.Provider{
		ID:           "mocked",
		Protocol:     openai_chat.Protocol,
		BaseURL:      server.URL,
		Secret:       model.SecretRef{Name: envName, Source: "env"},
		DefaultModel: "coder",
	}, envsecrets.New(), server.Client())
	if err != nil {
		return err
	}
	events, err := client.Stream(ctx, model.Request{
		Model:    model.NewRef("mocked", "coder"),
		Messages: []model.Message{model.TextMessage(model.RoleUser, "Say bench.")},
		Tools: []model.ToolSpec{{
			Name:        "record_benchmark",
			Description: "records the benchmark status",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{"type": "string"},
				},
			},
		}},
		Stream: true,
	})
	if err != nil {
		return err
	}

	var gotText string
	var gotTool bool
	var gotUsage bool
	var gotCompleted bool
	for event := range events {
		switch event.Type {
		case model.EventTextDelta:
			gotText += event.Text
		case model.EventToolCall:
			if event.ToolCall != nil && event.ToolCall.ID == "call_bench" && event.ToolCall.Name == "record_benchmark" && string(event.ToolCall.Arguments) == `{"status":"pass"}` {
				gotTool = true
			}
		case model.EventUsage:
			if event.Usage != nil && event.Usage.TotalTokens == 5 {
				gotUsage = true
			}
		case model.EventCompleted:
			gotCompleted = true
		case model.EventError:
			return errors.New(event.Error)
		}
	}
	if requests != 1 {
		return fmt.Errorf("provider requests = %d, want 1", requests)
	}
	if gotText != "bench ok" {
		return fmt.Errorf("provider text = %q, want bench ok", gotText)
	}
	if !gotTool {
		return errors.New("provider tool call was not streamed")
	}
	if !gotUsage {
		return errors.New("provider usage was not streamed")
	}
	if !gotCompleted {
		return errors.New("provider stream did not complete")
	}
	return nil
}
