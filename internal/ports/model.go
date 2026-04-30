package ports

import (
	"context"

	"github.com/Luew2/FreeCode/internal/core/model"
)

type ModelClient interface {
	Stream(ctx context.Context, request model.Request) (<-chan model.Event, error)
}

type ModelRegistry interface {
	ListModels(ctx context.Context) ([]model.Model, error)
	GetModel(ctx context.Context, ref model.Ref) (model.Model, error)
}

type ProtocolProbe interface {
	Probe(ctx context.Context, provider model.Provider, candidate model.ModelID) (ProbeResult, error)
}

type ProbeResult struct {
	Protocol model.ProtocolID
	Endpoint string
	Model    model.Model
	Metadata map[string]string
}
