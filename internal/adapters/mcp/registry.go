package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Luew2/FreeCode/internal/core/config"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/ports"
)

const (
	defaultStartupTimeout = 10 * time.Second
	defaultCallTimeout    = 60 * time.Second
	defaultMaxOutputBytes = 64 * 1024
)

type Manager struct {
	settings      config.MCPSettings
	workspaceRoot string
	version       string

	mu      sync.Mutex
	servers map[string]*serverRuntime
	routes  map[string]toolRuntime
	tools   []toolRuntime
}

type serverRuntime struct {
	name         string
	config       config.MCPServer
	state        string
	lastError    string
	serverInfo   string
	instructions string
	client       *Client
}

type toolRuntime struct {
	publicName   string
	serverName   string
	originalName string
	spec         model.ToolSpec
	capabilities []string
	unclassified bool
	server       *serverRuntime
}

type Options struct {
	Settings      config.MCPSettings
	WorkspaceRoot string
	Version       string
}

func NewManager(opts Options) *Manager {
	return &Manager{
		settings:      opts.Settings,
		workspaceRoot: opts.WorkspaceRoot,
		version:       opts.Version,
		servers:       map[string]*serverRuntime{},
		routes:        map[string]toolRuntime{},
	}
}

func (m *Manager) Start(ctx context.Context) error {
	if m == nil || !m.settings.Enabled {
		return nil
	}
	m.mu.Lock()
	m.servers = map[string]*serverRuntime{}
	m.routes = map[string]toolRuntime{}
	m.tools = nil
	m.mu.Unlock()

	names := make([]string, 0, len(m.settings.Servers))
	for name := range m.settings.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	publicNames := map[string]string{}
	for _, name := range names {
		cfg := m.settings.Servers[name]
		if !cfg.Enabled {
			continue
		}
		runtime := &serverRuntime{name: name, config: cfg, state: "starting"}
		m.setServer(runtime)
		tools, err := m.startServer(ctx, runtime)
		if err != nil {
			runtime.state = "failed"
			runtime.lastError = err.Error()
			m.setServer(runtime)
			continue
		}
		for _, tool := range tools {
			if existing := publicNames[tool.publicName]; existing != "" {
				runtime.state = "failed"
				runtime.lastError = fmt.Sprintf("duplicate MCP tool name %q also exposed by server %s", tool.publicName, existing)
				m.setServer(runtime)
				return fmt.Errorf("duplicate MCP tool name %q from servers %s and %s", tool.publicName, existing, runtime.name)
			}
			publicNames[tool.publicName] = runtime.name
		}
		runtime.state = "ready"
		m.setServer(runtime)
		m.addTools(tools)
	}
	return nil
}

func (m *Manager) Reload(ctx context.Context) error {
	if m == nil {
		return nil
	}
	_ = m.Close()
	return m.Start(ctx)
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	servers := make([]*serverRuntime, 0, len(m.servers))
	for _, server := range m.servers {
		servers = append(servers, server)
	}
	m.mu.Unlock()
	var messages []string
	for _, server := range servers {
		if server.client != nil {
			if err := server.client.Close(); err != nil {
				messages = append(messages, fmt.Sprintf("%s: %v", server.name, err))
			}
		}
		server.state = "closed"
		m.setServer(server)
	}
	if len(messages) > 0 {
		return fmt.Errorf("close MCP servers: %s", strings.Join(messages, "; "))
	}
	return nil
}

func (m *Manager) Provider(mode permission.Mode, gate ports.PermissionGate) *Provider {
	if m == nil || !m.settings.Enabled {
		return nil
	}
	return &Provider{manager: m, mode: mode, gate: gate}
}

