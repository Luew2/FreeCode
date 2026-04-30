package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/adapters/workspace/localfs"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
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

// TestReadToolsRespectReadDeny covers the regression where a session with
// Read=DecisionDeny could still call list_files / read_file / search_text
// because the read tools never consulted the permission gate. Every read
// tool must surface a clear "permission denied" error and refuse to do
// any I/O.
func TestReadToolsRespectReadDeny(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md", "hello repo\n")
	writeTestFile(t, root, "src/main.go", "package main\n")

	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	gate := NewStaticPermissionGate(permission.Policy{Read: permission.DecisionDeny})
	tools := NewReadOnlyWithGate(workspace.FileSystem(), gate)

	cases := []struct {
		name string
		call model.ToolCall
	}{
		{name: "list_files", call: model.ToolCall{Name: "list_files", Arguments: []byte(`{}`)}},
		{name: "read_file", call: model.ToolCall{Name: "read_file", Arguments: []byte(`{"path":"README.md"}`)}},
		{name: "search_text", call: model.ToolCall{Name: "search_text", Arguments: []byte(`{"query":"hello"}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tools.RunTool(context.Background(), tc.call)
			if err == nil {
				t.Fatalf("%s returned nil error under read-deny policy", tc.name)
			}
			if !strings.Contains(err.Error(), "denied") {
				t.Fatalf("%s error = %q, want permission denied", tc.name, err.Error())
			}
		})
	}
}

// TestReadToolsScopeReadsToAllowedPaths covers the regression where
// AllowedPaths only restricted writes — list_files and search_text would
// still enumerate every file, and read_file would still read inside any
// path. With AllowedPaths=["docs/"], reads under docs/ must succeed and
// reads under internal/ must be blocked, both for direct read_file and
// for the list/search surface.
func TestReadToolsScopeReadsToAllowedPaths(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "docs/handbook.md", "public docs\n")
	writeTestFile(t, root, "internal/secret.go", "package internal\n// secret token\n")

	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	gate := NewStaticPermissionGate(permission.Policy{
		Read:         permission.DecisionAllow,
		AllowedPaths: []string{"docs/"},
	})
	tools := NewReadOnlyWithGate(workspace.FileSystem(), gate)

	// read_file inside docs/ must succeed.
	read, err := tools.RunTool(context.Background(), model.ToolCall{
		Name:      "read_file",
		Arguments: []byte(`{"path":"docs/handbook.md"}`),
	})
	if err != nil {
		t.Fatalf("read_file under docs/ returned error: %v", err)
	}
	if !strings.Contains(read.Content, "public docs") {
		t.Fatalf("read content = %q, want docs body", read.Content)
	}

	// read_file outside docs/ must be denied.
	if _, err := tools.RunTool(context.Background(), model.ToolCall{
		Name:      "read_file",
		Arguments: []byte(`{"path":"internal/secret.go"}`),
	}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("read_file under internal/ err = %v, want permission denied", err)
	}

	// list_files must filter out internal/ entries while keeping docs/.
	list, err := tools.RunTool(context.Background(), model.ToolCall{
		Name: "list_files", Arguments: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("list_files returned error: %v", err)
	}
	if !strings.Contains(list.Content, "docs/handbook.md") {
		t.Fatalf("list = %q, want docs entry", list.Content)
	}
	if strings.Contains(list.Content, "internal/secret.go") {
		t.Fatalf("list = %q, must not leak internal entries", list.Content)
	}

	// search_text must skip internal/ files even when their content matches.
	search, err := tools.RunTool(context.Background(), model.ToolCall{
		Name:      "search_text",
		Arguments: []byte(`{"query":"secret"}`),
	})
	if err != nil {
		t.Fatalf("search_text returned error: %v", err)
	}
	if strings.Contains(search.Content, "internal/secret.go") {
		t.Fatalf("search = %q, must not surface scoped-out matches", search.Content)
	}
}

// TestReadToolsRespectDeniedPaths covers the inverse of AllowedPaths:
// DeniedPaths=["secrets/"] must block reads inside secrets/ but allow
// neighboring directories.
func TestReadToolsRespectDeniedPaths(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "secrets/api.key", "very-secret\n")
	writeTestFile(t, root, "public/notes.md", "ok to read\n")

	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	gate := NewStaticPermissionGate(permission.Policy{
		Read:        permission.DecisionAllow,
		DeniedPaths: []string{"secrets/"},
	})
	tools := NewReadOnlyWithGate(workspace.FileSystem(), gate)

	// public/ must remain readable.
	read, err := tools.RunTool(context.Background(), model.ToolCall{
		Name:      "read_file",
		Arguments: []byte(`{"path":"public/notes.md"}`),
	})
	if err != nil {
		t.Fatalf("read_file public returned error: %v", err)
	}
	if !strings.Contains(read.Content, "ok to read") {
		t.Fatalf("public read = %q, want body", read.Content)
	}

	// secrets/ must be blocked.
	if _, err := tools.RunTool(context.Background(), model.ToolCall{
		Name:      "read_file",
		Arguments: []byte(`{"path":"secrets/api.key"}`),
	}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("secrets read err = %v, want permission denied", err)
	}

	// list_files and search_text must omit secrets/ entries.
	list, _ := tools.RunTool(context.Background(), model.ToolCall{
		Name: "list_files", Arguments: []byte(`{}`),
	})
	if strings.Contains(list.Content, "secrets/api.key") {
		t.Fatalf("list = %q, must not list denied path", list.Content)
	}
	if !strings.Contains(list.Content, "public/notes.md") {
		t.Fatalf("list = %q, want public neighbor", list.Content)
	}

	search, _ := tools.RunTool(context.Background(), model.ToolCall{
		Name:      "search_text",
		Arguments: []byte(`{"query":"secret"}`),
	})
	if strings.Contains(search.Content, "secrets/api.key") {
		t.Fatalf("search = %q, must not match denied path", search.Content)
	}
}

func TestReadToolsDefaultDenySecretPatterns(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".env.local", "TOKEN=secret\n")
	writeTestFile(t, root, "docs/readme.md", "ok\n")
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	gate := NewStaticPermissionGate(permission.DefaultPolicy())
	tools := NewReadOnlyWithGate(workspace.FileSystem(), gate)

	if _, err := tools.RunTool(context.Background(), model.ToolCall{Name: "read_file", Arguments: []byte(`{"path":".env.local"}`)}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf(".env read err = %v, want denied", err)
	}
	list, err := tools.RunTool(context.Background(), model.ToolCall{Name: "list_files", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatalf("list_files returned error: %v", err)
	}
	if strings.Contains(list.Content, ".env.local") || !strings.Contains(list.Content, "docs/readme.md") {
		t.Fatalf("list = %q, want secret omitted and docs retained", list.Content)
	}
}

// TestReadToolsAllowAllWithoutGate makes sure the legacy NewReadOnly
// constructor (no gate) still allows reads — that is what older sessions
// rely on, and what registry.go's NewVerifier built before the fix.
func TestReadToolsAllowAllWithoutGate(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "any.txt", "contents\n")
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tools := NewReadOnly(workspace.FileSystem())
	if _, err := tools.RunTool(context.Background(), model.ToolCall{
		Name:      "read_file",
		Arguments: []byte(`{"path":"any.txt"}`),
	}); err != nil {
		t.Fatalf("ungated read_file returned error: %v", err)
	}
}
