package toolregistry

import (
	"context"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

type fakeProvider struct {
	tools  []model.ToolSpec
	calls  []string
	closed bool
}

func (p *fakeProvider) Tools() []model.ToolSpec {
	return append([]model.ToolSpec(nil), p.tools...)
}

func (p *fakeProvider) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	p.calls = append(p.calls, call.Name)
	return ports.ToolResult{CallID: call.ID, Content: call.Name}, nil
}

func (p *fakeProvider) Close() error {
	p.closed = true
	return nil
}

func TestCompositeToolRegistryRejectsDuplicateNames(t *testing.T) {
	_, err := NewCompositeToolRegistry(
		&fakeProvider{tools: []model.ToolSpec{{Name: "read_file"}}},
		&fakeProvider{tools: []model.ToolSpec{{Name: "read_file"}}},
	)
	if err == nil || !strings.Contains(err.Error(), "duplicate tool name") {
		t.Fatalf("err = %v, want duplicate tool name", err)
	}
}

func TestCompositeToolRegistryRoutesCallsAndClosesProviders(t *testing.T) {
	first := &fakeProvider{tools: []model.ToolSpec{{Name: "read_file"}}}
	second := &fakeProvider{tools: []model.ToolSpec{{Name: "apply_patch"}}}
	registry, err := NewCompositeToolRegistry(first, second)
	if err != nil {
		t.Fatalf("NewCompositeToolRegistry: %v", err)
	}

	result, err := registry.RunTool(context.Background(), model.ToolCall{ID: "call_1", Name: "apply_patch"})
	if err != nil {
		t.Fatalf("RunTool: %v", err)
	}
	if result.Content != "apply_patch" || len(second.calls) != 1 || len(first.calls) != 0 {
		t.Fatalf("routing result=%#v first=%v second=%v", result, first.calls, second.calls)
	}
	if err := registry.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !first.closed || !second.closed {
		t.Fatalf("closed first=%v second=%v, want both closed", first.closed, second.closed)
	}
}

func TestFilterToolNamesHidesAndRejectsDeniedTools(t *testing.T) {
	base := &fakeProvider{tools: []model.ToolSpec{{Name: "terminal_read"}, {Name: "terminal_write"}}}
	filtered := FilterToolNames(base, map[string]string{"terminal_write": "read-only approval mode"})
	for _, tool := range filtered.Tools() {
		if tool.Name == "terminal_write" {
			t.Fatalf("terminal_write exposed in filtered tools: %#v", filtered.Tools())
		}
	}
	if _, err := filtered.RunTool(context.Background(), model.ToolCall{Name: "terminal_write"}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("RunTool err = %v, want read-only rejection", err)
	}
}
