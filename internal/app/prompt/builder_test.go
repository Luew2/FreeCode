package prompt

import (
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/model"
)

func TestBuilderBuildsDeterministicPromptOrder(t *testing.T) {
	messages := NewBuilder().Build(Environment{
		WorkspaceRoot: "/repo",
		Shell:         "/bin/zsh",
		GitBranch:     "main",
		Platform:      "darwin/arm64",
		WritableRoots: []string{"/repo/b", "/repo/a"},
	}, "  explain repo  ")

	if len(messages) != 5 {
		t.Fatalf("messages = %d, want 5", len(messages))
	}
	wantRoles := []model.Role{
		model.RoleSystem,
		model.RoleDeveloper,
		model.RoleDeveloper,
		model.RoleDeveloper,
		model.RoleUser,
	}
	for i, want := range wantRoles {
		if messages[i].Role != want {
			t.Fatalf("message %d role = %q, want %q", i, messages[i].Role, want)
		}
	}
	env := messages[3].Content[0].Text
	if !strings.Contains(env, "workspace_root: /repo") {
		t.Fatalf("environment prompt = %q, want workspace root", env)
	}
	if !strings.Contains(env, "writable_roots: /repo/a, /repo/b") {
		t.Fatalf("environment prompt = %q, want sorted writable roots", env)
	}
	if got := messages[4].Content[0].Text; got != "explain repo" {
		t.Fatalf("user request = %q, want trimmed request", got)
	}
	developer := messages[1].Content[0].Text
	if !strings.Contains(developer, "call the tool in the same turn") ||
		!strings.Contains(developer, "nested orchestration tasks") {
		t.Fatalf("developer prompt = %q, want tool-followthrough and nested delegation guidance", developer)
	}
}
