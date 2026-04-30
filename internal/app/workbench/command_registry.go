package workbench

import (
	"strings"
)

type CommandScope string

const (
	CommandScopeGlobal     CommandScope = "global"
	CommandScopeNormal     CommandScope = "normal"
	CommandScopeTranscript CommandScope = "transcript"
	CommandScopeContext    CommandScope = "context"
	CommandScopeAgents     CommandScope = "agents"
	CommandScopeTerminal   CommandScope = "terminal"
)

type CommandArgKind string

const (
	CommandArgNone         CommandArgKind = ""
	CommandArgPrompt       CommandArgKind = "prompt"
	CommandArgShell        CommandArgKind = "shell"
	CommandArgFile         CommandArgKind = "file"
	CommandArgSession      CommandArgKind = "session"
	CommandArgConversation CommandArgKind = "conversation"
	CommandArgTerminal     CommandArgKind = "terminal"
	CommandArgApproval     CommandArgKind = "approval"
	CommandArgModel        CommandArgKind = "model"
)

type CommandInvocation struct {
	ID     string
	Args   string
	Alias  string
	Source string
}

type CommandRegistry struct {
	commands []Command
	byID     map[string]Command
	aliases  map[string]string
	keys     map[CommandScope]map[string]string
}

func DefaultCommandRegistry() CommandRegistry {
	return NewCommandRegistry(DefaultCommands())
}

func NewCommandRegistry(commands []Command) CommandRegistry {
	registry := CommandRegistry{
		commands: append([]Command(nil), commands...),
		byID:     map[string]Command{},
		aliases:  map[string]string{},
		keys:     map[CommandScope]map[string]string{},
	}
	for _, command := range registry.commands {
		if strings.TrimSpace(command.ID) == "" {
			continue
		}
		registry.byID[command.ID] = command
		for _, alias := range commandAliases(command) {
			registry.aliases[normalizeCommandAlias(alias)] = command.ID
		}
		scopes := commandScopes(command)
		for _, key := range commandKeybindings(command) {
			key = strings.TrimSpace(key)
			if key == "" || strings.HasPrefix(key, ":") || strings.Contains(key, " ") {
				continue
			}
			for _, scope := range scopes {
				if registry.keys[scope] == nil {
					registry.keys[scope] = map[string]string{}
				}
				registry.keys[scope][strings.ToLower(key)] = command.ID
			}
		}
	}
	return registry
}

func (r CommandRegistry) Commands() []Command {
	return append([]Command(nil), r.commands...)
}

func (r CommandRegistry) Palette(query string) []Command {
	var commands []Command
	for _, command := range r.commands {
		commands = append(commands, command)
	}
	return FilterCommands(commands, query)
}

func (r CommandRegistry) ResolveLine(line string) (CommandInvocation, bool) {
	raw := strings.TrimSpace(line)
	if raw == "" || raw == ":" {
		return CommandInvocation{}, false
	}
	if strings.HasPrefix(raw, ":!") {
		return CommandInvocation{ID: "shell.run", Args: strings.TrimSpace(strings.TrimPrefix(raw, ":!")), Alias: "!", Source: "line"}, true
	}
	if strings.HasPrefix(raw, "!") {
		return CommandInvocation{ID: "shell.run", Args: strings.TrimSpace(strings.TrimPrefix(raw, "!")), Alias: "!", Source: "line"}, true
	}
	raw = strings.TrimPrefix(raw, ":")
	lowerRaw := strings.ToLower(raw)
	bestAlias := ""
	bestID := ""
	for alias, id := range r.aliases {
		if lowerRaw == alias || strings.HasPrefix(lowerRaw, alias+" ") {
			if len(alias) > len(bestAlias) {
				bestAlias = alias
				bestID = id
			}
		}
	}
	if bestID == "" {
		return CommandInvocation{}, false
	}
	args := strings.TrimSpace(raw[len(bestAlias):])
	return CommandInvocation{ID: bestID, Args: args, Alias: bestAlias, Source: "line"}, true
}

func (r CommandRegistry) ResolveKey(scope CommandScope, key string) (CommandInvocation, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return CommandInvocation{}, false
	}
	if id, ok := r.keys[scope][key]; ok {
		return CommandInvocation{ID: id, Alias: key, Source: "key"}, true
	}
	if id, ok := r.keys[CommandScopeGlobal][key]; ok {
		return CommandInvocation{ID: id, Alias: key, Source: "key"}, true
	}
	return CommandInvocation{}, false
}

