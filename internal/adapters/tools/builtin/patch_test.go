package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luew2/FreeCode/internal/adapters/workspace/localfs"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/ports"
)

func TestApplyPatchPreviewsByDefault(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md", "hello repo\n")
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tools := NewWritable(workspace.FileSystem(), NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))

	result, err := tools.RunTool(context.Background(), model.ToolCall{
		ID:        "call_1",
		Name:      "apply_patch",
		Arguments: []byte(`{"summary":"update readme","changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`),
	})
	if err != nil {
		t.Fatalf("apply_patch returned error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello repo\n" {
		t.Fatalf("content = %q, preview should not patch file", string(data))
	}
	if result.Artifact == nil || result.Artifact.ID.String() != "p1" {
		t.Fatalf("artifact = %#v, want patch p1", result.Artifact)
	}
	if result.Metadata["state"] != "preview" || !strings.Contains(result.Content, "preview patch p1") || !strings.Contains(result.Artifact.Body, "-hello repo") {
		t.Fatalf("result = %#v, want preview metadata and diff body", result)
	}
}

func TestApplyPatchReplacesExactTextWhenAccepted(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md", "hello repo\n")
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tools := NewWritable(workspace.FileSystem(), NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))

	preview, err := tools.RunTool(context.Background(), model.ToolCall{
		ID:        "call_1",
		Name:      "apply_patch",
		Arguments: []byte(`{"summary":"update readme","changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`),
	})
	if err != nil {
		t.Fatalf("preview apply_patch returned error: %v", err)
	}
	token := preview.Metadata["preview_token"]
	if token == "" {
		t.Fatalf("preview metadata = %#v, want preview token", preview.Metadata)
	}

	result, err := tools.RunTool(context.Background(), model.ToolCall{
		ID:        "call_2",
		Name:      "apply_patch",
		Arguments: []byte(`{"summary":"update readme","accepted":true,"preview_token":"` + token + `","changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`),
	})
	if err != nil {
		t.Fatalf("apply_patch returned error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello FreeCode\n" {
		t.Fatalf("content = %q, want patched content", string(data))
	}
	if result.Metadata["state"] != "applied" || result.Metadata["changed_files"] != "README.md" || !strings.Contains(result.Content, "applied patch p2") {
		t.Fatalf("result = %#v, want applied changed file metadata and patch id", result)
	}
}

func TestApplyPatchRejectsAcceptedWithoutPriorPreview(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md", "hello repo\n")
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tool := NewApplyPatch(workspace.FileSystem(), NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))

	_, err = tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "preview_token") {
		t.Fatalf("error = %v, want preview token requirement", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello repo\n" {
		t.Fatalf("content = %q, accepted without preview overwrote file", string(data))
	}
}

func TestApplyPatchRejectsAcceptedPatchThatDiffersFromPreview(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md", "hello repo\n")
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tool := NewApplyPatch(workspace.FileSystem(), NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))

	preview, err := tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`),
	})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}

	_, err = tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"preview_token":"` + preview.Metadata["preview_token"] + `","changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"different\n"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want preview mismatch", err)
	}
}

func TestApplyPatchRejectsExpiredPreviewToken(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md", "hello repo\n")
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	now := time.Unix(1, 0).UTC()
	tool := NewApplyPatch(workspace.FileSystem(), NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))
	tool.previews = NewPreviewStore(8, time.Second)
	tool.now = func() time.Time { return now }

	preview, err := tool.Run(context.Background(), model.ToolCall{
		ID:        "call_1",
		Name:      "apply_patch",
		Arguments: []byte(`{"changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`),
	})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}
	now = now.Add(2 * time.Second)
	_, err = tool.Run(context.Background(), model.ToolCall{
		ID:        "call_2",
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"preview_token":"` + preview.Metadata["preview_token"] + `","changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want expired preview token", err)
	}
}

func TestApplyPatchCreatesNewFile(t *testing.T) {
	workspace, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tool := NewApplyPatch(workspace.FileSystem(), NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))

	preview, err := tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"changes":[{"path":"notes/todo.txt","new_text":"ship it\n"}]}`),
	})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}
	_, err = tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"preview_token":"` + preview.Metadata["preview_token"] + `","changes":[{"path":"notes/todo.txt","new_text":"ship it\n"}]}`),
	})
	if err != nil {
		t.Fatalf("apply_patch returned error: %v", err)
	}
	data, err := workspace.FileSystem().ReadFile(context.Background(), "notes/todo.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "ship it\n" {
		t.Fatalf("content = %q, want created file", string(data))
	}
}

