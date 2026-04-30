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
	SessionsDir     string
	EditorCommand   string
	EditorDoubleEsc bool
}

func DefaultSettings() Settings {
	return Settings{
		Version:       CurrentVersion,
		Providers:     map[model.ProviderID]model.Provider{},
		Models:        map[model.Ref]model.Model{},
		Agents:        agent.DefaultDefinitions(),
		Permissions:   permission.DefaultPolicy(),
		SessionsDir:   ".freecode/sessions",
		EditorCommand: "nvim",
	}
}