func (m *Manager) Status(mode permission.Mode) ports.MCPStatus {
	status := ports.MCPStatus{Enabled: m != nil && m.settings.Enabled}
	if m == nil {
		return status
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	serverToolCounts := map[string]int{}
	for _, tool := range m.tools {
		serverToolCounts[tool.serverName]++
	}
	serverNames := make([]string, 0, len(m.servers))
	for name := range m.servers {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)
	for _, name := range serverNames {
		server := m.servers[name]
		stderr := ""
		if server.client != nil {
			stderr = server.client.Stderr()
		}
		status.Servers = append(status.Servers, ports.MCPServerStatus{
			Name:        server.name,
			State:       server.state,
			Command:     strings.Join(append([]string{server.config.Command}, server.config.Args...), " "),
			ToolCount:   serverToolCounts[name],
			LastError:   firstNonEmpty(server.lastError, clientLastError(server.client)),
			Stderr:      stderr,
			ServerInfo:  server.serverInfo,
			Instruction: server.instructions,
		})
	}
	for _, tool := range m.tools {
		visible, reason := toolVisible(tool, mode)
		status.Tools = append(status.Tools, ports.MCPToolStatus{
			PublicName:   tool.publicName,
			ServerName:   tool.serverName,
			OriginalName: tool.originalName,
			Visible:      visible,
			HiddenReason: reason,
			Capabilities: append([]string(nil), tool.capabilities...),
			Unclassified: tool.unclassified,
		})
	}
	return status
}

func (m *Manager) startServer(ctx context.Context, runtime *serverRuntime) ([]toolRuntime, error) {
	cfg := runtime.config
	startCtx, cancel := context.WithTimeout(ctx, timeoutDuration(cfg.StartupTimeoutMS, defaultStartupTimeout))
	defer cancel()
	transport, err := startStdio(startCtx, runtime.name, cfg.Command, cfg.Args, cfg.Env, cfg.WorkDir, m.workspaceRoot, 16*1024)
	if err != nil {
		return nil, err
	}
	client := newClient(runtime.name, m.version, transport, timeoutDuration(cfg.CallTimeoutMS, defaultCallTimeout))
	runtime.client = client
	initResult, err := client.Initialize(startCtx)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	runtime.serverInfo = strings.TrimSpace(strings.Join([]string{initResult.ServerInfo.Name, initResult.ServerInfo.Version}, " "))
	runtime.instructions = initResult.Instructions
	listed, err := client.ListTools(startCtx)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	tools := make([]toolRuntime, 0, len(listed))
	for _, listedTool := range listed {
		publicName, err := publicToolName(runtime.name, cfg.ToolsPrefix, listedTool.Name)
		if err != nil {
			_ = client.Close()
			return nil, err
		}
		capabilities, unclassified := toolCapabilities(cfg, listedTool.Name)
		description := strings.TrimSpace(listedTool.Description)
		suffix := fmt.Sprintf("MCP server: %s, tool: %s.", runtime.name, listedTool.Name)
		if description == "" {
			description = suffix
		} else {
			description += "\n\n" + suffix
		}
		tools = append(tools, toolRuntime{
			publicName:   publicName,
			serverName:   runtime.name,
			originalName: listedTool.Name,
			spec: model.ToolSpec{
				Name:        publicName,
				Description: description,
				InputSchema: listedTool.InputSchema,
			},
			capabilities: capabilities,
			unclassified: unclassified,
			server:       runtime,
		})
	}
	return tools, nil
}

func (m *Manager) setServer(server *serverRuntime) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.servers[server.name] = server
}

func (m *Manager) addTools(tools []toolRuntime) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tool := range tools {
		m.routes[tool.publicName] = tool
		m.tools = append(m.tools, tool)
	}
}

func (m *Manager) setServerLastError(name string, message string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if server := m.servers[name]; server != nil {
		server.lastError = message
	}
}

func (m *Manager) snapshotTools() []toolRuntime {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]toolRuntime(nil), m.tools...)
}

