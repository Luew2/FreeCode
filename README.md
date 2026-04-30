# FreeCode

FreeCode is a Vim-shaped coding CLI: fast, modal, keyboard-first, and built around addressable artifacts. It is intended to make arbitrary inference endpoints boring to configure while keeping every model request, tool call, file edit, patch, and agent trace inspectable.

The current MVP has a provider-neutral Go core, OpenAI-compatible `/v1/chat/completions` and Anthropic-compatible `/v1/messages` support, read tools, preview-gated patch writes, JSONL session logs with recovery, Git status/diff commands, structured context compaction, swarm agents, persistent terminal sharing, diagnostics, an Ops board, and a Bubble Tea terminal workbench.

## Local Development

Run the test suite:

```sh
go test ./...
```

Print the CLI version:

```sh
go run ./cmd/freecode version
```

Open the terminal workbench:

```sh
go run ./cmd/freecode
```

Use the line-based fallback when piping scripted commands or debugging terminal behavior:

```sh
go run ./cmd/freecode --plain
```

Useful TUI shortcuts:

```text
i                   enter prompt composer
Enter               focus chat; open/collapse folders; edit files; in Git tab, show diff
Ctrl+K / ?          open searchable command palette and cheat sheet
j/k                 move selection
Ctrl+H/Ctrl+L       switch panes
left/right arrows   local horizontal action; right pane cycles tabs
h/l or [/]          local horizontal action; right pane cycles tabs
1/2/3/4/5           switch right pane tabs when the right pane is focused
ga / gt / gm / gf   jump to agents / transcript / messages / files panes
gd                  jump to active detail; opens approval inspector when pending
b                   return to the main orchestrator chat
o / e               open selected file in external Neovim; activate other selected items
d                   inspect selected item
y / Y               copy selected item without/with fences
v                   copy selection mode; mouse selection/Cmd+C also works on visible text
a / r               approve or reject selected pending action
:                   enter command mode
:ls / :buffers      list conversation buffers
:b main / :b a1     switch conversation buffer
:bn / :bp           next/previous conversation buffer
:sessions           list prior sessions for this workspace
:new                start a new session
:rename <title>     rename the current session
:resume <id>        resume a prior session
:files/:artifacts/:git/:ops
                    switch right pane tab
:edit README.md     open a workspace file in external Neovim
:agent <prompt>     send a directed follow-up to the active agent conversation
:s <prompt>         send a staged long-running swarm run
:swarm <prompt>     long form of :s
:! <command>        run a local-only shell command and inspect output
:term / :terminal   open the persistent local terminal
:t                  short form of :term
:st [n]             share terminal n with the agent; enables terminal_read/terminal_write
:st off             revoke direct terminal sharing
:share-term [n]     long form of :st
:sto [n]            attach recent terminal output to the next prompt without live control
:share-output [n]   long form of :sto
:settings           show provider/model/editor settings
:context            inspect what FreeCode will attach to the next model turn
:memory             inspect remembered structured context
:doctor             run local diagnostics
:debug-bundle       show a redacted bug-report bundle
:main / :back       return to the main orchestrator chat
:o f1:12            open file id at line 12
:y c1 / :Y c1       copy item by id without/with fences
:a p1 / :r p1       approve or reject pending action by id
:danger confirm     approve all tools for this session
:compact            write a compact session summary
:cancel             cancel the active run and clear queued prompts
:noqueue            clear queued prompts without cancelling the active run
q                   quit
```

File editing currently uses an external Neovim handoff. Configure `editor_command` in `.freecode/config.toml` when you want a specific Neovim invocation; `editor_double_esc` is available for the planned embedded editor and defaults off so Vim keeps ownership of `Esc`.

Inside the persistent terminal, the shell owns `Esc`. Use `Ctrl+G` to return focus to FreeCode while leaving the terminal running.

Check local repo and runtime status:

```sh
go run ./cmd/freecode doctor --config .freecode/config.toml
```

Export a redacted debug bundle for bug reports:

```sh
go run ./cmd/freecode debug-bundle --session .freecode/sessions/latest.jsonl --out .freecode/debug-bundle.txt
```

Run the local workflow regression harness without provider keys:

```sh
go run ./cmd/freecode bench
go run ./cmd/freecode bench --task copy-code-block
```

Add an OpenAI-compatible provider without probing:

```sh
go run ./cmd/freecode provider add --name local --base-url http://localhost:8000/v1 --api-key-env LOCAL_API_KEY --model coder --protocol openai-chat --context-window 128000 --max-output-tokens 8192 --skip-probe
```

Add an Anthropic-compatible provider:

```sh
go run ./cmd/freecode provider add --name anthropic --base-url https://api.anthropic.com/v1 --api-key-env ANTHROPIC_API_KEY --model claude-sonnet-4-5 --protocol anthropic-messages
```

Compact the current session log:

```sh
go run ./cmd/freecode compact
```

Ask a read-only repo question:

```sh
go run ./cmd/freecode ask "what is this repo?"
```

Allow a single headless `ask` run to use workspace-scoped patch writes:

```sh
go run ./cmd/freecode ask --allow-writes "update README.md with a short usage note"
```

The explicit approval form is also available:

```sh
go run ./cmd/freecode ask --approval auto --max-input-tokens 64000 --max-output-tokens 8192 "update README.md with a short usage note"
```

Run the bounded swarm flow:

```sh
go run ./cmd/freecode swarm --approval auto --max-input-tokens 64000 --max-output-tokens 8192 "implement the next scoped task and verify it"
```

Inspect local Git state and diffs:

```sh
go run ./cmd/freecode status
go run ./cmd/freecode diff
go run ./cmd/freecode diff README.md
```

Add an OpenAI-compatible provider without probing the network:

```sh
go run ./cmd/freecode provider add --name local --base-url https://api.example.com/v1 --api-key-env LOCAL_API_KEY --model coder --protocol openai-chat --skip-probe
```

List configured providers and models:

```sh
go run ./cmd/freecode provider list
```

## Safety Model

FreeCode treats the workspace as the trust boundary. Tool paths are resolved inside the workspace root, symlinks are checked, and `.git` / `.freecode` internals are not exposed by normal file listing. Default policy also denies common secret paths such as `.env`, SSH keys, PEM/key files, credential files, and secret-looking paths.

Approval modes:

```text
read-only   allow reads, deny writes/shell/network
ask         allow reads, ask for writes/shell/network
auto        allow reads and normal workspace writes, still ask for shell/network
danger      allow all tool classes after explicit :danger confirm
```

Patch writes are preview-first. A preview token is bound to the exact patch digest and changed files, expires automatically, and is consumed when applied. Patch application revalidates file contents under a workspace-level lock and rolls back partial writes when possible.

Terminal sharing is local-only until you explicitly run `:st [n]`. Once shared, the agent gets `terminal_read` and `terminal_write` for that terminal only; use `:st off` to revoke access. Use `:sto [n]` when you only want to attach recent terminal output to the next prompt without granting live terminal control. One-shot `:!` commands are local artifacts and are not sent to the model unless explicitly attached later.

Context compaction writes a structured handoff snapshot with the active objective, constraints, preferences, touched files, commands, approvals, agent state, pending work, and unresolved risks. Use `:context` to inspect what the next model turn will receive and `:memory` to inspect what FreeCode currently remembers.
