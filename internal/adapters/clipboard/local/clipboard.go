package local

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"
)

type Clipboard struct {
	Terminal io.Writer
}

func New(terminal io.Writer) *Clipboard {
	return &Clipboard{Terminal: terminal}
}

func (c *Clipboard) Copy(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cmd, ok := clipboardCommand(); ok {
		command := exec.CommandContext(ctx, cmd.name, cmd.args...)
		command.Stdin = strings.NewReader(text)
		if output, err := command.CombinedOutput(); err != nil {
			message := strings.TrimSpace(string(output))
			if message == "" {
				message = err.Error()
			}
			return errors.New(message)
		}
		return nil
	}
	if c != nil && c.Terminal != nil {
		encoded := base64.StdEncoding.EncodeToString([]byte(text))
		_, err := io.WriteString(c.Terminal, "\x1b]52;c;"+encoded+"\a")
		return err
	}
	return errors.New("no clipboard backend found")
}

type commandSpec struct {
	name string
	args []string
}

func clipboardCommand() (commandSpec, bool) {
	candidates := []commandSpec{}
	switch runtime.GOOS {
	case "darwin":
		candidates = append(candidates, commandSpec{name: "pbcopy"})
	default:
		candidates = append(candidates,
			commandSpec{name: "wl-copy"},
			commandSpec{name: "xclip", args: []string{"-selection", "clipboard"}},
		)
	}
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate.name); err == nil {
			return candidate, true
		}
	}
	return commandSpec{}, false
}
