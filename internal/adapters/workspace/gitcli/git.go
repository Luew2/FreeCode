package gitcli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Luew2/FreeCode/internal/ports"
)

var _ ports.Git = (*Git)(nil)

type Git struct {
	root string
}

func New(root string) (*Git, error) {
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Git{root: abs}, nil
}

func (g *Git) Status(ctx context.Context) (ports.GitStatus, error) {
	if g == nil || g.root == "" {
		return ports.GitStatus{}, errors.New("git root is not configured")
	}
	output, err := g.run(ctx, "status", "--porcelain=v1", "-b")
	if err != nil {
		return ports.GitStatus{}, err
	}

	status := ports.GitStatus{Clean: true}
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			status.Branch = parseBranch(line[3:])
			continue
		}
		status.Clean = false
		status.ChangedFiles = append(status.ChangedFiles, line)
	}
	return status, nil
}

func (g *Git) Diff(ctx context.Context, paths []string) (string, error) {
	if g == nil || g.root == "" {
		return "", errors.New("git root is not configured")
	}
	args := []string{"diff", "--no-ext-diff", "--src-prefix=a/", "--dst-prefix=b/", "--"}
	for _, path := range paths {
		clean, err := cleanGitPath(path)
		if err != nil {
			return "", err
		}
		args = append(args, clean)
	}
	return g.run(ctx, args...)
}

func (g *Git) run(ctx context.Context, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.root}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return string(output), nil
}

func parseBranch(value string) string {
	value = strings.TrimSpace(value)
	if branch, ok := strings.CutPrefix(value, "No commits yet on "); ok {
		value = branch
	}
	if before, _, ok := strings.Cut(value, "..."); ok {
		value = before
	}
	if before, _, ok := strings.Cut(value, " "); ok {
		value = before
	}
	if value == "" {
		return "unknown"
	}
	return value
}

func cleanGitPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("%q must be relative to the workspace", path)
	}
	clean := filepath.Clean(path)
	if clean == "." {
		return ".", nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside workspace", path)
	}
	return filepath.ToSlash(clean), nil
}
