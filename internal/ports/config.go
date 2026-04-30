package ports

import (
	"context"

	"github.com/Luew2/FreeCode/internal/core/config"
)

type ConfigStore interface {
	Load(ctx context.Context) (config.Settings, error)
	Save(ctx context.Context, settings config.Settings) error
}

type SecretStore interface {
	Get(ctx context.Context, name string) (string, error)
	Set(ctx context.Context, name string, value string) error
	Delete(ctx context.Context, name string) error
}
