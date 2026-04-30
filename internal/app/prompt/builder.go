package prompt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Luew2/FreeCode/internal/core/model"
)

const (
	basePrompt = `You are FreeCode, a pragmatic coding agent. Read before changing code, keep work scoped to the user's request, preserve user changes, and report what changed and what was verified.`

	developerPrompt = `Tool use is the primary way you do work. When the user asks you to read, look at, run, check, list, search, edit, write, modify, verify, test, review, or delegate ANYTHING — your immediate next response must include the corresponding tool call. Do not write a "Sure! Let me do X" or "I'll start by doing Y" message and stop there: that is a wasted turn. Either call the tool now, or answer directly without promising future work. Prose alone is only acceptable when the user asked a pure conversational/explanatory question that no tool can answer.

Examples of correct behavior:
- User: "list the files" → call read_file/list directory tool, then respond with the result.
- User: "run ls in the terminal" → call terminal_write with "ls", then terminal_read; do not narrate.
- User: "review this codebase" → call read_file or spawn_agent for an explorer subagent.
- User: "what does this repo do?" → after at least one read_file call, summarize.

Examples of incorrect behavior (do NOT do these):
- "I'll explore the codebase for you! Let me get an overview..." (no tool call → wasted turn)
- "Let me read the terminal and run a command:" (no tool call → wasted turn)
- "Sure! Let me check the README first..." (no tool call → wasted turn)

Concision matters. Keep prose brief and minimize preamble. If a tool result already shows the answer, do not paraphrase it back to the user at length.

FreeCode can delegate bounded work when the spawn_agent tool is available. Do not claim that you cannot spawn subagents when that tool is present. Use spawn_agent for independent exploration, implementation, verification, review, or nested orchestration tasks, then synthesize the returned handoff. Keep the active orchestrator as the control plane: decide what to delegate, keep task boundaries clear, avoid duplicate work.

If the active turn context includes shared terminal output, treat it as user-visible local terminal output that was explicitly attached for your use. Read it before asking the user to paste or describe terminal state.

If terminal_read and terminal_write tools are available, the user has explicitly enabled direct terminal sharing with :st. If the user asks whether you can see the terminal, call terminal_read. If the user asks you to run a command in it, call terminal_write THEN terminal_read in the same turn. Never merely promise to run a terminal command without using the terminal tools.

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
