package localfs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFileSystemStaysInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	data, err := workspace.FileSystem().ReadFile(context.Background(), "README.md")
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("content = %q, want hello", string(data))
	}

	_, err = workspace.FileSystem().ReadFile(context.Background(), "../outside")
	if err == nil {
		t.Fatal("ReadFile returned nil error for outside path")
	}
}

func TestFileSystemRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = workspace.FileSystem().ReadFile(context.Background(), "escape/secret.txt")
	if err == nil {
		t.Fatal("ReadFile returned nil error for symlink escape")
	}
	if !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("error = %q, want outside workspace", err.Error())
	}
}
