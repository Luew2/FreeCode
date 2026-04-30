package commands

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/ports"
)

func TestPrintStatus(t *testing.T) {
	git := &fakeGit{
		status: ports.GitStatus{
			Branch:       "main",
			Clean:        false,
			ChangedFiles: []string{" M README.md"},
		},
	}
	var out bytes.Buffer
	if err := PrintStatus(context.Background(), &out, git); err != nil {
		t.Fatalf("PrintStatus: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, "branch: main") || !strings.Contains(output, " M README.md") {
		t.Fatalf("output = %q, want branch and changed file", output)
	}
}

func TestPrintDiff(t *testing.T) {
	git := &fakeGit{diff: "diff --git a/README.md b/README.md\n"}
	var out bytes.Buffer
	if err := PrintDiff(context.Background(), &out, git, []string{"README.md"}); err != nil {
		t.Fatalf("PrintDiff: %v", err)
	}
	if out.String() != git.diff {
		t.Fatalf("output = %q, want diff", out.String())
	}
	if len(git.paths) != 1 || git.paths[0] != "README.md" {
		t.Fatalf("paths = %#v, want README.md", git.paths)
	}
}

func TestPrintDiffShowsEmptyDiff(t *testing.T) {
	var out bytes.Buffer
	if err := PrintDiff(context.Background(), &out, &fakeGit{}, nil); err != nil {
		t.Fatalf("PrintDiff: %v", err)
	}
	if out.String() != "no diff\n" {
		t.Fatalf("output = %q, want no diff", out.String())
	}
}

func TestPrintStatusPropagatesErrors(t *testing.T) {
	errBoom := errors.New("boom")
	var out bytes.Buffer
	err := PrintStatus(context.Background(), &out, &fakeGit{err: errBoom})
	if !errors.Is(err, errBoom) {
		t.Fatalf("error = %v, want boom", err)
	}
}

type fakeGit struct {
	status ports.GitStatus
	diff   string
	paths  []string
	err    error
}

func (g *fakeGit) Status(ctx context.Context) (ports.GitStatus, error) {
	if g.err != nil {
		return ports.GitStatus{}, g.err
	}
	return g.status, nil
}

func (g *fakeGit) Diff(ctx context.Context, paths []string) (string, error) {
	if g.err != nil {
		return "", g.err
	}
	g.paths = paths
	return g.diff, nil
}
