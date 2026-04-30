package tomlconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/Luew2/FreeCode/internal/core/agent"
	"github.com/Luew2/FreeCode/internal/core/config"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
)

const DefaultPath = ".freecode/config.toml"

type Store struct {
	path string
}

func New(path string) *Store {
	if path == "" {
		path = DefaultPath
	}
	return &Store{path: path}
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) Load(ctx context.Context) (config.Settings, error) {
	if err := ctx.Err(); err != nil {
		return config.Settings{}, err
	}

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return config.DefaultSettings(), nil
	}
	if err != nil {
		return config.Settings{}, fmt.Errorf("read config %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return config.DefaultSettings(), nil
	}

	var dto settingsDTO
	if err := toml.Unmarshal(data, &dto); err != nil {
		return config.Settings{}, fmt.Errorf("parse config %s: %w", s.path, err)
	}
	settings, err := dto.toCore()
	if err != nil {
		return config.Settings{}, fmt.Errorf("convert config %s: %w", s.path, err)
	}
	return settings, nil
}

func (s *Store) Save(ctx context.Context, settings config.Settings) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	dto := settingsDTOFromCore(settings)
	data, err := toml.Marshal(dto)
	if err != nil {
		return fmt.Errorf("encode config %s: %w", s.path, err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory %s: %w", dir, err)
	}
	return atomicWrite(s.path, data, 0o600)
}

// atomicWrite writes data to a sibling temp file in the target directory,
// fsyncs it, closes it, then renames over the target. The previous
// implementation used os.WriteFile which truncates first and writes second
// — an interrupted save (SIGKILL, power loss, OOM) left the user with an
// empty or half-written config. Same-directory rename is atomic on POSIX
// and on NTFS via ReplaceFileW; Go's os.Rename uses the right primitive.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if anything below fails before the rename.
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp file %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp file %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp file %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp file over %s: %w", path, err)
	}
	return nil
}

type settingsDTO struct {
	Version         int                    `toml:"version"`
	ActiveModel     string                 `toml:"active_model,omitempty"`
	SessionsDir     string                 `toml:"sessions_dir,omitempty"`
	EditorCommand   string                 `toml:"editor_command,omitempty"`
	EditorDoubleEsc bool                   `toml:"editor_double_esc,omitempty"`
	Permissions     *permissionDTO         `toml:"permissions,omitempty"`
	MCP             *mcpDTO                `toml:"mcp,omitempty"`
	Providers       map[string]providerDTO `toml:"providers,omitempty"`
	Models          map[string]modelDTO    `toml:"models,omitempty"`
	Agents          []agentDTO             `toml:"agents,omitempty"`
}

type providerDTO struct {
	Name         string            `toml:"name,omitempty"`
	Protocol     string            `toml:"protocol"`
	BaseURL      string            `toml:"base_url"`
	APIKeyEnv    string            `toml:"api_key_env,omitempty"`
	SecretSource string            `toml:"secret_source,omitempty"`
	DefaultModel string            `toml:"default_model,omitempty"`
	Enabled      *bool             `toml:"enabled,omitempty"`
	Metadata     map[string]string `toml:"metadata,omitempty"`
}

type modelDTO struct {
	Name                string            `toml:"name,omitempty"`
	Provider            string            `toml:"provider"`
	ID                  string            `toml:"id"`
	ContextWindow       int               `toml:"context_window,omitempty"`
	MaxOutputTokens     int               `toml:"max_output_tokens,omitempty"`
	SupportsTools       bool              `toml:"supports_tools,omitempty"`
	SupportsStreaming   bool              `toml:"supports_streaming,omitempty"`
	SupportsJSONOutput  bool              `toml:"supports_json_output,omitempty"`
	SupportsVision      bool              `toml:"supports_vision,omitempty"`
	SupportsEmbeddings  bool              `toml:"supports_embeddings,omitempty"`
	SupportsReasoning   bool              `toml:"supports_reasoning,omitempty"`
	SupportsAttachments bool              `toml:"supports_attachments,omitempty"`
	Enabled             *bool             `toml:"enabled,omitempty"`
	Metadata            map[string]string `toml:"metadata,omitempty"`
}

