package editorcli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type Editor struct {
	Command string
}

func NewFromEnv() *Editor {
	command := strings.TrimSpace(os.Getenv("EDITOR"))
	if command == "" {
		command = "nvim"
	}
	return &Editor{Command: command}
}

func (e *Editor) Open(ctx context.Context, path string, line int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil || strings.TrimSpace(e.Command) == "" {
		return errors.New("editor command is not configured")
	}
	args := strings.Fields(e.Command)
	if len(args) == 0 {
		return errors.New("editor command is not configured")
	}
	name := args[0]
	args = append(args[:0:0], args[1:]...)
	args = append(args, editorArgs(filepath.Clean(path), line, filepath.Base(name))...)

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func editorArgs(path string, line int, editorName string) []string {
	if line <= 0 {
		return []string{path}
	}
	switch editorName {
	case "code", "code-insiders", "codium":
		return []string{"--goto", path + ":" + strconv.Itoa(line)}
	case "vim", "nvim", "vi":
		return []string{"+" + strconv.Itoa(line), path}
	default:
		return []string{path}
	}
}
