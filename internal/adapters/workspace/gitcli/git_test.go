package gitcli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitStatusAndDiff(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "branch", "-M", "main")
	writeFile(t, root, "README.md", "hello repo\n")
	runGit(t, root, "add", "README.md")
	runGit(t, root, "-c", "user.name=FreeCode", "-c", "user.email=freecode@example.test", "commit", "-m", "initial")
	writeFile(t, root, "README.md", "hello FreeCode\n")

	git, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	status, err := git.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Branch != "main" {
		t.Fatalf("branch = %q, want main", status.Branch)
	}
	if status.Clean || len(status.ChangedFiles) != 1 || !strings.Contains(status.ChangedFiles[0], "README.md") {
		t.Fatalf("status = %#v, want modified README", status)
	}

	diff, err := git.Diff(context.Background(), []string{"README.md"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "-hello repo") || !strings.Contains(diff, "+hello FreeCode") {
		t.Fatalf("diff = %q, want README change", diff)
	}
}

func TestGitDiffRejectsPathTraversal(t *testing.T) {
	git, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = git.Diff(context.Background(), []string{"../outside"})
	if err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("error = %v, want outside workspace", err)
	}
}

func TestParseBranchHandlesUnbornBranch(t *testing.T) {
	if got := parseBranch("No commits yet on main"); got != "main" {
		t.Fatalf("branch = %q, want main", got)
	}
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

func writeFile(t *testing.T, root string, path string, contents string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