type agentDTO struct {
	Name        string        `toml:"name"`
	Role        string        `toml:"role"`
	Description string        `toml:"description,omitempty"`
	Model       string        `toml:"model,omitempty"`
	Permissions permissionDTO `toml:"permissions"`
	Flow        string        `toml:"flow"`
	MaxSteps    int           `toml:"max_steps"`
}

type permissionDTO struct {
	Read           string   `toml:"read,omitempty"`
	Write          string   `toml:"write,omitempty"`
	Shell          string   `toml:"shell,omitempty"`
	Network        string   `toml:"network,omitempty"`
	DestructiveGit string   `toml:"destructive_git,omitempty"`
	AllowedPaths   []string `toml:"allowed_paths,omitempty"`
	DeniedPaths    []string `toml:"denied_paths,omitempty"`
}

type mcpDTO struct {
	Enabled bool                    `toml:"enabled,omitempty"`
	Servers map[string]mcpServerDTO `toml:"servers,omitempty"`
}

type mcpServerDTO struct {
	Enabled          *bool               `toml:"enabled,omitempty"`
	Transport        string              `toml:"transport,omitempty"`
	Command          string              `toml:"command,omitempty"`
	Args             []string            `toml:"args,omitempty"`
	Env              []string            `toml:"env,omitempty"`
	WorkDir          string              `toml:"work_dir,omitempty"`
	ToolsPrefix      string              `toml:"tools_prefix,omitempty"`
	Capabilities     []string            `toml:"capabilities,omitempty"`
	ToolCapabilities map[string][]string `toml:"tool_capabilities,omitempty"`
	StartupTimeoutMS int                 `toml:"startup_timeout_ms,omitempty"`
	CallTimeoutMS    int                 `toml:"call_timeout_ms,omitempty"`
	MaxOutputBytes   int                 `toml:"max_output_bytes,omitempty"`
}

func settingsDTOFromCore(settings config.Settings) settingsDTO {
	dto := settingsDTO{
		Version:         settings.Version,
		ActiveModel:     settings.ActiveModel.String(),
		SessionsDir:     settings.SessionsDir,
		EditorCommand:   settings.EditorCommand,
		EditorDoubleEsc: settings.EditorDoubleEsc,
		Providers:       providersDTOFromCore(settings.Providers),
		Models:          modelsDTOFromCore(settings.Models),
	}
	if dto.Version == 0 {
		dto.Version = config.CurrentVersion
	}
	if dto.SessionsDir == config.DefaultSettings().SessionsDir {
		dto.SessionsDir = ""
	}
	if dto.EditorCommand == config.DefaultSettings().EditorCommand {
		dto.EditorCommand = ""
	}
	if !reflect.DeepEqual(settings.Permissions, permission.DefaultPolicy()) {
		permissions := permissionDTOFromCore(settings.Permissions)
		dto.Permissions = &permissions
	}
	if !reflect.DeepEqual(settings.Agents, agent.DefaultDefinitions()) {
		dto.Agents = agentsDTOFromCore(settings.Agents)
	}
	if settings.MCP.Enabled || len(settings.MCP.Servers) > 0 {
		mcp := mcpDTOFromCore(settings.MCP)
		dto.MCP = &mcp
	}
	return dto
}

func (dto settingsDTO) toCore() (config.Settings, error) {
	settings := config.DefaultSettings()
	if dto.Version > config.CurrentVersion {
		return config.Settings{}, fmt.Errorf("config version %d is newer than supported version %d", dto.Version, config.CurrentVersion)
	}
	if dto.Version != 0 {
		settings.Version = dto.Version
	}
	if dto.SessionsDir != "" {
		settings.SessionsDir = dto.SessionsDir
	}
	if dto.EditorCommand != "" {
		settings.EditorCommand = dto.EditorCommand
	}
	settings.EditorDoubleEsc = dto.EditorDoubleEsc
	if dto.Permissions != nil {
		settings.Permissions = dto.Permissions.toCore()
	}
	if dto.MCP != nil {
		mcp, err := dto.MCP.toCore()
		if err != nil {
			return config.Settings{}, err
		}
		settings.MCP = mcp
	}

	providers, err := dto.providersToCore()
	if err != nil {
		return config.Settings{}, err
	}
	settings.Providers = providers

	models, err := dto.modelsToCore()
	if err != nil {
		return config.Settings{}, err
	}
	settings.Models = models

	agents, err := dto.agentsToCore()
	if err != nil {
		return config.Settings{}, err
	}
	if dto.Agents != nil {
		settings.Agents = agents
	}

	if dto.ActiveModel != "" {
		ref, err := model.ParseRef(dto.ActiveModel)
		if err != nil {
			return config.Settings{}, fmt.Errorf("active_model: %w", err)
		}
		settings.ActiveModel = ref
	}
	return settings, nil
}