func TestApplyPatchRejectsStaleContextBeforeOverwrite(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md", "user changed\n")
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tool := NewApplyPatch(workspace.FileSystem(), NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))

	_, err = tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "stale patch") {
		t.Fatalf("error = %v, want stale patch", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "user changed\n" {
		t.Fatalf("content = %q, stale patch overwrote file", string(data))
	}
}

func TestApplyPatchRejectsPathTraversal(t *testing.T) {
	workspace, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tool := NewApplyPatch(workspace.FileSystem(), NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))

	_, err = tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"changes":[{"path":"../outside.txt","new_text":"nope\n"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("error = %v, want outside workspace", err)
	}
}

func TestApplyPatchRequiresAllowedWritePermission(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md", "hello repo\n")
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tool := NewApplyPatch(workspace.FileSystem(), NewStaticPermissionGate(permission.Policy{Write: permission.DecisionDeny}))

	preview, err := tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`),
	})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}
	_, err = tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"preview_token":"` + preview.Metadata["preview_token"] + `","changes":[{"path":"README.md","old_text":"hello repo\n","new_text":"hello FreeCode\n"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "write permission denied") {
		t.Fatalf("error = %v, want write permission denied", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello repo\n" {
		t.Fatalf("content = %q, permission failure overwrote file", string(data))
	}
}

func TestApplyPatchRespectsAllowedPathScope(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "allowed/a.txt", "old\n")
	writeTestFile(t, root, "blocked/b.txt", "old\n")
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tool := NewApplyPatch(workspace.FileSystem(), NewStaticPermissionGate(permission.Policy{
		Write:        permission.DecisionAllow,
		AllowedPaths: []string{"allowed"},
	}))

	preview, err := tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"changes":[{"path":"blocked/b.txt","old_text":"old\n","new_text":"new\n"}]}`),
	})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}
	_, err = tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"preview_token":"` + preview.Metadata["preview_token"] + `","changes":[{"path":"blocked/b.txt","old_text":"old\n","new_text":"new\n"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "write permission denied") {
		t.Fatalf("error = %v, want path-scoped denial", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "blocked/b.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "old\n" {
		t.Fatalf("blocked file = %q, path-scoped denial overwrote file", string(data))
	}
}

func TestApplyPatchRejectsConflictingPathsBeforePartialWrite(t *testing.T) {
	root := t.TempDir()
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tool := NewApplyPatch(workspace.FileSystem(), NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))

	preview, err := tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"changes":[{"path":"a","new_text":"file\n"},{"path":"a/b","new_text":"nested\n"}]}`),
	})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}
	_, err = tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"preview_token":"` + preview.Metadata["preview_token"] + `","changes":[{"path":"a","new_text":"file\n"},{"path":"a/b","new_text":"nested\n"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting paths") {
		t.Fatalf("error = %v, want conflicting paths", err)
	}
	if _, readErr := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(readErr) {
		t.Fatalf("path a exists after failed patch: %v", readErr)
	}
}

func TestApplyPatchRollsBackWrittenFilesOnLaterFailure(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "old a\n")
	writeTestFile(t, root, "b.txt", "old b\n")
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tool := NewApplyPatch(failingWriteFileSystem{
		FileSystem: workspace.FileSystem(),
		failPath:   "b.txt",
	}, NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))

	preview, err := tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"changes":[{"path":"a.txt","old_text":"old a\n","new_text":"new a\n"},{"path":"b.txt","old_text":"old b\n","new_text":"new b\n"}]}`),
	})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}
	_, err = tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"preview_token":"` + preview.Metadata["preview_token"] + `","changes":[{"path":"a.txt","old_text":"old a\n","new_text":"new a\n"},{"path":"b.txt","old_text":"old b\n","new_text":"new b\n"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "write b.txt") {
		t.Fatalf("error = %v, want b.txt write failure", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("ReadFile a.txt: %v", err)
	}
	if string(data) != "old a\n" {
		t.Fatalf("a.txt = %q, want rollback to old content", string(data))
	}
}

func TestApplyPatchRemovesCreatedDirectoriesOnRollback(t *testing.T) {
	root := t.TempDir()
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tool := NewApplyPatch(failingWriteFileSystem{
		FileSystem: workspace.FileSystem(),
		failPath:   "z.txt",
	}, NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))

	preview, err := tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"changes":[{"path":"a/new.txt","new_text":"new\n"},{"path":"z.txt","new_text":"fail\n"}]}`),
	})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}
	_, err = tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"preview_token":"` + preview.Metadata["preview_token"] + `","changes":[{"path":"a/new.txt","new_text":"new\n"},{"path":"z.txt","new_text":"fail\n"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "write z.txt") {
		t.Fatalf("error = %v, want z.txt write failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(statErr) {
		t.Fatalf("created directory remains after rollback: %v", statErr)
	}
}

func TestApplyPatchCleansFailedWriteSideEffects(t *testing.T) {
	root := t.TempDir()
	workspace, err := localfs.New(root)
	if err != nil {
		t.Fatalf("New workspace: %v", err)
	}
	tool := NewApplyPatch(sideEffectFailingFileSystem{
		FileSystem: workspace.FileSystem(),
		failPath:   "nested/new.txt",
	}, NewStaticPermissionGate(permission.Policy{Write: permission.DecisionAllow}))

	preview, err := tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"changes":[{"path":"nested/new.txt","new_text":"new\n"}]}`),
	})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}
	_, err = tool.Run(context.Background(), model.ToolCall{
		Name:      "apply_patch",
		Arguments: []byte(`{"accepted":true,"preview_token":"` + preview.Metadata["preview_token"] + `","changes":[{"path":"nested/new.txt","new_text":"new\n"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "write nested/new.txt") {
		t.Fatalf("error = %v, want nested/new.txt write failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "nested")); !os.IsNotExist(statErr) {
		t.Fatalf("created directory remains after failed write rollback: %v", statErr)
	}
}

type failingWriteFileSystem struct {
	ports.FileSystem
	failPath string
}

func (f failingWriteFileSystem) WriteFile(ctx context.Context, path string, contents []byte) error {
	if path == f.failPath {
		return os.ErrPermission
	}
	return f.FileSystem.WriteFile(ctx, path, contents)
}

type sideEffectFailingFileSystem struct {
	ports.FileSystem
	failPath string
}

func (f sideEffectFailingFileSystem) WriteFile(ctx context.Context, path string, contents []byte) error {
	if path == f.failPath {
		if err := f.FileSystem.WriteFile(ctx, path, contents); err != nil {
			return err
		}
		return os.ErrPermission
	}
	return f.FileSystem.WriteFile(ctx, path, contents)
}
