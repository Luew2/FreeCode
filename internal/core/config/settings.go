package config

import (
	"github.com/Luew2/FreeCode/internal/core/agent"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
)

const CurrentVersion = 1

type Settings struct {
	Version         int
	Providers       map[model.ProviderID]model.Provider
	Models          map[model.Ref]model.Model
	ActiveModel     model.Ref
	Agents          []agent.Definition
	Permissions     permission.Policy
	MCP             MCPSettings
	SessionsDir     string
	EditorCommand   string
	EditorDoubleEsc bool
}

type MCPSettings struct {
	Enabled bool
	Servers map[string]MCPServer
}

type MCPServer struct {
	Enabled          bool
	Transport        string
	Command          string
	Args             []string
	Env              []string
	WorkDir          string
	ToolsPrefix      string
	Capabilities     []string
	ToolCapabilities map[string][]string
	StartupTimeoutMS int
	CallTimeoutMS    int
	MaxOutputBytes   int
}

func DefaultSettings() Settings {
	return Settings{
		Version:       CurrentVersion,
		Providers:     map[model.ProviderID]model.Provider{},
		Models:        map[model.Ref]model.Model{},
		Agents:        agent.DefaultDefinitions(),
		Permissions:   permission.DefaultPolicy(),
		MCP:           MCPSettings{Servers: map[string]MCPServer{}},
		SessionsDir:   ".freecode/sessions",
		EditorCommand: "nvim",
	}
}