func providersDTOFromCore(providers map[model.ProviderID]model.Provider) map[string]providerDTO {
	if len(providers) == 0 {
		return map[string]providerDTO{}
	}
	dto := make(map[string]providerDTO, len(providers))
	for id, provider := range providers {
		key := string(id)
		if key == "" {
			key = string(provider.ID)
		}
		enabled := provider.Enabled
		valueDTO := providerDTO{
			Name:         provider.Name,
			Protocol:     string(provider.Protocol),
			BaseURL:      provider.BaseURL,
			APIKeyEnv:    provider.Secret.Name,
			SecretSource: provider.Secret.Source,
			DefaultModel: string(provider.DefaultModel),
			Metadata:     copyStringMap(provider.Metadata),
		}
		if valueDTO.Name == key {
			valueDTO.Name = ""
		}
		if valueDTO.SecretSource == "env" {
			valueDTO.SecretSource = ""
		}
		if !enabled {
			valueDTO.Enabled = &enabled
		}
		dto[key] = valueDTO
	}
	return dto
}

func (dto settingsDTO) providersToCore() (map[model.ProviderID]model.Provider, error) {
	providers := map[model.ProviderID]model.Provider{}
	for key, value := range dto.Providers {
		if key == "" {
			return nil, errors.New("providers contains empty key")
		}
		id := model.ProviderID(key)
		provider := model.Provider{
			ID:           id,
			Name:         value.Name,
			Protocol:     model.ProtocolID(value.Protocol),
			BaseURL:      value.BaseURL,
			Secret:       model.SecretRef{Name: value.APIKeyEnv, Source: value.SecretSource},
			DefaultModel: model.ModelID(value.DefaultModel),
			Enabled:      true,
			Metadata:     copyStringMap(value.Metadata),
		}
		if value.Enabled != nil {
			provider.Enabled = *value.Enabled
		}
		if provider.Name == "" {
			provider.Name = key
		}
		if provider.Secret.Source == "" && provider.Secret.Name != "" {
			provider.Secret.Source = "env"
		}
		providers[id] = provider
	}
	return providers, nil
}

func modelsDTOFromCore(models map[model.Ref]model.Model) map[string]modelDTO {
	if len(models) == 0 {
		return map[string]modelDTO{}
	}
	dto := make(map[string]modelDTO, len(models))
	for ref, candidate := range models {
		key := ref.String()
		if key == "" {
			key = candidate.Ref.String()
		}
		candidateRef := candidate.Ref
		if candidateRef == (model.Ref{}) {
			candidateRef = ref
		}
		enabled := candidate.Enabled
		valueDTO := modelDTO{
			Name:                candidate.Name,
			Provider:            string(candidateRef.Provider),
			ID:                  string(candidateRef.ID),
			ContextWindow:       candidate.Limits.ContextWindow,
			MaxOutputTokens:     candidate.Limits.MaxOutputTokens,
			SupportsTools:       candidate.Capabilities.Tools,
			SupportsStreaming:   candidate.Capabilities.Streaming,
			SupportsJSONOutput:  candidate.Capabilities.JSONOutput,
			SupportsVision:      candidate.Capabilities.Vision,
			SupportsEmbeddings:  candidate.Capabilities.Embeddings,
			SupportsReasoning:   candidate.Capabilities.Reasoning,
			SupportsAttachments: candidate.Capabilities.Attachments,
			Metadata:            copyStringMap(candidate.Metadata),
		}
		if !enabled {
			valueDTO.Enabled = &enabled
		}
		dto[key] = valueDTO
	}
	return dto
}

