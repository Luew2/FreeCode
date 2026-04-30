package tomlconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

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
