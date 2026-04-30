package builtin

import (
	"context"
	"fmt"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/ports"
)

var _ ports.ToolRegistry = (*Registry)(nil)

type Registry struct {
	read       *ReadOnly
	applyPatch *ApplyPatch
	verify     *Verify
}

func NewWritable(fs ports.FileSystem, gate ports.PermissionGate) *Registry {
	return &Registry{
		read:       NewReadOnlyWithGate(fs, gate),
		applyPatch: NewApplyPatch(fs, gate),
	}
}

func NewVerifier(fs ports.FileSystem, gate ports.PermissionGate, root string) *Registry {
	return &Registry{
		read:   NewReadOnlyWithGate(fs, gate),
		verify: NewVerify(gate, root),
	}
}

func (r *Registry) Tools() []model.ToolSpec {
	if r == nil {
		return nil
	}

	var tools []model.ToolSpec
	if r.read != nil {
		tools = append(tools, r.read.Tools()...)
	}
	if r.applyPatch != nil {
		tools = append(tools, r.applyPatch.ToolSpec())
	}
	if r.verify != nil {
		tools = append(tools, r.verify.ToolSpec())
	}
	return tools
}

func (r *Registry) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	if r == nil {
		return ports.ToolResult{}, fmt.Errorf("tool registry is not configured")
	}
	if call.Name == "apply_patch" {
		if r.applyPatch == nil {
			return ports.ToolResult{}, fmt.Errorf("write tool %q is not enabled", call.Name)
		}
		return r.applyPatch.Run(ctx, call)
	}
	if call.Name == "run_check" {
		if r.verify == nil {
			return ports.ToolResult{}, fmt.Errorf("verify tool %q is not enabled", call.Name)
		}
		return r.verify.Run(ctx, call)
	}
	if r.read == nil {
		return ports.ToolResult{}, fmt.Errorf("read tools are not configured")
	}
	return r.read.RunTool(ctx, call)
}
