package commands

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Luew2/FreeCode/internal/ports"
)

func PrintStatus(ctx context.Context, w io.Writer, git ports.Git) error {
	if git == nil {
		return fmt.Errorf("git is not configured")
	}
	status, err := git.Status(ctx)
	if err != nil {
		return err
	}
	branch := strings.TrimSpace(status.Branch)
	if branch == "" {
		branch = "unknown"
	}
	if _, err := fmt.Fprintf(w, "branch: %s\n", branch); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "clean: %t\n", status.Clean); err != nil {
		return err
	}
	if status.Clean {
		return nil
	}
	if _, err := fmt.Fprintln(w, "changed:"); err != nil {
		return err
	}
	for _, file := range status.ChangedFiles {
		if _, err := fmt.Fprintf(w, "  %s\n", file); err != nil {
			return err
		}
	}
	return nil
}

func PrintDiff(ctx context.Context, w io.Writer, git ports.Git, paths []string) error {
	if git == nil {
		return fmt.Errorf("git is not configured")
	}
	diff, err := git.Diff(ctx, paths)
	if err != nil {
		return err
	}
	if strings.TrimSpace(diff) == "" {
		_, err = fmt.Fprintln(w, "no diff")
		return err
	}
	_, err = fmt.Fprint(w, diff)
	return err
}
