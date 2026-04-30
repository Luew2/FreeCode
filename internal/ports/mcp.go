package ports

import (
	"context"

	"github.com/Luew2/FreeCode/internal/core/permission"
)

type MCPController interface {
	Status(mode permission.Mode) MCPStatus
	Reload(ctx context.Context) error
}

type MCPStatus struct {
	Enabled bool
	Servers []MCPServerStatus
	Tools   []MCPToolStatus
}

type MCPServerStatus struct {
	Name        string
	State       string
	Command     string
	ToolCount   int
	LastError   string
	Stderr      string
	ServerInfo  string
	Instruction string
}

type MCPToolStatus struct {
	PublicName   string
	ServerName   string
	OriginalName string
	Visible      bool
	HiddenReason string
	Capabilities []string
	Unclassified bool
}
