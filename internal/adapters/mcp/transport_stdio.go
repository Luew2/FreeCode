package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *ringBuffer

	closeOnce sync.Once
}

func startStdio(ctx context.Context, serverName string, command string, args []string, envNames []string, workDir string, workspaceRoot string, maxStderr int) (*stdioTransport, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("MCP server %s command is required", serverName)
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("MCP server %s command %q not found on PATH: %w", serverName, command, err)
	}
	if strings.TrimSpace(workDir) != "" && !filepath.IsAbs(workDir) {
		workDir = filepath.Join(workspaceRoot, workDir)
	}
	if strings.TrimSpace(workDir) == "" {
		workDir = workspaceRoot
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = workDir
	cmd.Env = selectedEnv(envNames)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("MCP server %s stdin: %w", serverName, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("MCP server %s stdout: %w", serverName, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("MCP server %s stderr: %w", serverName, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start MCP server %s: %w", serverName, err)
	}
	transport := &stdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: newRingBuffer(maxStderr),
	}
	go func() {
		_, _ = io.Copy(transport.stderr, stderr)
	}()
	return transport, nil
}

func selectedEnv(names []string) []string {
	defaultNames := []string{"PATH", "HOME", "USER", "TMPDIR", "TEMP", "TMP"}
	env := make([]string, 0, len(names)+len(defaultNames))
	seen := map[string]bool{}
	for _, name := range append(defaultNames, names...) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
			seen[name] = true
		}
	}
	return env
}

func (t *stdioTransport) Close() error {
	if t == nil {
		return nil
	}
	var err error
	t.closeOnce.Do(func() {
		if t.stdin != nil {
			_ = t.stdin.Close()
		}
		done := make(chan error, 1)
		go func() {
			done <- t.cmd.Wait()
		}()
		select {
		case waitErr := <-done:
			err = waitErr
		case <-time.After(time.Second):
			_ = t.cmd.Process.Kill()
			err = <-done
		}
		if t.stdout != nil {
			_ = t.stdout.Close()
		}
	})
	return err
}

func (t *stdioTransport) Stderr() string {
	if t == nil || t.stderr == nil {
		return ""
	}
	return t.stderr.String()
}

type ringBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func newRingBuffer(limit int) *ringBuffer {
	if limit <= 0 {
		limit = 16 * 1024
	}
	return &ringBuffer{limit: limit}
}

func (b *ringBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	return len(p), nil
}

func (b *ringBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.TrimSpace(b.data))
}
