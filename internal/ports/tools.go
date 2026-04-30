package ports

import (
	"context"

	"github.com/Luew2/FreeCode/internal/core/artifact"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
)

type Tool interface {
	Name() string
	Description() string
	InputSchema() map[string]any
}

type ToolRegistry interface {
	Tools() []model.ToolSpec
	ToolRunner
}

type ToolRunner interface {
	RunTool(ctx context.Context, call model.ToolCall) (ToolResult, error)
}

type ToolResult struct {
	CallID   string
	Content  string
	Artifact *artifact.Artifact
	Metadata map[string]string
}

type PermissionGate interface {
	Decide(ctx context.Context, request permission.Request) (permission.Decision, error)
}
