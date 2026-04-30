package config

import (
	"testing"

	"github.com/Luew2/FreeCode/internal/core/permission"
)

func TestDefaultSettings(t *testing.T) {
	settings := DefaultSettings()

	if settings.Version != CurrentVersion {
		t.Fatalf("Version = %d, want %d", settings.Version, CurrentVersion)
	}
	if settings.Providers == nil {
		t.Fatal("Providers map is nil")
	}
	if settings.Models == nil {
		t.Fatal("Models map is nil")
	}
	if len(settings.Agents) == 0 {
		t.Fatal("Agents is empty")
	}
	if settings.Permissions.DecisionFor(permission.ActionRead) != permission.DecisionAllow {
		t.Fatalf("read permission = %q, want %q", settings.Permissions.Read, permission.DecisionAllow)
	}
	if settings.Permissions.DecisionFor(permission.ActionNetwork) != permission.DecisionDeny {
		t.Fatalf("network permission = %q, want %q", settings.Permissions.Network, permission.DecisionDeny)
	}
	if settings.SessionsDir == "" {
		t.Fatal("SessionsDir is empty")
	}
	if settings.EditorCommand != "nvim" {
		t.Fatalf("EditorCommand = %q, want nvim", settings.EditorCommand)
	}
	if settings.EditorDoubleEsc {
		t.Fatal("EditorDoubleEsc = true, want false by default")
	}
}