func (dto settingsDTO) modelsToCore() (map[model.Ref]model.Model, error) {
	models := map[model.Ref]model.Model{}
	keys := make([]string, 0, len(dto.Models))
	for key := range dto.Models {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := dto.Models[key]
		ref, err := model.ParseRef(key)
		if err != nil {
			return nil, fmt.Errorf("models.%q: %w", key, err)
		}
		if value.Provider != "" || value.ID != "" {
			explicitRef := model.NewRef(model.ProviderID(value.Provider), model.ModelID(value.ID))
			if explicitRef != ref {
				return nil, fmt.Errorf("models.%q provider/id mismatch with %q/%q", key, value.Provider, value.ID)
			}
		}
		candidate := model.Model{
			Ref:  ref,
			Name: value.Name,
			Capabilities: model.Capabilities{
				Tools:       value.SupportsTools,
				Streaming:   value.SupportsStreaming,
				JSONOutput:  value.SupportsJSONOutput,
				Vision:      value.SupportsVision,
				Embeddings:  value.SupportsEmbeddings,
				Reasoning:   value.SupportsReasoning,
				Attachments: value.SupportsAttachments,
			},
			Limits: model.Limits{
				ContextWindow:   value.ContextWindow,
				MaxOutputTokens: value.MaxOutputTokens,
			},
			Enabled:  true,
			Metadata: copyStringMap(value.Metadata),
		}
		if value.Enabled != nil {
			candidate.Enabled = *value.Enabled
		}
		if candidate.Name == "" {
			candidate.Name = string(ref.ID)
		}
		models[ref] = candidate
	}
	return models, nil
}

func agentsDTOFromCore(agents []agent.Definition) []agentDTO {
	if len(agents) == 0 {
		return nil
	}
	dto := make([]agentDTO, 0, len(agents))
	for _, definition := range agents {
		dto = append(dto, agentDTO{
			Name:        definition.Name,
			Role:        string(definition.Role),
			Description: definition.Description,
			Model:       definition.Model.String(),
			Permissions: permissionDTOFromCore(definition.Permissions),
			Flow:        string(definition.Flow),
			MaxSteps:    definition.MaxSteps,
		})
	}
	return dto
}

func (dto settingsDTO) agentsToCore() ([]agent.Definition, error) {
	agents := make([]agent.Definition, 0, len(dto.Agents))
	for _, value := range dto.Agents {
		var ref model.Ref
		if value.Model != "" {
			parsed, err := model.ParseRef(value.Model)
			if err != nil {
				return nil, fmt.Errorf("agent %q model: %w", value.Name, err)
			}
			ref = parsed
		}
		agents = append(agents, agent.Definition{
			Name:        value.Name,
			Role:        agent.Role(value.Role),
			Description: value.Description,
			Model:       ref,
			Permissions: value.Permissions.toCore(),
			Flow:        agent.Flow(value.Flow),
			MaxSteps:    value.MaxSteps,
		})
	}
	return agents, nil
}

func permissionDTOFromCore(policy permission.Policy) permissionDTO {
	return permissionDTO{
		Read:           string(policy.Read),
		Write:          string(policy.Write),
		Shell:          string(policy.Shell),
		Network:        string(policy.Network),
		DestructiveGit: string(policy.DestructiveGit),
		AllowedPaths:   append([]string(nil), policy.AllowedPaths...),
		DeniedPaths:    append([]string(nil), policy.DeniedPaths...),
	}
}

func (dto permissionDTO) toCore() permission.Policy {
	return permission.Policy{
		Read:           permission.Decision(dto.Read),
		Write:          permission.Decision(dto.Write),
		Shell:          permission.Decision(dto.Shell),
		Network:        permission.Decision(dto.Network),
		DestructiveGit: permission.Decision(dto.DestructiveGit),
		AllowedPaths:   append([]string(nil), dto.AllowedPaths...),
		DeniedPaths:    append([]string(nil), dto.DeniedPaths...),
	}
}

