package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Luew2/FreeCode/internal/app/contextmgr"
	"github.com/Luew2/FreeCode/internal/app/orchestrator"
	"github.com/Luew2/FreeCode/internal/app/prompt"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

type AskOptions struct {
	Question        string
	SessionID       string
	TurnContext     string
	MaxSteps        int
	MaxInputTokens  int
	MaxOutputTokens int
	// IncludeHistory replays prior user/assistant/tool messages from the
	// session log as real chat messages instead of folding them into a
	// textual SessionContext blob. This is what the interactive workbench
	// wants — so the agent sees its own prior tool_call_ids and can
	// reference them. Default is false to keep the headless `ask` CLI lean.
	IncludeHistory bool
}

type AskDependencies struct {
	Model                   model.Ref
	Client                  ports.ModelClient
	Tools                   ports.ToolRegistry
	Log                     ports.EventLog
	Prompt                  prompt.Builder
	Env                     prompt.Environment
	Session                 session.ID
	ContextBudget           contextmgr.Budget
	SessionContextMaxTokens int
}

func Ask(ctx context.Context, w io.Writer, deps AskDependencies, opts AskOptions) error {
	_, err := AskWithResponse(ctx, w, deps, opts)
	return err
}

func AskWithResponse(ctx context.Context, w io.Writer, deps AskDependencies, opts AskOptions) (orchestrator.Response, error) {
	question := strings.TrimSpace(opts.Question)
	if question == "" {
		return orchestrator.Response{}, errors.New("question is required")
	}
	if deps.Model == (model.Ref{}) {
		return orchestrator.Response{}, errors.New("active model is not configured")
	}
	if deps.Client == nil {
		return orchestrator.Response{}, errors.New("model client is not configured")
	}

	sessionID := deps.Session
	if opts.SessionID != "" {
		sessionID = session.ID(opts.SessionID)
	}
	if sessionID == "" {
		sessionID = "default"
	}

	budget := deps.ContextBudget
	if opts.MaxInputTokens > 0 {
		budget.MaxInputTokens = opts.MaxInputTokens
	}
	if opts.MaxOutputTokens > 0 {
		budget.MaxOutputTokens = opts.MaxOutputTokens
	}
	if err := contextmgr.ValidateBudget(budget); err != nil {
		return orchestrator.Response{}, err
	}
	sessionContextMax := deps.SessionContextMaxTokens
	if sessionContextMax <= 0 {
		sessionContextMax = 4096
	}

	var priorMessages []model.Message
	var sessionContext string
	if opts.IncludeHistory {
		messages, err := contextmgr.LoadMessageHistory(ctx, deps.Log, sessionID)
		if err != nil {
			return orchestrator.Response{}, err
		}
		// Cap the replayed history at half the input budget so we leave
		// headroom for tools, the system prompt, and the new user turn.
		historyBudget := sessionContextMax * 4
		if budget.MaxInputTokens > 0 {
			if half := budget.MaxInputTokens / 2; half > historyBudget {
				historyBudget = half
			}
		}
		priorMessages = contextmgr.HistoryWithBudget(messages, historyBudget)
	} else {
		ctxText, _, err := contextmgr.BuildSessionContext(ctx, deps.Log, sessionID, sessionContextMax)
		if err != nil {
			return orchestrator.Response{}, err
		}
		sessionContext = ctxText
	}

	response, err := orchestrator.Runner{
		Model:  deps.Client,
		Tools:  deps.Tools,
		Log:    deps.Log,
		Prompt: deps.Prompt,
	}.Run(ctx, orchestrator.Request{
		SessionID:      sessionID,
		Model:          deps.Model,
		UserRequest:    question,
		Environment:    deps.Env,
		MaxSteps:       opts.MaxSteps,
		SessionContext: sessionContext,
		TurnContext:    opts.TurnContext,
		ContextBudget:  budget,
		PriorMessages:  priorMessages,
	})
	if err != nil {
		return orchestrator.Response{}, err
	}

	if w != nil {
		_, err = fmt.Fprintln(w, response.Text)
		if err != nil {
			return response, err
		}
	}
	return response, nil
}