func (r CommandRegistry) Complete(line string, state State) (string, bool) {
	prefix := strings.TrimLeft(strings.TrimSpace(line), ":")
	trailingSpace := strings.HasSuffix(line, " ")
	fields := strings.Fields(prefix)
	if len(fields) == 0 {
		return "", false
	}
	if len(fields) == 1 && !trailingSpace {
		if completion, ok := uniqueCommandCompletion(fields[0], r.commandNames()); ok {
			if command, exists := r.commandForAlias(completion); exists && commandNeedsArg(command) {
				completion += " "
			}
			return completion, true
		}
		return prefix, false
	}
	command, ok := r.commandForAlias(fields[0])
	if !ok {
		return prefix, false
	}
	argPrefix := ""
	if !trailingSpace && len(fields) > 1 {
		argPrefix = fields[len(fields)-1]
	}
	candidates := completionValuesForCommand(command, state)
	if len(candidates) == 0 {
		return prefix, false
	}
	if completion, ok := uniqueCommandCompletion(argPrefix, candidates); ok {
		return fields[0] + " " + completion, true
	}
	return prefix, false
}

func (r CommandRegistry) commandNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, command := range r.commands {
		for _, alias := range commandAliases(command) {
			alias = strings.TrimSpace(alias)
			if alias == "" || strings.HasPrefix(alias, "!") {
				continue
			}
			if fields := strings.Fields(alias); len(fields) > 0 {
				alias = fields[0]
			}
			key := strings.ToLower(alias)
			if seen[key] {
				continue
			}
			seen[key] = true
			names = append(names, alias)
		}
	}
	return names
}

func (r CommandRegistry) commandForAlias(alias string) (Command, bool) {
	id, ok := r.aliases[normalizeCommandAlias(alias)]
	if !ok {
		return Command{}, false
	}
	command, ok := r.byID[id]
	return command, ok
}

func commandAliases(command Command) []string {
	values := append([]string(nil), command.Aliases...)
	if strings.HasPrefix(command.Keybinding, ":") {
		values = append(values, splitKeybinding(command.Keybinding)...)
	}
	for _, key := range command.Keybindings {
		if strings.HasPrefix(key, ":") {
			values = append(values, key)
		}
	}
	return normalizeAliases(values)
}

func commandKeybindings(command Command) []string {
	values := append([]string(nil), command.Keybindings...)
	if command.Keybinding != "" {
		values = append(values, command.Keybinding)
	}
	return values
}

func commandScopes(command Command) []CommandScope {
	if len(command.Scopes) == 0 {
		return []CommandScope{CommandScopeGlobal}
	}
	var scopes []CommandScope
	for _, raw := range command.Scopes {
		scope := CommandScope(strings.TrimSpace(raw))
		if scope != "" {
			scopes = append(scopes, scope)
		}
	}
	if len(scopes) == 0 {
		scopes = append(scopes, CommandScopeGlobal)
	}
	return scopes
}

func normalizeAliases(values []string) []string {
	seen := map[string]bool{}
	var aliases []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		value = strings.TrimPrefix(value, ":")
		if value == "" {
			continue
		}
		value = strings.TrimSuffix(value, "!")
		fields := strings.Fields(value)
		var kept []string
		for _, field := range fields {
			if strings.HasPrefix(field, "<") || strings.HasPrefix(field, "[") {
				break
			}
			kept = append(kept, field)
		}
		value = strings.Join(kept, " ")
		if value == "" || seen[strings.ToLower(value)] {
			continue
		}
		seen[strings.ToLower(value)] = true
		aliases = append(aliases, value)
	}
	return aliases
}

func normalizeCommandAlias(alias string) string {
	alias = strings.TrimSpace(alias)
	alias = strings.TrimPrefix(alias, ":")
	alias = strings.TrimSuffix(alias, "!")
	return strings.ToLower(alias)
}

func commandNeedsArg(command Command) bool {
	switch CommandArgKind(command.ArgKind) {
	case CommandArgNone:
		return false
	default:
		return true
	}
}

func completionValuesForCommand(command Command, state State) []string {
	switch CommandArgKind(command.ArgKind) {
	case CommandArgFile:
		var values []string
		for _, file := range append(append([]WorkspaceFile(nil), state.Files...), state.GitFiles...) {
			for _, value := range []string{file.ID, file.Path, file.Name} {
				if strings.TrimSpace(value) != "" {
					values = append(values, value)
				}
			}
		}
		return values
	case CommandArgSession:
		var values []string
		for _, summary := range state.Sessions {
			if strings.TrimSpace(string(summary.ID)) != "" {
				values = append(values, string(summary.ID))
			}
		}
		return values
	case CommandArgConversation:
		values := []string{"main"}
		for _, agent := range state.Agents {
			for _, value := range []string{agent.ID, agent.TaskID, agent.Name} {
				if strings.TrimSpace(value) != "" {
					values = append(values, value)
				}
			}
		}
		return values
	default:
		return nil
	}
}

func uniqueCommandCompletion(prefix string, values []string) (string, bool) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return "", false
	}
	var matches []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(value), prefix) {
			matches = append(matches, value)
		}
	}
	if len(matches) != 1 {
		return "", false
	}
	return matches[0], true
}
