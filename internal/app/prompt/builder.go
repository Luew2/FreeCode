package prompt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Luew2/FreeCode/internal/core/model"
)

const (
	basePrompt = `You are FreeCode, a pragmatic coding agent. Read before changing code, keep work scoped to the user's request, preserve user changes, and report what changed and what was verified.`

	developerPrompt = `Use tool calls when repository context is needed. Prefer concise answers. If you say you will inspect, run, edit, verify, or delegate something and the needed tool is available, call the tool in the same turn before giving the final answer. Do not end a turn with only a promise to do future work.

FreeCode can delegate bounded work when the spawn_agent tool is available. Do not claim that you cannot spawn subagents when that tool is present. Use spawn_agent for independent exploration, implementation, verification, review, or nested orchestration tasks, then synthesize the returned handoff for the user. Prefer delegation for complex coding tasks with separable research, edits, tests, or review. Keep the active orchestrator as the control plane: decide what to delegate, keep task boundaries clear, avoid duplicate work, and tell the user what changed or what each subagent found.

If the active turn context includes shared terminal output, treat it as user-visible local terminal output that was explicitly attached for your use. Read it before asking the user to paste or describe terminal state.

If terminal_read and terminal_write tools are available, the user has explicitly enabled direct terminal sharing with :st. If the user asks whether you can see the terminal, call terminal_read. If the user asks you to run a command in it, call terminal_write and then terminal_read. Do not merely say you will run a terminal command without using the terminal tools.

Do not invent unavailable tools. If write, shell, or delegation tools are not present in the provided tool list, say what you can do with the available tools.`

	permissionPrompt = `Reads are allowed only inside the workspace. Do not attempt path traversal. Writes, patch application, shell mutation, destructive git operations, and network tools are unavailable.`
)

type Environment struct {
	WorkspaceRoot string
	Shell         string
	GitBranch     string
	Platform      string
	WritableRoots []string
}

type Builder struct {
	Base        string
	Developer   string
	Permissions string
}

func NewBuilder() Builder {
	return Builder{
		Base:        basePrompt,
		Developer:   developerPrompt,
		Permissions: permissionPrompt,
	}
}

func (b Builder) Build(env Environment, userRequest string) []model.Message {
	base := strings.TrimSpace(firstNonEmpty(b.Base, basePrompt))
	developer := strings.TrimSpace(firstNonEmpty(b.Developer, developerPrompt))
	permissions := strings.TrimSpace(firstNonEmpty(b.Permissions, permissionPrompt))

	return []model.Message{
		model.TextMessage(model.RoleSystem, base),
		model.TextMessage(model.RoleDeveloper, developer),
		model.TextMessage(model.RoleDeveloper, permissions),
		model.TextMessage(model.RoleDeveloper, environmentPrompt(env)),
		model.TextMessage(model.RoleUser, strings.TrimSpace(userRequest)),
	}
}

func environmentPrompt(env Environment) string {
	writableRoots := append([]string(nil), env.WritableRoots...)
	sort.Strings(writableRoots)
	if len(writableRoots) == 0 {
		writableRoots = []string{"none"}
	}

	lines := []string{
		"Environment:",
		fmt.Sprintf("- workspace_root: %s", valueOrUnknown(env.WorkspaceRoot)),
		fmt.Sprintf("- shell: %s", valueOrUnknown(env.Shell)),
		fmt.Sprintf("- git_branch: %s", valueOrUnknown(env.GitBranch)),
		fmt.Sprintf("- platform: %s", valueOrUnknown(env.Platform)),
		fmt.Sprintf("- writable_roots: %s", strings.Join(writableRoots, ", ")),
	}
	return strings.Join(lines, "\n")
}

func firstNonEmpty(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}