func (m *Manager) route(name string) (toolRuntime, bool) {
	if m == nil {
		return toolRuntime{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tool, ok := m.routes[name]
	return tool, ok
}

type Provider struct {
	manager *Manager
	mode    permission.Mode
	gate    ports.PermissionGate
}

func (p *Provider) Tools() []model.ToolSpec {
	if p == nil || p.manager == nil {
		return nil
	}
	var specs []model.ToolSpec
	for _, tool := range p.manager.snapshotTools() {
		if visible, _ := toolVisible(tool, p.mode); visible {
			specs = append(specs, tool.spec)
		}
	}
	return specs
}

func (p *Provider) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	if p == nil || p.manager == nil {
		return ports.ToolResult{}, fmt.Errorf("MCP tools are not configured")
	}
	tool, ok := p.manager.route(call.Name)
	if !ok {
		return ports.ToolResult{}, fmt.Errorf("unknown MCP tool %q", call.Name)
	}
	if err := p.checkPermission(ctx, tool); err != nil {
		return ports.ToolResult{}, err
	}
	var args map[string]any
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return ports.ToolResult{}, fmt.Errorf("decode MCP tool arguments for %s: %w", call.Name, err)
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	result, err := tool.server.client.CallTool(ctx, tool.originalName, args)
	if err != nil {
		p.manager.setServerLastError(tool.serverName, err.Error())
		return ports.ToolResult{}, err
	}
	content := normalizeToolResult(result, serverMaxOutput(tool.server))
	metadata := map[string]string{
		"mcp_server":       tool.serverName,
		"mcp_tool":         tool.originalName,
		"mcp_public_name":  tool.publicName,
		"mcp_capabilities": strings.Join(tool.capabilities, ","),
		"mcp_unclassified": fmt.Sprintf("%t", tool.unclassified),
		"mcp_result_error": fmt.Sprintf("%t", result.IsError),
	}
	return ports.ToolResult{CallID: call.ID, Content: content, Metadata: metadata}, nil
}

func (p *Provider) Close() error {
	return nil
}

func (p *Provider) checkPermission(ctx context.Context, tool toolRuntime) error {
	for _, action := range permissionActions(tool.capabilities) {
		decision := permission.PolicyForMode(p.mode).DecisionFor(action)
		if decision == permission.DecisionDeny {
			return fmt.Errorf("%s requires %s permission", tool.publicName, action)
		}
		if decision == permission.DecisionAsk {
			if p.gate == nil {
				return fmt.Errorf("%s requires %s approval but no approval gate is configured", tool.publicName, action)
			}
			request := permission.Request{
				Action:  action,
				Subject: tool.publicName,
				Reason:  permissionReason(tool, action),
			}
			granted, err := p.gate.Decide(ctx, request)
			if err != nil {
				return err
			}
			if granted != permission.DecisionAllow {
				return permission.ApprovalRequired(request)
			}
		}
	}
	return nil
}

func toolCapabilities(server config.MCPServer, toolName string) ([]string, bool) {
	if capabilities := server.ToolCapabilities[toolName]; len(capabilities) > 0 {
		return append([]string(nil), capabilities...), false
	}
	if len(server.Capabilities) > 0 {
		return append([]string(nil), server.Capabilities...), false
	}
	return []string{"network"}, true
}

func permissionActions(capabilities []string) []permission.Action {
	seen := map[permission.Action]bool{}
	var actions []permission.Action
	add := func(action permission.Action) {
		if !seen[action] {
			seen[action] = true
			actions = append(actions, action)
		}
	}
	for _, capability := range capabilities {
		switch capability {
		case "read_workspace":
			add(permission.ActionRead)
		case "write_workspace":
			add(permission.ActionWrite)
		case "shell":
			add(permission.ActionShell)
		case "network", "external_write":
			add(permission.ActionNetwork)
		case "destructive_git":
			add(permission.ActionDestructiveGit)
		default:
			add(permission.ActionNetwork)
		}
	}
	if len(actions) == 0 {
		add(permission.ActionNetwork)
	}
	return actions
}

func toolVisible(tool toolRuntime, mode permission.Mode) (bool, string) {
	if mode != permission.ModeReadOnly {
		return true, ""
	}
	for _, action := range permissionActions(tool.capabilities) {
		if action != permission.ActionRead {
			return false, "hidden in read-only mode; requires " + string(action)
		}
	}
	return true, ""
}

func permissionReason(tool toolRuntime, action permission.Action) string {
	var details []string
	details = append(details, fmt.Sprintf("MCP server %s tool %s", tool.serverName, tool.originalName))
	if tool.unclassified {
		details = append(details, "unclassified MCP tool defaults to network permission")
	}
	if contains(tool.capabilities, "external_write") {
		details = append(details, "may write to an external service")
	}
	details = append(details, "requires "+string(action))
	return strings.Join(details, "; ")
}

func normalizeToolResult(result callToolResult, limit int) string {
	var sections []string
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				sections = append(sections, block.Text)
			}
		case "image", "audio":
			label := block.Type
			if block.MIMEType != "" {
				label += " " + block.MIMEType
			}
			sections = append(sections, "["+label+" omitted]")
		case "resource", "resource_link":
			label := firstNonEmpty(block.URI, block.Name, "resource")
			sections = append(sections, "[resource omitted: "+label+"]")
		default:
			if block.Text != "" {
				sections = append(sections, block.Text)
			} else {
				sections = append(sections, "[unsupported MCP content block: "+block.Type+"]")
			}
		}
	}
	if len(result.StructuredContent) > 0 {
		if data, err := json.Marshal(result.StructuredContent); err == nil {
			sections = append(sections, string(data))
		}
	}
	if len(sections) == 0 {
		sections = append(sections, "MCP tool completed with no output.")
	}
	content := strings.Join(sections, "\n\n")
	if limit <= 0 {
		limit = defaultMaxOutputBytes
	}
	if len(content) > limit {
		content = content[:limit] + "\n\n[truncated MCP tool output]"
	}
	return content
}

func serverMaxOutput(server *serverRuntime) int {
	if server == nil || server.config.MaxOutputBytes <= 0 {
		return defaultMaxOutputBytes
	}
	return server.config.MaxOutputBytes
}

func clientLastError(client *Client) string {
	if client == nil {
		return ""
	}
	return client.LastError()
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
