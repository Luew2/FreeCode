package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/adapters/workspace/localfs"
	"github.com/Luew2/FreeCode/internal/core/model"
)

func TestReadOnlyToolsListReadAndSearch(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md", "hello repo\nsecond line\n")
	writeTestFile(t, root, "src/main.go", "package main\n// hello\n")
	writeTestFile(t, root, ".git/ignored", "hello\n")

	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tools := NewReadOnly(workspace.FileSystem())

	list, err := tools.RunTool(context.Background(), model.ToolCall{Name: "list_files", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatalf("list_files returned error: %v", err)
	}
	if !strings.Contains(list.Content, "README.md") || !strings.Contains(list.Content, "src/main.go") {
		t.Fatalf("list output = %q, want workspace files", list.Content)
	}
	if strings.Contains(list.Content, ".git/ignored") {
		t.Fatalf("list output = %q, should skip .git", list.Content)
	}

	read, err := tools.RunTool(context.Background(), model.ToolCall{Name: "read_file", Arguments: []byte(`{"path":"README.md"}`)})
	if err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}
	if !strings.Contains(read.Content, "hello repo") {
		t.Fatalf("read output = %q, want file content", read.Content)
	}

	search, err := tools.RunTool(context.Background(), model.ToolCall{Name: "search_text", Arguments: []byte(`{"query":"hello"}`)})
	if err != nil {
		t.Fatalf("search_text returned error: %v", err)
	}
	if !strings.Contains(search.Content, "README.md:1:hello repo") || !strings.Contains(search.Content, "src/main.go:2:// hello") {
		t.Fatalf("search output = %q, want matches", search.Content)
	}
}

func TestReadOnlyToolsRejectInvalidArgs(t *testing.T) {
	workspace, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	_, err = NewReadOnly(workspace.FileSystem()).RunTool(context.Background(), model.ToolCall{Name: "read_file", Arguments: []byte(`{"path":"../outside"}`)})
	if err == nil {
		t.Fatal("read_file returned nil error for outside path")
	}
}

func writeTestFile(t *testing.T, root string, path string, contents string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
