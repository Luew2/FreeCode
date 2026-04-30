package builtin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
)

// TestVerifyRejectsParentTraversalPackageArg covers the regression where
// `go test ../...` ran in the freecode process cwd and walked outside
// the workspace, exposing the user's parent directory to the agent.
func TestVerifyRejectsParentTraversalPackageArg(t *testing.T) {
	gate := NewStaticPermissionGate(permission.Policy{Shell: permission.DecisionAllow})
	verify := NewVerify(gate, t.TempDir())

	_, err := verify.Run(context.Background(), model.ToolCall{
		Name:      "run_check",
		Arguments: []byte(`{"command":"go test ../..."}`),
	})
	if err == nil {
		t.Fatalf("run_check ../... returned nil error")
	}
	if !strings.Contains(err.Error(), "must stay inside the workspace") {
		t.Fatalf("error = %q, want workspace-scope message", err.Error())
	}
}

// TestVerifyRejectsAbsolutePackageArg covers the regression where the
// agent could ask for an absolute path, e.g. `/etc`, and the verifier
// would happily run it because absolute paths short-circuited the
// allow-list. Absolute paths must be rejected before exec runs.
func TestVerifyRejectsAbsolutePackageArg(t *testing.T) {
	gate := NewStaticPermissionGate(permission.Policy{Shell: permission.DecisionAllow})
	verify := NewVerify(gate, t.TempDir())

	_, err := verify.Run(context.Background(), model.ToolCall{
		Name:      "run_check",
		Arguments: []byte(`{"command":"go test /etc"}`),
	})
	if err == nil {
		t.Fatalf("run_check /etc returned nil error")
	}
	if !strings.Contains(err.Error(), "must stay inside the workspace") {
		t.Fatalf("error = %q, want workspace-scope message", err.Error())
	}
}

// TestVerifyRejectsHiddenParentTraversal makes sure a sneakier traversal
// like `foo/../../etc` is also blocked. We split on '/' and reject any
// '..' segment, regardless of where it sits in the path.
func TestVerifyRejectsHiddenParentTraversal(t *testing.T) {
	gate := NewStaticPermissionGate(permission.Policy{Shell: permission.DecisionAllow})
	verify := NewVerify(gate, t.TempDir())

	_, err := verify.Run(context.Background(), model.ToolCall{
		Name:      "run_check",
		Arguments: []byte(`{"command":"go test foo/../../etc"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "must stay inside the workspace") {
		t.Fatalf("err = %v, want workspace-scope message", err)
	}
}

// TestVerifyRunsInWorkspaceRootNotProcessCwd is the end-to-end check for
// the cmd.Dir fix. We build a tiny Go module inside a temp directory,
// chdir the test process to a different directory, and confirm `go test
// ./...` discovers the temp module — which is only possible if cmd.Dir
// was set to the workspace root rather than inherited from the process.
func TestVerifyRunsInWorkspaceRootNotProcessCwd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}

	workspaceRoot := t.TempDir()
	processCwd := t.TempDir()

	// A minimal module + passing test inside workspaceRoot. If cmd.Dir
	// were left unset the verifier would run `go test ./...` from
	// processCwd, which has no go.mod and would error before hitting
	// the test we just wrote.
	if err := os.WriteFile(filepath.Join(workspaceRoot, "go.mod"), []byte("module verifytest\n\ngo 1.24\n"), 0o600); err != nil {
		t.Fatalf("WriteFile go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "verify_marker_test.go"), []byte(`package verifytest

import "testing"

func TestVerifyMarkerSentinel(t *testing.T) {}
`), 0o600); err != nil {
		t.Fatalf("WriteFile test: %v", err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(processCwd); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})

	gate := NewStaticPermissionGate(permission.Policy{Shell: permission.DecisionAllow})
	verify := NewVerify(gate, workspaceRoot)

	result, err := verify.Run(context.Background(), model.ToolCall{
		Name:      "run_check",
		Arguments: []byte(`{"command":"go test ./..."}`),
	})
	if err != nil {
		t.Fatalf("run_check returned error: %v\nresult: %#v", err, result)
	}
	if result.Metadata["exit"] != "passed" {
		t.Fatalf("result = %#v, want passed exit", result)
	}
	// The verifier ran inside the temp module, so its output should
	// reference the verifytest package or "ok" line. Either confirms
	// cmd.Dir landed at workspaceRoot.
	if !strings.Contains(result.Content, "verifytest") && !strings.Contains(result.Content, "ok") {
		t.Fatalf("output = %q, want module-specific output", result.Content)
	}
}

// TestVerifyAcceptsCanonicalPackageSelector confirms the safe-path arg
// `./...` continues to work end-to-end after we added validation. This
// is the most common run_check call and was previously the only case.
// We build a real (trivial) package + test inside the temp module so
// `go test ./...` has something to run.
func TestVerifyAcceptsCanonicalPackageSelector(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}

	workspaceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspaceRoot, "go.mod"), []byte("module verifytest\n\ngo 1.24\n"), 0o600); err != nil {
		t.Fatalf("WriteFile go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "verify_canonical_test.go"), []byte(`package verifytest

import "testing"

func TestVerifyCanonicalSentinel(t *testing.T) {}
`), 0o600); err != nil {
		t.Fatalf("WriteFile test: %v", err)
	}

	gate := NewStaticPermissionGate(permission.Policy{Shell: permission.DecisionAllow})
	verify := NewVerify(gate, workspaceRoot)

	result, err := verify.Run(context.Background(), model.ToolCall{
		Name:      "run_check",
		Arguments: []byte(`{"command":"go test ./..."}`),
	})
	if err != nil {
		t.Fatalf("run_check ./... returned error: %v\nresult: %#v", err, result)
	}
	if result.Metadata["exit"] != "passed" {
		t.Fatalf("result = %#v, want passed exit", result)
	}
}

// TestValidatePackageArgUnitCases pins the package-arg validator behavior
// without spinning up a process. It exercises edge cases the higher-level
// tests do not cover (empty arg, leading "./", etc).
func TestValidatePackageArgUnitCases(t *testing.T) {
	cases := []struct {
		arg     string
		wantErr bool
	}{
		{arg: "./...", wantErr: false},
		{arg: "./pkg", wantErr: false},
		{arg: "pkg/sub", wantErr: false},
		{arg: "-race", wantErr: false},
		{arg: "../...", wantErr: true},
		{arg: "../foo", wantErr: true},
		{arg: "foo/../bar", wantErr: true},
		{arg: "/etc", wantErr: true},
		{arg: "/abs/path", wantErr: true},
		{arg: "", wantErr: true},
	}
	for _, tc := range cases {
		err := validatePackageArg(tc.arg)
		if (err != nil) != tc.wantErr {
			t.Errorf("validatePackageArg(%q) err = %v, wantErr = %v", tc.arg, err, tc.wantErr)
		}
	}
}
