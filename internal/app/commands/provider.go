package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

const (
	ProviderProtocolAuto              = "auto"
	ProviderProtocolOpenAIChat        = "openai-chat"
	ProviderProtocolAnthropicMessages = "anthropic-messages"
)

var envVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type ProviderAddOptions struct {
	Name            string
	BaseURL         string
	APIKeyEnv       string
	Model           string
	Protocol        string
	ContextWindow   int
	MaxOutputTokens int
	SkipProbe       bool
}

func AddProvider(ctx context.Context, w io.Writer, store ports.ConfigStore, probe ports.ProtocolProbe, opts ProviderAddOptions) error {
	if err := validateProviderAddOptions(opts); err != nil {
		return err
	}

	settings, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if settings.Providers == nil {
		settings.Providers = map[model.ProviderID]model.Provider{}
	}
	if settings.Models == nil {
		settings.Models = map[model.Ref]model.Model{}
	}

	providerID := model.ProviderID(opts.Name)
	modelID := model.ModelID(opts.Model)
	protocol := requestedProtocol(opts.Protocol)
	storedProtocol := protocol
	if opts.SkipProbe {
		storedProtocol = defaultProtocol(protocol)
	}
	provider := model.Provider{
		ID:           providerID,
		Name:         opts.Name,
		Protocol:     model.ProtocolID(storedProtocol),
		BaseURL:      opts.BaseURL,
		Secret:       model.SecretRef{Name: opts.APIKeyEnv, Source: "env"},
		DefaultModel: modelID,
		Enabled:      true,
	}
	configuredModel := model.NewModel(providerID, modelID)
	configuredModel.Limits.ContextWindow = opts.ContextWindow
	configuredModel.Limits.MaxOutputTokens = opts.MaxOutputTokens

	if !opts.SkipProbe {
		provider.Protocol = model.ProtocolID(protocol)
		if probe == nil {
			return errors.New("provider probe is not configured")
		}
		result, err := probe.Probe(ctx, provider, modelID)
		if err != nil {
			return err
		}
		provider.Protocol = result.Protocol
		provider.Metadata = copyMetadata(result.Metadata)
		configuredModel = result.Model
		configuredModel.Ref = model.NewRef(providerID, modelID)
		if configuredModel.Name == "" {
			configuredModel.Name = opts.Model
		}
		if opts.ContextWindow > 0 {
			configuredModel.Limits.ContextWindow = opts.ContextWindow
		}
		if opts.MaxOutputTokens > 0 {
			configuredModel.Limits.MaxOutputTokens = opts.MaxOutputTokens
		}
	}

	ref := model.NewRef(providerID, modelID)
	settings.Providers[providerID] = provider
	settings.Models[ref] = configuredModel
	if settings.ActiveModel == (model.Ref{}) {
		settings.ActiveModel = ref
	}

	if err := store.Save(ctx, settings); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "added provider %s model %s using %s\n", opts.Name, opts.Model, provider.Protocol)
	return err
}

func ListProviders(ctx context.Context, w io.Writer, store ports.ConfigStore) error {
	settings, err := store.Load(ctx)
	if err != nil {
		return err
	}

	ids := make([]string, 0, len(settings.Providers))
	for id := range settings.Providers {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tPROTOCOL\tBASE URL\tMODELS"); err != nil {
		return err
	}
	for _, id := range ids {
		provider := settings.Providers[model.ProviderID(id)]
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", id, provider.Protocol, provider.BaseURL, strings.Join(modelIDsForProvider(settings.Models, model.ProviderID(id), provider.DefaultModel), ", ")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func validateProviderAddOptions(opts ProviderAddOptions) error {
	if strings.TrimSpace(opts.Name) == "" {
		return errors.New("--name is required")
	}
	if strings.Contains(opts.Name, "/") {
		return errors.New("--name must not contain /")
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return errors.New("--base-url is required")
	}
	if strings.TrimSpace(opts.APIKeyEnv) == "" {
		return errors.New("--api-key-env is required")
	}
	if !envVarNamePattern.MatchString(opts.APIKeyEnv) {
		return errors.New("--api-key-env must be an environment variable name, not an API key value")
	}
	if strings.TrimSpace(opts.Model) == "" {
		return errors.New("--model is required")
	}
	if opts.ContextWindow < 0 {
		return errors.New("--context-window must not be negative")
	}
	if opts.MaxOutputTokens < 0 {
		return errors.New("--max-output-tokens must not be negative")
	}
	switch opts.Protocol {
	case "", ProviderProtocolAuto, ProviderProtocolOpenAIChat, ProviderProtocolAnthropicMessages:
		return nil
	default:
		return fmt.Errorf("--protocol must be %s, %s, or %s", ProviderProtocolOpenAIChat, ProviderProtocolAnthropicMessages, ProviderProtocolAuto)
	}
}

func requestedProtocol(protocol string) string {
	if protocol == "" || protocol == ProviderProtocolAuto {
		return ProviderProtocolAuto
	}
	return protocol
}

func defaultProtocol(protocol string) string {
	if protocol == "" || protocol == ProviderProtocolAuto {
		return ProviderProtocolOpenAIChat
	}
	return protocol
}

func modelIDsForProvider(models map[model.Ref]model.Model, providerID model.ProviderID, fallback model.ModelID) []string {
	ids := []string{}
	for ref := range models {
		if ref.Provider == providerID {
			ids = append(ids, string(ref.ID))
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 && fallback != "" {
		return []string{string(fallback)}
	}
	return ids
}

func copyMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	copied := make(map[string]string, len(metadata))
	for key, value := range metadata {
		copied[key] = value
	}
	return copied
}