func mcpDTOFromCore(settings config.MCPSettings) mcpDTO {
	dto := mcpDTO{
		Enabled: settings.Enabled,
		Servers: map[string]mcpServerDTO{},
	}
	for name, server := range settings.Servers {
		enabled := server.Enabled
		value := mcpServerDTO{
			Transport:        server.Transport,
			Command:          server.Command,
			Args:             append([]string(nil), server.Args...),
			Env:              append([]string(nil), server.Env...),
			WorkDir:          server.WorkDir,
			ToolsPrefix:      server.ToolsPrefix,
			Capabilities:     append([]string(nil), server.Capabilities...),
			ToolCapabilities: copyStringSliceMap(server.ToolCapabilities),
			StartupTimeoutMS: server.StartupTimeoutMS,
			CallTimeoutMS:    server.CallTimeoutMS,
			MaxOutputBytes:   server.MaxOutputBytes,
		}
		if !enabled {
			value.Enabled = &enabled
		}
		dto.Servers[name] = value
	}
	return dto
}

func (dto mcpDTO) toCore() (config.MCPSettings, error) {
	settings := config.MCPSettings{
		Enabled: dto.Enabled,
		Servers: map[string]config.MCPServer{},
	}
	for name, value := range dto.Servers {
		if name == "" {
			return config.MCPSettings{}, errors.New("mcp.servers contains empty key")
		}
		enabled := true
		if value.Enabled != nil {
			enabled = *value.Enabled
		}
		server := config.MCPServer{
			Enabled:          enabled,
			Transport:        value.Transport,
			Command:          value.Command,
			Args:             append([]string(nil), value.Args...),
			Env:              append([]string(nil), value.Env...),
			WorkDir:          value.WorkDir,
			ToolsPrefix:      value.ToolsPrefix,
			Capabilities:     append([]string(nil), value.Capabilities...),
			ToolCapabilities: copyStringSliceMap(value.ToolCapabilities),
			StartupTimeoutMS: value.StartupTimeoutMS,
			CallTimeoutMS:    value.CallTimeoutMS,
			MaxOutputBytes:   value.MaxOutputBytes,
		}
		if server.Transport == "" {
			server.Transport = "stdio"
		}
		if err := validateMCPServer(name, server); err != nil {
			return config.MCPSettings{}, err
		}
		settings.Servers[name] = server
	}
	return settings, nil
}

func validateMCPServer(name string, server config.MCPServer) error {
	if !server.Enabled {
		return nil
	}
	if server.Transport != "stdio" {
		return fmt.Errorf("mcp.servers.%s transport %q is unsupported; only stdio is supported", name, server.Transport)
	}
	if server.Command == "" {
		return fmt.Errorf("mcp.servers.%s command is required", name)
	}
	if sanitizeMCPName(firstNonEmpty(server.ToolsPrefix, name)) == "" {
		return fmt.Errorf("mcp.servers.%s tools_prefix must contain at least one letter or digit", name)
	}
	for _, capability := range server.Capabilities {
		if !knownMCPCapability(capability) {
			return fmt.Errorf("mcp.servers.%s capability %q is unknown", name, capability)
		}
	}
	for tool, capabilities := range server.ToolCapabilities {
		if tool == "" {
			return fmt.Errorf("mcp.servers.%s tool_capabilities contains empty tool name", name)
		}
		for _, capability := range capabilities {
			if !knownMCPCapability(capability) {
				return fmt.Errorf("mcp.servers.%s tool %s capability %q is unknown", name, tool, capability)
			}
		}
	}
	return nil
}

func knownMCPCapability(value string) bool {
	switch value {
	case "read_workspace", "write_workspace", "shell", "network", "destructive_git", "external_write":
		return true
	default:
		return false
	}
}

func sanitizeMCPName(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '_' || r == '-':
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func copyStringSliceMap(values map[string][]string) map[string][]string {
	if len(values) == 0 {
		return nil
	}
	copied := make(map[string][]string, len(values))
	for key, value := range values {
		copied[key] = append([]string(nil), value...)
	}
	return copied
}
