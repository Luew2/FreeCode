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

type ContinueOptions struct {
	SessionID       string
	TurnContext     string
	MaxSteps        int
	MaxInputTokens  int
	MaxOutputTokens int
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

var ErrApprovalRequired = orchestrator.ErrApprovalRequired

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
		// Defensive: OpenAI requires every assistant tool_calls message
		// to be followed immediately by one tool result per tool_call_id.
		// Partial logs, cancellation, crashes, or history clipping can
		// leave either orphan tool messages or incomplete assistant tool
		// groups. Sanitize both directions before replay.
		priorMessages = sanitizeToolCallHistory(priorMessages)
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

func ContinueAfterApproval(ctx context.Context, w io.Writer, deps AskDependencies, opts ContinueOptions) (orchestrator.Response, error) {
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
	messages, err := contextmgr.LoadMessageHistory(ctx, deps.Log, sessionID)
	if err != nil {
		return orchestrator.Response{}, err
	}
	historyBudget := sessionContextMax * 4
	if budget.MaxInputTokens > 0 {
		if half := budget.MaxInputTokens / 2; half > historyBudget {
			historyBudget = half
		}
	}
	priorMessages := sanitizeToolCallHistory(contextmgr.HistoryWithBudget(messages, historyBudget))
	if len(priorMessages) == 0 {
		return orchestrator.Response{}, errors.New("no resumable conversation history")
	}

	response, err := orchestrator.Runner{
		Model:  deps.Client,
		Tools:  deps.Tools,
		Log:    deps.Log,
		Prompt: deps.Prompt,
	}.Continue(ctx, orchestrator.ContinueRequest{
		SessionID:     sessionID,
		Model:         deps.Model,
		Environment:   deps.Env,
		MaxSteps:      opts.MaxSteps,
		TurnContext:   opts.TurnContext,
		ContextBudget: budget,
		PriorMessages: priorMessages,
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

// sanitizeToolCallHistory enforces provider replay invariants:
//   - RoleTool messages must belong to the immediately preceding assistant
//     tool_calls group.
//   - Assistant tool_calls groups are kept only when every tool_call_id has
//     exactly one following tool result before the next non-tool message.
//
// OpenAI rejects either broken shape with HTTP 400. Dropping incomplete
// groups is safer than inventing synthetic tool results because the model
// would otherwise reason from tool outputs that never existed.
func sanitizeToolCallHistory(messages []model.Message) []model.Message {
	if len(messages) == 0 {
		return messages
	}
	out := make([]model.Message, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		switch m.Role {
		case model.RoleAssistant:
			if len(m.ToolCalls) == 0 {
				out = append(out, m)
				continue
			}
			expected := map[string]bool{}
			for _, call := range m.ToolCalls {
				if call.ID == "" {
					expected = nil
					break
				}
				expected[call.ID] = true
			}
			if len(expected) == 0 {
				continue
			}
			var tools []model.Message
			j := i + 1
			for ; j < len(messages) && messages[j].Role == model.RoleTool; j++ {
				if !expected[messages[j].ToolCallID] {
					continue
				}
				tools = append(tools, messages[j])
				delete(expected, messages[j].ToolCallID)
			}
			if len(expected) == 0 {
				out = append(out, m)
				out = append(out, tools...)
			}
			i = j - 1
		case model.RoleTool:
			continue
		default:
			out = append(out, m)
		}
	}
	return out
}
