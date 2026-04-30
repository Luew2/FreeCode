package commands

import (
	"bytes"
	"context"
	"testing"

	"github.com/Luew2/FreeCode/internal/core/model"
)

func TestAskPrintsOrchestratorResponse(t *testing.T) {
	client := &singleResponseModelClient{
		events: []model.Event{
			{Type: model.EventTextDelta, Text: "hello"},
			{Type: model.EventCompleted},
		},
	}

	var out bytes.Buffer
	err := Ask(context.Background(), &out, AskDependencies{
		Model:  model.NewRef("local", "coder"),
		Client: client,
	}, AskOptions{Question: "say hi"})
	if err != nil {
		t.Fatalf("Ask returned error: %v", err)
	}
	if out.String() != "hello\n" {
		t.Fatalf("output = %q, want hello", out.String())
	}
}

type singleResponseModelClient struct {
	events []model.Event
}

func (c *singleResponseModelClient) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	ch := make(chan model.Event, len(c.events))
	for _, event := range c.events {
		ch <- event
	}
	close(ch)
	return ch, nil
}
