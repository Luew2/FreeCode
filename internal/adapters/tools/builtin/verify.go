package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/ports"
)

type Verify struct {
	gate    ports.PermissionGate
	root    string
	timeout time.Duration
}

// NewVerify constructs a verifier that runs allowlisted commands. root is
// the workspace root: the verifier sets cmd.Dir to root so checks run
// inside the workspace (not the freecode process cwd) and rejects package
// arguments that escape root with ".." or absolute paths. An empty root
// falls back to "." which matches the legacy behavior; pass an explicit
// root in production.
func NewVerify(gate ports.PermissionGate, root string) *Verify {
	return &Verify{gate: gate, root: root, timeout: 2 * time.Minute}
}

func (t *Verify) ToolSpec() model.ToolSpec {
	return model.ToolSpec{
		Name:        "run_check",
		Description: "Run a focused verification command from an allowlisted set.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Verification command. Allowed: go test ./..., go test PACKAGE, go vet ./...",
				},
			},
			"required": []string{"command"},
		},
	}
}

func (t *Verify) Run(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	if t == nil {
		return ports.ToolResult{}, errors.New("verify tool is not configured")
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return ports.ToolResult{}, fmt.Errorf("run_check arguments: %w", err)
	}
	command := strings.TrimSpace(args.Command)
	if command == "" {
		return ports.ToolResult{}, errors.New("run_check requires command")
	}
	argv, err := allowedCheck(command)
	if err != nil {
		return ports.ToolResult{}, err
	}
	if t.gate != nil {
		decision, err := t.gate.Decide(ctx, permission.Request{Action: permission.ActionShell, Subject: command, Reason: "run_check"})
		if err != nil {
			return ports.ToolResult{}, err
		}
		switch decision {
		case permission.DecisionAllow:
		case permission.DecisionAsk:
			return ports.ToolResult{}, errors.New("shell permission requires approval")
		default:
			return ports.ToolResult{}, errors.New("shell permission denied")
		}
	}
	timeout := t.timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	if t != nil && strings.TrimSpace(t.root) != "" {
		cmd.Dir = t.root
	}
	output, err := cmd.CombinedOutput()
	content := strings.TrimSpace(string(output))
	if content == "" {
		content = "check completed with no output"
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return ports.ToolResult{}, fmt.Errorf("run_check timed out: %s", command)
	}
	metadata := map[string]string{"command": command}
	if err != nil {
		metadata["exit"] = "failed"
		return ports.ToolResult{CallID: call.ID, Content: "check failed: " + command + "\n" + content, Metadata: metadata}, fmt.Errorf("run_check failed: %w", err)
	}
	metadata["exit"] = "passed"
	return ports.ToolResult{CallID: call.ID, Content: "check passed: " + command + "\n" + content, Metadata: metadata}, nil
}

func allowedCheck(command string) ([]string, error) {
	fields := strings.Fields(command)
	if len(fields) < 2 {
		return nil, fmt.Errorf("run_check command %q is not allowed", command)
	}
	if fields[0] != "go" {
		return nil, fmt.Errorf("run_check command %q is not allowed", command)
	}
	switch fields[1] {
	case "test":
		if len(fields) == 2 {
			return nil, fmt.Errorf("run_check command %q is not allowed", command)
		}
		for _, arg := range fields[2:] {
			if strings.HasPrefix(arg, "-") && arg != "-race" {
				return nil, fmt.Errorf("run_check flag %q is not allowed", arg)
			}
			if strings.Contains(arg, ";") || strings.Contains(arg, "&&") || strings.Contains(arg, "|") {
				return nil, fmt.Errorf("run_check command %q is not allowed", command)
			}
			if err := validatePackageArg(arg); err != nil {
				return nil, err
			}
		}
		return fields, nil
	case "vet":
		if len(fields) == 3 && fields[2] == "./..." {
			return fields, nil
		}
	}
	return nil, fmt.Errorf("run_check command %q is not allowed", command)
}

// validatePackageArg rejects `go test` package arguments that would leave
// the workspace. We allow:
//   - flags ("-race") — caller already enforces the allowlist.
//   - the canonical `./...` selector and other relative paths under cwd.
//
// We reject:
//   - absolute paths ("/etc", "/tmp/...").
//   - any path containing a "..": both the dotted parent and any segment
//     that would walk out of the workspace.
func validatePackageArg(arg string) error {
	if strings.HasPrefix(arg, "-") {
		return nil
	}
	if arg == "" {
		return fmt.Errorf("run_check package arg is empty")
	}
	if strings.HasPrefix(arg, "/") {
		return fmt.Errorf("run_check package %q must stay inside the workspace", arg)
	}
	// Reject any segment that is ".." or starts with "../"; this catches
	// "../...", "../../foo", "foo/../bar" and similar attempts to walk
	// out of the workspace through an apparently-relative path.
	for _, segment := range strings.Split(arg, "/") {
		if segment == ".." {
			return fmt.Errorf("run_check package %q must stay inside the workspace", arg)
		}
	}
	return nil
}
