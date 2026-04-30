package toolregistry

import (
	"context"
	"fmt"
	"strings"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

type ToolProvider interface {
	Tools() []model.ToolSpec
	RunTool(context.Context, model.ToolCall) (ports.ToolResult, error)
	Close() error
}

type RegistryProvider struct {
	Registry  ports.ToolRegistry
	CloseFunc func() error
}

func FromRegistry(registry ports.ToolRegistry) ToolProvider {
	if registry == nil {
		return nil
	}
	return RegistryProvider{Registry: registry}
}

func (p RegistryProvider) Tools() []model.ToolSpec {
	if p.Registry == nil {
		return nil
	}
	return p.Registry.Tools()
}

func (p RegistryProvider) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	if p.Registry == nil {
		return ports.ToolResult{}, fmt.Errorf("tool registry is not configured")
	}
	return p.Registry.RunTool(ctx, call)
}

func (p RegistryProvider) Close() error {
	if p.CloseFunc == nil {
		return nil
	}
	return p.CloseFunc()
}

type CompositeToolRegistry struct {
	providers []ToolProvider
	routes    map[string]ToolProvider
	tools     []model.ToolSpec
}

var _ ports.ToolRegistry = (*CompositeToolRegistry)(nil)

func NewCompositeToolRegistry(providers ...ToolProvider) (*CompositeToolRegistry, error) {
	composite := &CompositeToolRegistry{
		routes: map[string]ToolProvider{},
	}
	for _, provider := range providers {
		if provider == nil {
			continue
		}
		for _, tool := range provider.Tools() {
			name := strings.TrimSpace(tool.Name)
			if name == "" {
				return nil, fmt.Errorf("tool provider exposed an unnamed tool")
			}
			if _, exists := composite.routes[name]; exists {
				return nil, fmt.Errorf("duplicate tool name %q", name)
			}
			tool.Name = name
			composite.routes[name] = provider
			composite.tools = append(composite.tools, tool)
		}
		composite.providers = append(composite.providers, provider)
	}
	return composite, nil
}

func MustComposite(providers ...ToolProvider) ports.ToolRegistry {
	registry, err := NewCompositeToolRegistry(providers...)
	if err != nil {
		panic(err)
	}
	return registry
}

func (r *CompositeToolRegistry) Tools() []model.ToolSpec {
	if r == nil {
		return nil
	}
	return append([]model.ToolSpec(nil), r.tools...)
}

func (r *CompositeToolRegistry) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	if r == nil {
		return ports.ToolResult{}, fmt.Errorf("tool registry is not configured")
	}
	provider, ok := r.routes[call.Name]
	if !ok {
		return ports.ToolResult{}, fmt.Errorf("unknown tool %q", call.Name)
	}
	return provider.RunTool(ctx, call)
}

func (r *CompositeToolRegistry) Close() error {
	if r == nil {
		return nil
	}
	var messages []string
	for _, provider := range r.providers {
		if err := provider.Close(); err != nil {
			messages = append(messages, err.Error())
		}
	}
	if len(messages) > 0 {
		return fmt.Errorf("close tool providers: %s", strings.Join(messages, "; "))
	}
	return nil
}

type filteredRegistry struct {
	inner ports.ToolRegistry
	deny  map[string]string
}

func FilterToolNames(inner ports.ToolRegistry, deny map[string]string) ports.ToolRegistry {
	if inner == nil || len(deny) == 0 {
		return inner
	}
	copyDeny := make(map[string]string, len(deny))
	for name, reason := range deny {
		name = strings.TrimSpace(name)
		if name != "" {
			copyDeny[name] = reason
		}
	}
	if len(copyDeny) == 0 {
		return inner
	}
	return filteredRegistry{inner: inner, deny: copyDeny}
}

func (r filteredRegistry) Tools() []model.ToolSpec {
	if r.inner == nil {
		return nil
	}
	var tools []model.ToolSpec
	for _, tool := range r.inner.Tools() {
		if _, denied := r.deny[tool.Name]; denied {
			continue
		}
		tools = append(tools, tool)
	}
	return tools
}

func (r filteredRegistry) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	if reason, denied := r.deny[call.Name]; denied {
		if strings.TrimSpace(reason) == "" {
			reason = "tool is disabled"
		}
		return ports.ToolResult{}, fmt.Errorf("%s: %s", call.Name, reason)
	}
	if r.inner == nil {
		return ports.ToolResult{}, fmt.Errorf("tool registry is not configured")
	}
	return r.inner.RunTool(ctx, call)
}
