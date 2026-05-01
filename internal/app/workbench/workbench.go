package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Luew2/FreeCode/internal/app/commands"
	"github.com/Luew2/FreeCode/internal/app/contextmgr"
	"github.com/Luew2/FreeCode/internal/core/artifact"
	"github.com/Luew2/FreeCode/internal/core/config"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

type Clipboard interface {
	Copy(ctx context.Context, text string) error
}

type Submitter func(ctx context.Context, request SubmitRequest) error

type ApprovalContinuation func(ctx context.Context, request ApprovalContinuationRequest) error

type ApprovalContinuationRequest struct {
	ApprovalID string
	Item       Item
	Permission permission.Request
	Target     ConversationTarget
}

type SubmitRequest struct {
	Text          string
	Approval      permission.Mode
	Swarm         bool
	Target        ConversationTarget
	TurnContext   string
	TerminalTools ports.ToolRegistry
}

type Service struct {
	Log              ports.EventLog
	Git              ports.Git
	Editor           ports.Editor
	Files            ports.FileSystem
	Clipboard        Clipboard
	Tools            ports.ToolRegistry
	Submit           Submitter
	ContinueApproval ApprovalContinuation
	Approval         *ApprovalGate
	Sessions         SessionIndex
	MCP              ports.MCPController
	// Config powers the :models modal — listing every configured provider
	// and switching the active model without dropping back to the CLI.
	// Optional; nil disables the swap UI but everything else still works.
	Config ports.ConfigStore
	Now    func() time.Time

	LogForSession      func(session.ID) ports.EventLog
	SessionID          session.ID
	WorkspaceRoot      string
	EditorCommand      string
	EditorDoubleEsc    bool
	ActiveConversation ConversationTarget
	Provider           string
	Model              model.Ref
}

// ModelEntry is one row in the :models modal: a provider+model pair the
// user can pick to activate.
type ModelEntry struct {
	Provider model.ProviderID
	Model    model.ModelID
	Ref      string
	BaseURL  string
	Active   bool
}

// ListModels returns every provider/model pair from config, with the
// active one flagged. Returns ([], nil) when no config store is wired so
// callers can render an empty/disabled modal cleanly.
func (s *Service) ListModels(ctx context.Context) ([]ModelEntry, error) {
	if s == nil || s.Config == nil {
		return nil, nil
	}
	settings, err := s.Config.Load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ModelEntry, 0, len(settings.Models))
	for ref, m := range settings.Models {
		provider := settings.Providers[ref.Provider]
		out = append(out, ModelEntry{
			Provider: ref.Provider,
			Model:    m.Ref.ID,
			Ref:      ref.String(),
			BaseURL:  provider.BaseURL,
			Active:   ref == settings.ActiveModel,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		return out[i].Ref < out[j].Ref
	})
	return out, nil
}

// SetActiveModel swaps the active model. Accepts either "provider" (the
// provider's default model is used) or "provider/model" for an explicit
// pick. Updates the in-memory Service state too so the next Load() picks
// up the new active model without a restart.
func (s *Service) SetActiveModel(ctx context.Context, ref string) (State, error) {
	if s == nil || s.Config == nil {
		return State{}, errors.New("config store is not configured")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return State{}, errors.New("model ref is required")
	}
	settings, err := s.Config.Load(ctx)
	if err != nil {
		return State{}, err
	}
	resolved, err := resolveModelRef(settings, ref)
	if err != nil {
		return State{}, err
	}
	settings.ActiveModel = resolved
	if err := s.Config.Save(ctx, settings); err != nil {
		return State{}, err
	}
	s.Model = resolved
	if provider, ok := settings.Providers[resolved.Provider]; ok {
		s.Provider = provider.Name
		if s.Provider == "" {
			s.Provider = string(provider.ID)
		}
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Notice = "active model: " + resolved.String()
	return state, nil
}

func resolveModelRef(settings config.Settings, ref string) (model.Ref, error) {
	if strings.Contains(ref, "/") {
		parsed, err := model.ParseRef(ref)
		if err != nil {
			return model.Ref{}, err
		}
		if _, ok := settings.Models[parsed]; !ok {
			return model.Ref{}, fmt.Errorf("model %q is not configured", parsed.String())
		}
		return parsed, nil
	}
	providerID := model.ProviderID(ref)
	provider, ok := settings.Providers[providerID]
	if !ok {
		return model.Ref{}, fmt.Errorf("provider %q is not configured", ref)
	}
	if provider.DefaultModel == "" {
		return model.Ref{}, fmt.Errorf("provider %q has no default_model; specify provider/model explicitly", ref)
	}
	return model.NewRef(providerID, provider.DefaultModel), nil
}

type State struct {
	SessionID          session.ID
	Provider           string
	Model              string
	Branch             string
	Approval           permission.Mode
	Mode               string
	Notice             string
	Detail             Item
	TokenEstimate      int
	WorkspaceRoot      string
	RightTab           RightTab
	ActiveConversation ConversationTarget
	Editor             EditorState
	Operations         OperationsState
	ContextPreview     ContextPreview

	Sessions             []SessionSummary
	Transcript           []TranscriptItem
	Agents               []AgentItem
	Files                []WorkspaceFile
	GitFiles             []WorkspaceFile
	Artifacts            []Item
	Approvals            []ApprovalItem
	Commands             []Command
	CompletionCandidates []CompletionCandidate
}

type SessionIndex interface {
	List(ctx context.Context, workspaceRoot string) ([]SessionSummary, error)
	Create(ctx context.Context, summary SessionSummary) error
	Rename(ctx context.Context, id session.ID, title string) error
}

type SessionSummary struct {
	ID            session.ID `json:"id"`
	Title         string     `json:"title"`
	WorkspaceRoot string     `json:"workspace_root"`
	Branch        string     `json:"branch"`
	Model         string     `json:"model"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LogPath       string     `json:"log_path,omitempty"`
}

type ConversationTarget struct {
	Kind  string
	ID    string
	Title string
}

type WorkspaceFile struct {
	ID         string
	Path       string
	Name       string
	Kind       string
	Status     string
	StatusLine string
}

type RightTab string

const (
	RightTabFiles     RightTab = "files"
	RightTabArtifacts RightTab = "artifacts"
	RightTabGit       RightTab = "git"
	RightTabTerm      RightTab = "term"
	RightTabOps       RightTab = "ops"
)

type EditorState struct {
	Command           string
	Available         bool
	Active            bool
	Focused           bool
	Path              string
	Line              int
	Error             string
	DoubleEscToReturn bool
}

type OperationsState struct {
	Items []OperationItem
}

type OperationItem struct {
	ID     string
	Kind   string
	Title  string
	Status string
	Body   string
	Target string
}

type ContextPreview struct {
	Summary            string
	Included           []string
	Excluded           []string
	TokenEstimate      int
	ActiveConversation string
}

type CompletionCandidate struct {
	Kind  string
	Value string
	Label string
}

type TranscriptKind string

const (
	TranscriptUser      TranscriptKind = "user"
	TranscriptAssistant TranscriptKind = "assistant"
	TranscriptStreaming TranscriptKind = "streaming"
	TranscriptThinking  TranscriptKind = "thinking"
	TranscriptTool      TranscriptKind = "tool"
	TranscriptPatch     TranscriptKind = "patch"
	TranscriptShell     TranscriptKind = "shell"
	TranscriptAgent     TranscriptKind = "agent"
	TranscriptContext   TranscriptKind = "context"
	TranscriptError     TranscriptKind = "error"
)

type TranscriptItem struct {
	ID         string
	Kind       TranscriptKind
	Actor      string
	Title      string
	Text       string
	Status     string
	ArtifactID string
	Streaming  bool
	Meta       map[string]string
}

type Item struct {
	ID       string
	Kind     string
	Title    string
	Body     string
	URI      string
	MIMEType string
	Meta     map[string]string
}

type AgentItem struct {
	ID            string
	Name          string
	Role          string
	Status        string
	TaskID        string
	Summary       string
	ChangedFiles  []string
	TestsRun      []string
	Findings      []string
	Questions     []string
	BlockedReason string
	CurrentStep   string
	Meta          map[string]string
	Text          string
}

type ApprovalItem struct {
	ID         string
	Title      string
	Body       string
	Kind       string
	Action     string
	Subject    string
	Reason     string
	ArtifactID string
}

type Command struct {
	ID             string
	Title          string
	Category       string
	Description    string
	Keybinding     string
	Aliases        []string
	Keybindings    []string
	Scopes         []string
	ArgKind        string
	Keywords       []string
	Enabled        bool
	DisabledReason string
	PaletteVisible bool
	HintVisible    bool

	// Key is kept for the plain fallback renderer and older tests.
	Key string
}

func (s *Service) Load(ctx context.Context) (State, error) {
	state := State{
		SessionID:          s.SessionID,
		Provider:           s.Provider,
		Model:              s.Model.String(),
		Approval:           s.approvalMode(),
		Mode:               "NORMAL",
		WorkspaceRoot:      s.WorkspaceRoot,
		RightTab:           RightTabFiles,
		ActiveConversation: s.activeConversation(),
		Editor:             s.editorState(),
		Commands:           DefaultCommands(),
	}

	if s.Sessions != nil {
		sessions, err := s.Sessions.List(ctx, s.WorkspaceRoot)
		if err == nil {
			state.Sessions = sessions
		}
	}

	if s.Git != nil {
		status, err := s.Git.Status(ctx)
		if err == nil {
			state.Branch = status.Branch
			for i, path := range status.ChangedFiles {
				statusLine := path
				path = cleanGitStatusPath(path)
				state.Artifacts = append(state.Artifacts, Item{
					ID:    fmt.Sprintf("f%d", i+1),
					Kind:  "file",
					Title: statusLine,
					URI:   "file:" + path,
					Meta:  map[string]string{"path": path},
				})
				state.GitFiles = append(state.GitFiles, WorkspaceFile{
					ID:         fmt.Sprintf("g%d", i+1),
					Path:       path,
					Name:       path,
					Kind:       "git",
					Status:     gitStatusCode(statusLine),
					StatusLine: statusLine,
				})
			}
		}
	}
	if s.Files != nil {
		files, err := s.Files.ListFiles(ctx, "")
		if err == nil {
			for i, path := range files {
				state.Files = append(state.Files, WorkspaceFile{
					ID:   fmt.Sprintf("w%d", i+1),
					Path: path,
					Name: path,
					Kind: "file",
				})
			}
		}
	}

	approvalDecisions := map[string]string{}
	if s.Log != nil {
		events, err := s.Log.Stream(ctx, s.SessionID)
		if err != nil {
			return State{}, err
		}
		messageCount := 0
		codeCount := countKind(state.Artifacts, "code")
		patchCount := countKind(state.Artifacts, "patch")
		approvalCount := 0
		var streaming strings.Builder
		modelStarted := false
		streamMeta := map[string]string{}
		var fullTranscript []TranscriptItem
		agentIndexes := map[string]int{}
		// agentTranscriptIndexes maps an agent_id to the position of its
		// transcript entry. The orchestrator emits an EventAgent twice per
		// subagent (running, completed) so we'd otherwise render the same
		// task description as two near-identical messages. With this map we
		// update the existing transcript entry's status/text in place.
		agentTranscriptIndexes := map[string]int{}
		appendTranscript := func(kind TranscriptKind, actor string, title string, text string, opts map[string]string) {
			text = strings.TrimSpace(text)
			if text == "" && kind != TranscriptThinking && kind != TranscriptTool && kind != TranscriptPatch && kind != TranscriptShell && kind != TranscriptAgent && kind != TranscriptContext && kind != TranscriptError {
				return
			}
			if opts["local_only"] != "true" && opts["share_with_model"] != "false" {
				state.TokenEstimate += estimateTokens(text)
			}
			messageCount++
			item := TranscriptItem{
				ID:     fmt.Sprintf("m%d", messageCount),
				Kind:   kind,
				Actor:  actor,
				Title:  title,
				Text:   text,
				Status: opts["status"],
				Meta:   map[string]string{},
			}
			if opts["artifact_id"] != "" {
				item.ArtifactID = opts["artifact_id"]
			}
			if opts["streaming"] == "true" {
				item.Streaming = true
			}
			for key, value := range opts {
				if value != "" && key != "status" && key != "artifact_id" && key != "streaming" {
					item.Meta[key] = value
				}
			}
			if len(item.Meta) == 0 {
				item.Meta = nil
			}
			fullTranscript = append(fullTranscript, item)
		}
		flushModelDraft := func() {
			text := strings.TrimSpace(streaming.String())
			if text == "" {
				return
			}
			opts := cloneStringMap(streamMeta)
			opts["status"] = "draft"
			appendTranscript(TranscriptAssistant, "assistant", "assistant", text, opts)
			streaming.Reset()
		}
		for event := range events {
			switch event.Type {
			case session.EventUserMessage, session.EventAssistantMessage:
				text := strings.TrimSpace(event.Text)
				// Tool-call-only assistant turns log empty text with the
				// tool_calls in the payload; we don't need to render those
				// as a separate item because the EventTool entries that
				// follow surface them. But for a true "model returned
				// nothing" case (no text, no tool_calls) we want to render
				// a placeholder so the user is not left looking at a stale
				// "waiting for model output" indicator with no resolution.
				if text == "" {
					if event.Type == session.EventAssistantMessage && !assistantPayloadHasToolCalls(event.Payload) {
						streaming.Reset()
						modelStarted = false
						placeholder := "(no response)"
						if hint := assistantDiagnosticHint(event.Payload); hint != "" {
							placeholder = "(no response — " + hint + ")"
						}
						appendTranscript(TranscriptAssistant, event.Actor, "assistant", placeholder, transcriptEventMeta(event))
					}
					continue
				}
				streaming.Reset()
				modelStarted = false
				kind := TranscriptUser
				title := "you"
				if event.Type == session.EventAssistantMessage {
					kind = TranscriptAssistant
					title = "assistant"
					// Reasoning turns get their own kind so the chat
					// renderer can show them muted / collapsed and they
					// never get folded into the user-facing assistant
					// answer text.
					if status := stringValue(event.Payload["status"]); status == "reasoning" {
						kind = TranscriptThinking
						title = "thinking"
					}
				}
				appendTranscript(kind, event.Actor, title, text, transcriptEventMeta(event))
				for _, block := range extractCodeBlocks(text) {
					codeCount++
					state.Artifacts = append(state.Artifacts, Item{
						ID:       fmt.Sprintf("c%d", codeCount),
						Kind:     "code",
						Title:    codeTitle(block.Language, codeCount),
						Body:     block.Body,
						MIMEType: codeMIME(block.Language),
						Meta:     map[string]string{"language": block.Language, "fenced": block.Fenced},
					})
				}
			case session.EventModel:
				switch stringValue(event.Payload["event_type"]) {
				case string(model.EventStarted):
					flushModelDraft()
					streaming.Reset()
					modelStarted = true
					streamMeta = transcriptEventMeta(event)
				case string(model.EventTextDelta):
					if event.Text != "" {
						// Reasoning deltas are shown as a separate
						// "thinking" transcript item once the
						// orchestrator logs them at end-of-turn — do not
						// fold them into the streaming text buffer or the
						// chat will show reasoning concatenated with the
						// final user-facing answer (the doubled-text
						// effect the user reported).
						if isReasoningPayload(event.Payload) {
							break
						}
						streaming.WriteString(event.Text)
					}
				case string(model.EventToolCall):
					flushModelDraft()
					modelStarted = false
					if call := mapStringAny(event.Payload["tool_call"]); len(call) > 0 {
						opts := transcriptEventMeta(event)
						opts["status"] = "requested"
						opts["tool_call_id"] = stringValue(call["id"])
						opts["arguments"] = stringValue(call["arguments"])
						appendTranscript(TranscriptTool, "model", stringValue(call["name"]), stringValue(call["arguments"]), opts)
					}
				case string(model.EventCompleted):
					modelStarted = false
				case string(model.EventError):
					streaming.Reset()
					modelStarted = false
					appendTranscript(TranscriptError, "model", "error", firstNonEmpty(stringValue(event.Payload["error"]), event.Text), transcriptEventMeta(event))
				}
			case session.EventTool, session.EventArtifact:
				if item, ok := artifactFromPayload(event.Payload); ok {
					if item.Kind == "patch" && item.ID == "" {
						patchCount++
						item.ID = fmt.Sprintf("p%d", patchCount)
					}
					state.Artifacts = append(state.Artifacts, item)
					kind := TranscriptTool
					if item.Kind == "patch" {
						kind = TranscriptPatch
					}
					opts := transcriptEventMeta(event)
					opts["artifact_id"] = item.ID
					opts["status"] = item.Meta["state"]
					opts["kind"] = item.Kind
					actor := event.Actor
					title := firstNonEmpty(item.Title, item.Meta["tool_call_name"], item.Kind)
					text := firstNonEmpty(event.Text, item.Body)
					if item.Kind == "shell" {
						kind = TranscriptShell
						actor = "local"
						text = item.Body
						opts["command"] = item.Meta["command"]
						opts["exit"] = item.Meta["exit"]
						opts["local_only"] = firstNonEmpty(item.Meta["local_only"], "true")
						opts["share_with_model"] = firstNonEmpty(item.Meta["share_with_model"], "false")
						if opts["status"] == "" && item.Meta["exit"] != "" && item.Meta["exit"] != "0" {
							opts["status"] = item.Meta["exit"]
						}
					}
					if item.Kind == "terminal" {
						kind = TranscriptContext
						actor = "local"
						title = firstNonEmpty(item.Title, "attached terminal output")
						text = item.Body
						opts["share_with_model"] = firstNonEmpty(item.Meta["share_with_model"], "true")
					}
					appendTranscript(kind, actor, title, text, opts)
				} else if item, ok := approvalFromToolEvent(approvalCount+1, event); ok {
					approvalCount++
					state.Artifacts = append(state.Artifacts, item)
					opts := transcriptEventMeta(event)
					opts["artifact_id"] = item.ID
					opts["status"] = item.Meta["state"]
					opts["kind"] = item.Kind
					appendTranscript(TranscriptTool, event.Actor, item.Title, item.Body, opts)
				} else if strings.TrimSpace(event.Text) != "" || stringValue(event.Payload["name"]) != "" {
					opts := transcriptEventMeta(event)
					opts["status"] = stringValue(event.Payload["error"])
					appendTranscript(TranscriptTool, event.Actor, stringValue(event.Payload["name"]), event.Text, opts)
				}
			case session.EventAgent:
				agentItem := agentFromEvent(len(state.Agents)+1, event)
				if isSwarmLifecycleAgent(agentItem) {
					opts := transcriptEventMeta(event)
					opts["status"] = agentItem.Status
					opts["role"] = agentItem.Role
					appendTranscript(TranscriptContext, event.Actor, agentTranscriptTitle(agentItem), firstNonEmpty(agentItem.Summary, event.Text), opts)
					continue
				}
				if key := agentIdentity(agentItem); key != "" {
					if index, ok := agentIndexes[key]; ok {
						agentItem = mergeAgentItem(state.Agents[index], agentItem)
						state.Agents[index] = agentItem
					} else {
						agentIndexes[key] = len(state.Agents)
						state.Agents = append(state.Agents, agentItem)
					}
				} else {
					state.Agents = append(state.Agents, agentItem)
				}
				opts := transcriptEventMeta(event)
				opts["agent_id"] = agentItem.ID
				opts["status"] = agentItem.Status
				opts["role"] = agentItem.Role
				agentText := firstNonEmpty(agentItem.Summary, event.Text)
				if existingIdx, ok := agentTranscriptIndexes[agentItem.ID]; ok && existingIdx < len(fullTranscript) {
					prev := fullTranscript[existingIdx]
					prev.Status = agentItem.Status
					if strings.TrimSpace(agentText) != "" {
						prev.Text = strings.TrimSpace(agentText)
					}
					prev.Title = agentTranscriptTitle(agentItem)
					if prev.Meta == nil {
						prev.Meta = map[string]string{}
					}
					for key, value := range opts {
						if value != "" && key != "status" {
							prev.Meta[key] = value
						}
					}
					fullTranscript[existingIdx] = prev
				} else {
					appendTranscript(TranscriptAgent, event.Actor, agentTranscriptTitle(agentItem), agentText, opts)
					agentTranscriptIndexes[agentItem.ID] = len(fullTranscript) - 1
				}
			case session.EventContextCompacted:
				appendTranscript(TranscriptContext, event.Actor, "context", event.Text, transcriptEventMeta(event))
			case session.EventError:
				appendTranscript(TranscriptError, event.Actor, "error", event.Text, transcriptEventMeta(event))
			case session.EventPermissionRequested:
				artifactID := stringValue(event.Payload["artifact"])
				decision := stringValue(event.Payload["approval"])
				if artifactID != "" && decision != "" {
					approvalDecisions[artifactID] = decision
				}
			}
		}
		if text := strings.TrimSpace(streaming.String()); text != "" {
			opts := cloneStringMap(streamMeta)
			opts["streaming"] = "true"
			appendTranscript(TranscriptStreaming, "assistant streaming", "assistant", text, opts)
		} else if modelStarted {
			appendTranscript(TranscriptThinking, "assistant", "thinking", "waiting for model output", streamMeta)
		}
		state.Transcript = compactTranscriptToolRequests(conversationTranscript(fullTranscript, state.Agents, state.ActiveConversation))
	}

	sort.SliceStable(state.Artifacts, func(i, j int) bool {
		return artifactSortKey(state.Artifacts[i].ID) < artifactSortKey(state.Artifacts[j].ID)
	})
	state.Approvals = pendingApprovals(state.Artifacts, approvalDecisions)
	state.ContextPreview = contextPreviewFromState(state)
	state.Operations = operationsFromState(state)
	state.CompletionCandidates = completionCandidates(state)
	return state, nil
}

func operationsFromState(state State) OperationsState {
	var items []OperationItem
	items = append(items, OperationItem{
		ID:     "ctx",
		Kind:   "context",
		Title:  "next context",
		Status: fmt.Sprintf("~%d tokens", state.TokenEstimate),
		Body:   state.ContextPreview.Summary,
		Target: "context",
	})
	if state.ActiveConversation.ID != "" || state.ActiveConversation.Title != "" {
		items = append(items, OperationItem{
			ID:     "chat",
			Kind:   "conversation",
			Title:  "active buffer",
			Status: conversationLabel(state.ActiveConversation),
			Target: state.ActiveConversation.ID,
		})
	}
	if len(state.Approvals) > 0 {
		items = append(items, OperationItem{
			ID:     "approvals",
			Kind:   "approval",
			Title:  "pending approvals",
			Status: strconv.Itoa(len(state.Approvals)),
			Body:   approvalIDs(state.Approvals),
			Target: "approvals",
		})
	}
	running, blocked, completed := agentStatusCounts(state.Agents)
	if len(state.Agents) > 0 {
		items = append(items, OperationItem{
			ID:     "agents",
			Kind:   "agents",
			Title:  "agents",
			Status: fmt.Sprintf("%d running  %d blocked  %d done", running, blocked, completed),
			Target: "agents",
		})
	}
	if len(state.GitFiles) > 0 {
		items = append(items, OperationItem{
			ID:     "git",
			Kind:   "git",
			Title:  "git changes",
			Status: strconv.Itoa(len(state.GitFiles)),
			Target: "git",
		})
	}
	if recent := recentOperationalItem(state.Transcript); recent.ID != "" {
		items = append(items, recent)
	}
	return OperationsState{Items: items}
}

func contextPreviewFromState(state State) ContextPreview {
	target := conversationLabel(state.ActiveConversation)
	if target == "" {
		target = "Main orchestrator"
	}
	included := []string{"new prompt", "visible " + strings.ToLower(target) + " conversation history"}
	excluded := []string{"local-only :! shell output unless attached with :sto", "unshared terminal state"}
	if len(state.Transcript) == 0 {
		included = []string{"new prompt"}
	}
	if hasSharedTerminalContext(state.Transcript) {
		included = append(included, "attached terminal output")
	}
	if len(state.Approvals) > 0 {
		included = append(included, fmt.Sprintf("%d pending approval(s)", len(state.Approvals)))
	}
	summary := fmt.Sprintf("%s, %d transcript cells, ~%d tokens", target, len(state.Transcript), state.TokenEstimate)
	return ContextPreview{
		Summary:            summary,
		Included:           included,
		Excluded:           excluded,
		TokenEstimate:      state.TokenEstimate,
		ActiveConversation: target,
	}
}

func hasSharedTerminalContext(items []TranscriptItem) bool {
	for _, item := range items {
		if item.Kind == TranscriptContext && firstNonEmpty(item.Meta["share_with_model"], "false") == "true" {
			return true
		}
	}
	return false
}

func conversationLabel(target ConversationTarget) string {
	target = normalizeConversationTarget(target)
	if target.Kind == "agent" {
		return firstNonEmpty(target.Title, target.ID, "agent")
	}
	return firstNonEmpty(target.Title, "Main orchestrator")
}

func approvalIDs(approvals []ApprovalItem) string {
	var ids []string
	for _, approval := range approvals {
		if approval.ID != "" {
			ids = append(ids, approval.ID)
		}
	}
	return strings.Join(ids, " ")
}

func agentStatusCounts(agents []AgentItem) (running int, blocked int, completed int) {
	for _, agent := range agents {
		status := strings.ToLower(strings.TrimSpace(agent.Status))
		switch {
		case strings.Contains(status, "block") || strings.TrimSpace(agent.BlockedReason) != "":
			blocked++
		case strings.Contains(status, "complete") || strings.Contains(status, "done"):
			completed++
		default:
			running++
		}
	}
	return running, blocked, completed
}

func recentOperationalItem(items []TranscriptItem) OperationItem {
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		switch item.Kind {
		case TranscriptTool, TranscriptPatch, TranscriptShell, TranscriptError:
			title := firstNonEmpty(item.Title, item.Actor, string(item.Kind))
			status := firstNonEmpty(item.Status, string(item.Kind))
			return OperationItem{
				ID:     "recent",
				Kind:   string(item.Kind),
				Title:  "recent " + string(item.Kind) + ": " + title,
				Status: status,
				Body:   item.Text,
				Target: item.ID,
			}
		}
	}
	return OperationItem{}
}

func (s *Service) SubmitPrompt(ctx context.Context, request SubmitRequest) (State, error) {
	if strings.TrimSpace(request.Text) == "" {
		return State{}, errors.New("prompt is required")
	}
	if s.Submit == nil {
		return State{}, errors.New("submit is not configured")
	}
	if request.Approval == "" {
		request.Approval = s.approvalMode()
	}
	if request.Target.Kind == "" {
		request.Target = s.activeConversation()
	}
	s.ActiveConversation = normalizeConversationTarget(request.Target)
	request.TurnContext = combineTurnContexts(s.submitTurnContext(ctx, s.ActiveConversation), request.TurnContext)
	if err := s.Submit(ctx, request); err != nil {
		if errors.Is(err, commands.ErrApprovalRequired) {
			state, loadErr := s.Load(ctx)
			if loadErr != nil {
				return State{}, loadErr
			}
			state.Notice = "approval required"
			return state, nil
		}
		return State{}, err
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	if request.Swarm {
		state.Notice = "swarm task sent to main orchestrator"
	} else {
		state.Notice = "prompt sent"
	}
	return state, nil
}

func (s *Service) submitTurnContext(ctx context.Context, target ConversationTarget) string {
	var sections []string
	if state, err := s.Load(ctx); err == nil && len(state.Transcript) > 0 {
		if history := transcriptTurnContext(state.Transcript, target, 7000); history != "" {
			sections = append(sections, history)
		}
	}
	if terminal := s.sharedTerminalTurnContext(ctx, 6000); terminal != "" {
		sections = append(sections, "Terminal output explicitly attached by the user with :sto / :share-output:\n"+terminal)
	}
	return trimContext(strings.Join(sections, "\n\n"), 12000)
}

func transcriptTurnContext(items []TranscriptItem, target ConversationTarget, maxChars int) string {
	var lines []string
	title := "Visible conversation history"
	target = normalizeConversationTarget(target)
	if target.Kind == "agent" {
		title = "Selected agent conversation history"
		if target.Title != "" {
			title += " for " + target.Title
		}
	}
	for _, item := range items {
		if item.Kind == TranscriptContext && firstNonEmpty(item.Meta["share_with_model"], "false") == "true" {
			continue
		}
		text := strings.TrimSpace(item.Text)
		if text == "" {
			continue
		}
		label := firstNonEmpty(item.Title, item.Actor, string(item.Kind))
		if item.Status != "" {
			label += " " + item.Status
		}
		lines = append(lines, label+":")
		for _, line := range strings.Split(text, "\n") {
			lines = append(lines, "  "+strings.TrimRight(line, "\r"))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	body := tailByChars(strings.Join(lines, "\n"), maxChars)
	return title + ":\n" + body
}

func (s *Service) sharedTerminalTurnContext(ctx context.Context, maxChars int) string {
	if s.Log == nil {
		return ""
	}
	stream, err := s.Log.Stream(ctx, s.SessionID)
	if err != nil {
		return ""
	}
	var blocks []string
	for event := range stream {
		if event.Type == session.EventUserMessage || event.Type == session.EventAssistantMessage {
			blocks = nil
		}
		item, ok := artifactFromPayload(event.Payload)
		if !ok || item.Kind != "terminal" || strings.TrimSpace(item.Body) == "" {
			continue
		}
		if firstNonEmpty(item.Meta["share_with_model"], "true") == "false" {
			continue
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = "Shared terminal output"
		}
		blocks = append(blocks, title+":\n"+strings.TrimSpace(item.Body))
	}
	if len(blocks) == 0 {
		return ""
	}
	if len(blocks) > 2 {
		blocks = blocks[len(blocks)-2:]
	}
	return tailByChars(strings.Join(blocks, "\n\n"), maxChars)
}

func trimContext(text string, maxChars int) string {
	text = strings.TrimSpace(text)
	if maxChars <= 0 || len(text) <= maxChars {
		return text
	}
	return "[truncated]\n" + text[len(text)-maxChars:]
}

func tailByChars(text string, maxChars int) string {
	text = strings.TrimSpace(text)
	if maxChars <= 0 || len(text) <= maxChars {
		return text
	}
	return "[earlier context truncated]\n" + text[len(text)-maxChars:]
}

func combineTurnContexts(base string, extra string) string {
	base = strings.TrimSpace(base)
	extra = strings.TrimSpace(extra)
	switch {
	case base == "":
		return extra
	case extra == "":
		return base
	default:
		return base + "\n\n" + extra
	}
}

func (s *Service) SetActiveConversation(ctx context.Context, target ConversationTarget) (State, error) {
	target = normalizeConversationTarget(target)
	if target.Kind == "agent" && strings.TrimSpace(target.ID) == "" {
		return State{}, errors.New("agent conversation requires an id")
	}
	s.ActiveConversation = target
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.ActiveConversation = target
	state.Notice = "conversation: " + target.Title
	return state, nil
}

func (s *Service) Copy(ctx context.Context, id string, withFences bool) (State, error) {
	state, item, err := s.item(ctx, id)
	if err != nil {
		return State{}, err
	}
	text := item.Body
	if withFences && item.Kind == "code" {
		text = item.Meta["fenced"]
	}
	if text == "" {
		text = item.Title
	}
	if s.Clipboard == nil {
		return State{}, errors.New("clipboard is not configured")
	}
	if err := s.Clipboard.Copy(ctx, text); err != nil {
		return State{}, err
	}
	state.Notice = "copied " + id
	return state, nil
}

func (s *Service) Open(ctx context.Context, ref string) (State, error) {
	id, line := SplitArtifactLine(ref)
	state, item, err := s.item(ctx, id)
	if err != nil {
		return State{}, err
	}
	path := strings.TrimPrefix(item.URI, "file:")
	if path == item.URI {
		path = item.Meta["path"]
	}
	if path == "" {
		state.Detail = item
		state.Notice = "opened " + id
		return state, nil
	}
	return s.openPath(ctx, path, line, "opened "+id)
}

func (s *Service) OpenPath(ctx context.Context, path string, line int) (State, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return State{}, errors.New("path is required")
	}
	return s.openPath(ctx, path, line, "opened "+path)
}

func (s *Service) RunShell(ctx context.Context, command string) (State, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return State{}, errors.New("shell command is required")
	}
	if s.approvalMode() == permission.ModeReadOnly {
		return State{}, errors.New("shell commands are disabled in read-only approval mode")
	}
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.CommandContext(runCtx, shell, "-lc", command)
	if strings.TrimSpace(s.WorkspaceRoot) != "" {
		cmd.Dir = s.WorkspaceRoot
	}
	output, runErr := cmd.CombinedOutput()
	body := strings.TrimSpace(string(output))
	if body == "" {
		body = "command completed with no output"
	}
	if len(body) > 64*1024 {
		body = body[:64*1024] + "\n[truncated]"
	}
	exit := "0"
	title := "! " + command
	if runCtx.Err() == context.DeadlineExceeded {
		exit = "timeout"
		body = "command timed out: " + command + "\n" + body
	} else if runErr != nil {
		exit = "failed"
		body = "command failed: " + command + "\n" + body
	}
	item := Item{
		ID:    fmt.Sprintf("sh%d", s.now().UnixNano()),
		Kind:  "shell",
		Title: title,
		Body:  body,
		Meta: map[string]string{
			"command":          command,
			"exit":             exit,
			"local_only":       "true",
			"share_with_model": "false",
		},
	}
	_ = s.logArtifact(ctx, item)
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Detail = item
	if exit == "0" {
		state.Notice = "shell completed"
	} else {
		state.Notice = "shell " + exit
	}
	return state, nil
}

func (s *Service) ShareTerminal(ctx context.Context, title string, body string) (State, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return State{}, errors.New("terminal output is empty")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Attached terminal output"
	}
	if len(body) > 64*1024 {
		body = body[:64*1024] + "\n[truncated]"
	}
	item := Item{
		ID:    fmt.Sprintf("term%d", s.now().UnixNano()),
		Kind:  "terminal",
		Title: title,
		Body:  body,
		Meta: map[string]string{
			"source":           "terminal",
			"share_with_model": "true",
		},
	}
	if s.Log != nil {
		metadata := map[string]any{}
		for key, value := range item.Meta {
			metadata[key] = value
		}
		if err := s.Log.Append(ctx, session.Event{
			ID:        s.eventID("terminal"),
			SessionID: s.SessionID,
			Type:      session.EventArtifact,
			At:        s.now(),
			Actor:     "user",
			Text:      item.Body,
			Payload: map[string]any{
				"artifact": map[string]any{
					"id":        item.ID,
					"kind":      item.Kind,
					"title":     item.Title,
					"body":      item.Body,
					"metadata":  metadata,
					"mime_type": "text/plain",
				},
			},
		}); err != nil {
			return State{}, err
		}
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Detail = item
	state.RightTab = RightTabTerm
	state.Notice = "terminal output attached to next prompt"
	return state, nil
}

func (s *Service) openPath(ctx context.Context, path string, line int, notice string) (State, error) {
	if err := s.ensureEditorAvailable(); err != nil {
		state, loadErr := s.Load(ctx)
		if loadErr != nil {
			return State{}, err
		}
		state.Editor.Error = err.Error()
		return state, err
	}
	if s.Editor == nil {
		return State{}, errors.New("editor is not configured")
	}
	before := gitChangedSet(ctx, s.Git)
	beforeFingerprint := fileFingerprint(s.WorkspaceRoot, path)
	if err := s.Editor.Open(ctx, path, line); err != nil {
		return State{}, err
	}
	after := gitChangedSet(ctx, s.Git)
	afterFingerprint := fileFingerprint(s.WorkspaceRoot, path)
	edited := changedAfterEdit(before, after)
	if len(edited) == 0 && beforeFingerprint != "" && afterFingerprint != "" && beforeFingerprint != afterFingerprint {
		edited = []string{path}
	}
	if len(edited) > 0 {
		_ = s.logUserEdit(ctx, edited)
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	if len(edited) > 0 {
		state.RightTab = RightTabGit
	}
	state.Editor.Active = false
	state.Editor.Focused = false
	state.Editor.Path = path
	state.Editor.Line = line
	state.Notice = notice
	return state, nil
}

func (s *Service) logUserEdit(ctx context.Context, paths []string) error {
	if s.Log == nil || len(paths) == 0 {
		return nil
	}
	title := "User edited " + strings.Join(paths, ", ")
	return s.Log.Append(ctx, session.Event{
		ID:        s.eventID("edit"),
		SessionID: s.SessionID,
		Type:      session.EventArtifact,
		At:        s.now(),
		Actor:     "user",
		Text:      title,
		Payload: map[string]any{
			"artifact": map[string]any{
				"id":    fmt.Sprintf("u%d", s.now().UnixNano()),
				"kind":  "user_edit",
				"title": title,
				"body":  strings.Join(paths, "\n"),
				"metadata": map[string]any{
					"changed_files": strings.Join(paths, ", "),
				},
			},
		},
	})
}

func (s *Service) logArtifact(ctx context.Context, item Item) error {
	if s.Log == nil {
		return nil
	}
	metadata := map[string]any{}
	for key, value := range item.Meta {
		metadata[key] = value
	}
	return s.Log.Append(ctx, session.Event{
		ID:        s.eventID(item.Kind),
		SessionID: s.SessionID,
		Type:      session.EventArtifact,
		At:        s.now(),
		Actor:     "user",
		Text:      item.Title,
		Payload: map[string]any{
			"artifact": map[string]any{
				"id":        item.ID,
				"kind":      item.Kind,
				"title":     item.Title,
				"body":      item.Body,
				"uri":       item.URI,
				"mime_type": item.MIMEType,
				"metadata":  metadata,
			},
		},
	})
}

func (s *Service) Detail(ctx context.Context, id string) (State, error) {
	state, item, err := s.item(ctx, id)
	if err != nil {
		return State{}, err
	}
	state.Detail = item
	return state, nil
}

func (s *Service) NewSession(ctx context.Context, title string) (State, error) {
	id := session.ID(NewSessionID(s.now()))
	title = strings.TrimSpace(title)
	if title == "" {
		title = defaultSessionTitle(s.WorkspaceRoot, id)
	}
	now := s.now()
	summary := SessionSummary{
		ID:            id,
		Title:         title,
		WorkspaceRoot: s.WorkspaceRoot,
		Branch:        currentBranch(ctx, s.Git),
		Model:         s.Model.String(),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if s.Sessions != nil {
		if err := s.Sessions.Create(ctx, summary); err != nil {
			return State{}, err
		}
	}
	s.SessionID = id
	if s.LogForSession != nil {
		s.Log = s.LogForSession(id)
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Notice = "new session " + string(id)
	return state, nil
}

func (s *Service) ResumeSession(ctx context.Context, id session.ID) (State, error) {
	if strings.TrimSpace(string(id)) == "" {
		return State{}, errors.New("session id is required")
	}
	if s.Sessions != nil {
		sessions, err := s.Sessions.List(ctx, s.WorkspaceRoot)
		if err != nil {
			return State{}, err
		}
		found := false
		for _, summary := range sessions {
			if summary.ID == id {
				found = true
				break
			}
		}
		if !found {
			return State{}, fmt.Errorf("unknown session %q", id)
		}
	}
	s.SessionID = id
	if s.LogForSession != nil {
		s.Log = s.LogForSession(id)
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Notice = "resumed session " + string(id)
	return state, nil
}

func currentSessionSummary(ctx context.Context, index SessionIndex, workspaceRoot string, id session.ID) SessionSummary {
	if index == nil || id == "" {
		return SessionSummary{}
	}
	sessions, err := index.List(ctx, workspaceRoot)
	if err != nil {
		return SessionSummary{}
	}
	for _, summary := range sessions {
		if summary.ID == id {
			return summary
		}
	}
	return SessionSummary{}
}

func (s *Service) RenameSession(ctx context.Context, title string) (State, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return State{}, errors.New("session title is required")
	}
	if s.Sessions == nil {
		return State{}, errors.New("session index is not configured")
	}
	if err := s.Sessions.Rename(ctx, s.SessionID, title); err != nil {
		return State{}, err
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Notice = "renamed session " + string(s.SessionID)
	return state, nil
}

func (s *Service) Approve(ctx context.Context, id string) (State, error) {
	state, item, err := s.item(ctx, id)
	if err != nil {
		return State{}, err
	}
	request, ok := permissionRequestForItem(item)
	if !ok {
		return State{}, fmt.Errorf("%s is not approvable", id)
	}
	if s.Approval != nil {
		s.Approval.Approve(request)
	}
	if item.Kind == "patch" && item.Meta["state"] == "preview" {
		if _, err := s.applyApprovedPatch(ctx, id, item); err != nil {
			return State{}, err
		}
		if err := s.logPermission(ctx, "approved", id, request); err != nil {
			return State{}, err
		}
		applied, err := s.Load(ctx)
		if err != nil {
			return State{}, err
		}
		applied.Notice = "approved and applied " + id
		return applied, nil
	}
	if err := s.logPermission(ctx, "approved", id, request); err != nil {
		return State{}, err
	}
	if s.ContinueApproval != nil && resumableApprovalItem(item) {
		err := s.ContinueApproval(ctx, ApprovalContinuationRequest{
			ApprovalID: id,
			Item:       item,
			Permission: request,
			Target:     s.activeConversation(),
		})
		if err != nil {
			if errors.Is(err, commands.ErrApprovalRequired) {
				state, loadErr := s.Load(ctx)
				if loadErr != nil {
					return State{}, loadErr
				}
				state.Notice = "approval required"
				return state, nil
			}
			return State{}, err
		}
		state, err = s.Load(ctx)
		if err != nil {
			return State{}, err
		}
		state.Notice = "approved " + id + " and resumed"
		return state, nil
	}
	state, err = s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Notice = "approved " + id
	return state, nil
}

func resumableApprovalItem(item Item) bool {
	return item.Kind == "approval" &&
		strings.TrimSpace(item.Meta["tool_call_id"]) != "" &&
		strings.TrimSpace(item.Meta["tool_call_name"]) != ""
}

func (s *Service) ApprovalContinuationToolCalls(ctx context.Context, item Item) ([]model.ToolCall, error) {
	targetID := strings.TrimSpace(item.Meta["tool_call_id"])
	if targetID == "" {
		return nil, errors.New("approval is missing tool_call_id")
	}
	if s.Log == nil {
		return fallbackApprovalToolCall(item)
	}
	stream, err := s.Log.Stream(ctx, s.SessionID)
	if err != nil {
		return nil, err
	}
	var latestGroup []model.ToolCall
	for event := range stream {
		if event.Type != session.EventAssistantMessage || stringValue(event.Payload["status"]) != "tool_calls" {
			continue
		}
		calls := toolCallsFromPayload(event.Payload)
		if toolCallIndex(calls, targetID) >= 0 {
			latestGroup = calls
		}
	}
	if len(latestGroup) == 0 {
		return fallbackApprovalToolCall(item)
	}
	index := toolCallIndex(latestGroup, targetID)
	if index < 0 {
		return fallbackApprovalToolCall(item)
	}
	return append([]model.ToolCall(nil), latestGroup[index:]...), nil
}

func fallbackApprovalToolCall(item Item) ([]model.ToolCall, error) {
	name := strings.TrimSpace(item.Meta["tool_call_name"])
	if name == "" {
		return nil, errors.New("approval is missing tool_call_name")
	}
	callID := strings.TrimSpace(item.Meta["tool_call_id"])
	if callID == "" {
		callID = "approval_" + item.ID
	}
	return []model.ToolCall{{
		ID:        callID,
		Name:      name,
		Arguments: toolArgumentsBytes(item.Meta["tool_call_arguments"]),
	}}, nil
}

func toolCallsFromPayload(payload map[string]any) []model.ToolCall {
	if len(payload) == 0 {
		return nil
	}
	var rawCalls []any
	switch calls := payload["tool_calls"].(type) {
	case []any:
		rawCalls = calls
	case []map[string]any:
		rawCalls = make([]any, 0, len(calls))
		for _, call := range calls {
			rawCalls = append(rawCalls, call)
		}
	default:
		return nil
	}
	out := make([]model.ToolCall, 0, len(rawCalls))
	for _, raw := range rawCalls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := firstNonEmpty(stringValue(call["id"]), stringValue(call["call_id"]))
		name := stringValue(call["name"])
		if id == "" && name == "" {
			continue
		}
		out = append(out, model.ToolCall{ID: id, Name: name, Arguments: toolArgumentsBytes(call["arguments"])})
	}
	return out
}

func toolCallIndex(calls []model.ToolCall, id string) int {
	for i, call := range calls {
		if call.ID == id {
			return i
		}
	}
	return -1
}

func toolArgumentsBytes(value any) []byte {
	switch v := value.(type) {
	case nil:
		return nil
	case []byte:
		return append([]byte(nil), v...)
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil
		}
		if json.Valid([]byte(trimmed)) {
			return []byte(trimmed)
		}
		encoded, _ := json.Marshal(v)
		return encoded
	case map[string]any, []any:
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return encoded
	default:
		return nil
	}
}

func (s *Service) Reject(ctx context.Context, id string) (State, error) {
	state, item, err := s.item(ctx, id)
	if err != nil {
		return State{}, err
	}
	request, ok := permissionRequestForItem(item)
	if !ok {
		return State{}, fmt.Errorf("%s is not rejectable", id)
	}
	if s.Approval != nil {
		s.Approval.Reject(request)
	}
	if err := s.logPermission(ctx, "rejected", id, request); err != nil {
		return State{}, err
	}
	state, err = s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Notice = "rejected " + id
	return state, nil
}

func (s *Service) SetApproval(ctx context.Context, mode permission.Mode) (State, error) {
	if s.Approval == nil {
		return State{}, errors.New("approval gate is not configured")
	}
	s.Approval.SetMode(mode)
	if err := s.logApprovalMode(ctx, mode); err != nil {
		return State{}, err
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Notice = "approval: " + string(mode)
	return state, nil
}

func (s *Service) Compact(ctx context.Context) (State, error) {
	if s.Log == nil {
		return State{}, errors.New("event log is not configured")
	}
	event, err := contextmgr.CompactSession(ctx, s.Log, s.SessionID, 4096, s.now)
	if err != nil {
		return State{}, err
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	tokens := "unknown"
	if raw, ok := event.Payload["estimated_tokens"].(int); ok {
		tokens = strconv.Itoa(raw)
	}
	state.Notice = "compacted context ~" + tokens + " tokens"
	return state, nil
}

func (s *Service) Memory(ctx context.Context) (State, error) {
	if s.Log == nil {
		return State{}, errors.New("event log is not configured")
	}
	body, tokens, err := contextmgr.BuildSessionContext(ctx, s.Log, s.SessionID, 4096)
	if err != nil {
		return State{}, err
	}
	if strings.TrimSpace(body) == "" {
		body = "No remembered session state yet."
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Detail = Item{
		ID:       "memory",
		Kind:     "context",
		Title:    "Remembered state",
		Body:     body,
		MIMEType: "text/plain",
		Meta: map[string]string{
			"estimated_tokens": strconv.Itoa(tokens),
		},
	}
	state.Notice = "remembered state ~" + strconv.Itoa(tokens) + " tokens"
	return state, nil
}

func (s *Service) Context(ctx context.Context) (State, error) {
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	target := s.activeConversation()
	turnContext := s.submitTurnContext(ctx, target)
	if strings.TrimSpace(turnContext) == "" {
		turnContext = "No additional context would be attached beyond the new prompt."
	}
	var sections []string
	sections = append(sections,
		"Next model turn context",
		"",
		"active conversation: "+conversationLabel(target),
		"estimated transcript tokens: "+strconv.Itoa(state.TokenEstimate),
		"",
		"Included:",
	)
	if len(state.ContextPreview.Included) == 0 {
		sections = append(sections, "  - new prompt only")
	} else {
		for _, item := range state.ContextPreview.Included {
			sections = append(sections, "  - "+item)
		}
	}
	if len(state.ContextPreview.Excluded) > 0 {
		sections = append(sections, "", "Excluded:")
		for _, item := range state.ContextPreview.Excluded {
			sections = append(sections, "  - "+item)
		}
	}
	sections = append(sections, "", "Rendered context:", turnContext)
	state.Detail = Item{
		ID:       "context",
		Kind:     "context",
		Title:    "Next model context",
		Body:     strings.Join(sections, "\n"),
		MIMEType: "text/plain",
		Meta: map[string]string{
			"estimated_tokens": strconv.Itoa(state.TokenEstimate),
		},
	}
	state.Notice = "context preview ready"
	return state, nil
}

func (s *Service) Doctor(ctx context.Context) (State, error) {
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	var checks []string
	checks = append(checks, "go: "+runtime.Version()+" "+runtime.GOOS+"/"+runtime.GOARCH)
	if s.WorkspaceRoot != "" {
		checks = append(checks, "workspace: "+s.WorkspaceRoot)
	}
	if s.Git != nil {
		if status, err := s.Git.Status(ctx); err == nil {
			checks = append(checks, "git: branch "+status.Branch)
		} else {
			checks = append(checks, "git: "+err.Error())
		}
	}
	editor := firstNonEmpty(s.EditorCommand, "nvim")
	editorCommand := "nvim"
	if fields := strings.Fields(editor); len(fields) > 0 {
		editorCommand = fields[0]
	}
	if _, err := exec.LookPath(editorCommand); err == nil {
		checks = append(checks, "editor: "+editor)
	} else {
		checks = append(checks, "editor: "+err.Error())
	}
	if s.Config != nil {
		if settings, err := s.Config.Load(ctx); err == nil {
			checks = append(checks, "config: loaded")
			checks = append(checks, "active model: "+settings.ActiveModel.String())
			checks = append(checks, "write approval: "+string(settings.Permissions.Write))
		} else {
			checks = append(checks, "config: "+err.Error())
		}
	}
	if s.MCP != nil {
		status := s.MCP.Status(s.approvalMode())
		ready := 0
		failed := 0
		visibleTools := 0
		for _, server := range status.Servers {
			switch server.State {
			case "ready":
				ready++
			case "failed":
				failed++
			}
		}
		for _, tool := range status.Tools {
			if tool.Visible {
				visibleTools++
			}
		}
		checks = append(checks, fmt.Sprintf("mcp: enabled=%t ready=%d failed=%d visible_tools=%d", status.Enabled, ready, failed, visibleTools))
	} else {
		checks = append(checks, "mcp: disabled")
	}
	state.Detail = Item{
		ID:       "doctor",
		Kind:     "diagnostics",
		Title:    "Doctor",
		Body:     strings.Join(checks, "\n"),
		MIMEType: "text/plain",
	}
	state.Notice = "doctor ready"
	return state, nil
}

func (s *Service) MCPStatus(ctx context.Context) (State, error) {
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Detail = Item{
		ID:       "mcp-status",
		Kind:     "mcp",
		Title:    "MCP status",
		Body:     renderMCPStatus(s.mcpStatus(), false),
		MIMEType: "text/plain",
	}
	state.Notice = "mcp status"
	return state, nil
}

func (s *Service) MCPTools(ctx context.Context) (State, error) {
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Detail = Item{
		ID:       "mcp-tools",
		Kind:     "mcp",
		Title:    "MCP tools",
		Body:     renderMCPStatus(s.mcpStatus(), true),
		MIMEType: "text/plain",
	}
	state.Notice = "mcp tools"
	return state, nil
}

func (s *Service) MCPDoctor(ctx context.Context) (State, error) {
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Detail = Item{
		ID:       "mcp-doctor",
		Kind:     "mcp",
		Title:    "MCP doctor",
		Body:     renderMCPStatus(s.mcpStatus(), true),
		MIMEType: "text/plain",
	}
	state.Notice = "mcp doctor"
	return state, nil
}

func (s *Service) MCPReload(ctx context.Context) (State, error) {
	if s.MCP == nil {
		state, err := s.Load(ctx)
		if err != nil {
			return State{}, err
		}
		state.Notice = "mcp is disabled"
		return state, nil
	}
	if err := s.MCP.Reload(ctx); err != nil {
		return State{}, err
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Detail = Item{
		ID:       "mcp-status",
		Kind:     "mcp",
		Title:    "MCP status",
		Body:     renderMCPStatus(s.mcpStatus(), true),
		MIMEType: "text/plain",
	}
	state.Notice = "mcp reloaded"
	return state, nil
}

func (s *Service) DebugBundle(ctx context.Context) (State, error) {
	var buf bytes.Buffer
	sessionPath := ""
	if summary := currentSessionSummary(ctx, s.Sessions, s.WorkspaceRoot, s.SessionID); summary.LogPath != "" {
		sessionPath = summary.LogPath
	}
	if err := commands.WriteDebugBundle(ctx, &buf, commands.DebugBundleOptions{
		WorkDir:     s.WorkspaceRoot,
		SessionPath: sessionPath,
		SessionID:   string(s.SessionID),
		MaxBytes:    128 * 1024,
		Now:         s.now,
	}); err != nil {
		return State{}, err
	}
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Detail = Item{
		ID:       "debug-bundle",
		Kind:     "diagnostics",
		Title:    "Debug bundle",
		Body:     buf.String(),
		MIMEType: "text/markdown",
	}
	state.Notice = "debug bundle ready"
	return state, nil
}

func (s *Service) mcpStatus() ports.MCPStatus {
	if s == nil || s.MCP == nil {
		return ports.MCPStatus{}
	}
	return s.MCP.Status(s.approvalMode())
}

func renderMCPStatus(status ports.MCPStatus, includeTools bool) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("enabled: %t", status.Enabled))
	if len(status.Servers) == 0 {
		lines = append(lines, "servers: none")
	} else {
		lines = append(lines, "", "servers:")
		for _, server := range status.Servers {
			line := fmt.Sprintf("  - %s [%s] tools=%d command=%s", server.Name, firstNonEmpty(server.State, "configured"), server.ToolCount, firstNonEmpty(server.Command, "-"))
			lines = append(lines, line)
			if server.ServerInfo != "" {
				lines = append(lines, "    server: "+server.ServerInfo)
			}
			if server.LastError != "" {
				lines = append(lines, "    error: "+server.LastError)
			}
			if server.Stderr != "" {
				lines = append(lines, "    stderr: "+server.Stderr)
			}
			if server.Instruction != "" {
				lines = append(lines, "    instructions: "+server.Instruction)
			}
		}
	}
	if includeTools {
		lines = append(lines, "", "tools:")
		if len(status.Tools) == 0 {
			lines = append(lines, "  none")
		}
		for _, tool := range status.Tools {
			visibility := "visible"
			if !tool.Visible {
				visibility = "hidden: " + tool.HiddenReason
			}
			capabilities := strings.Join(tool.Capabilities, ",")
			if capabilities == "" {
				capabilities = "-"
			}
			if tool.Unclassified {
				capabilities += " (unclassified)"
			}
			lines = append(lines, fmt.Sprintf("  - %s -> %s/%s [%s] caps=%s", tool.PublicName, tool.ServerName, tool.OriginalName, visibility, capabilities))
		}
	}
	return strings.Join(lines, "\n")
}

func (s *Service) Palette(ctx context.Context) (State, error) {
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	state.Detail = Item{ID: "commands", Kind: "commands", Title: "Command Palette", Body: renderCommands(state.Commands)}
	return state, nil
}

func (s *Service) item(ctx context.Context, id string) (State, Item, error) {
	state, err := s.Load(ctx)
	if err != nil {
		return State{}, Item{}, err
	}
	item, ok := FindItem(state.Artifacts, id)
	if !ok {
		item, ok = itemFromTranscript(state.Transcript, id)
	}
	if !ok {
		item, ok = itemFromAgent(state.Agents, id)
	}
	if !ok {
		if file, found := FindWorkspaceFile(state.Files, id); found {
			item, ok = itemFromWorkspaceFile(file, "")
		}
	}
	if !ok {
		if file, found := FindWorkspaceFile(state.GitFiles, id); found {
			item, ok = itemFromWorkspaceFile(file, gitDiff(ctx, s.Git, file.Path))
		}
	}
	if !ok {
		return State{}, Item{}, fmt.Errorf("unknown item %q", id)
	}
	return state, item, nil
}

func (s *Service) applyApprovedPatch(ctx context.Context, id string, item Item) (State, error) {
	if s.Tools == nil {
		return State{}, errors.New("tool registry is not configured")
	}
	arguments := item.Meta["tool_call_arguments"]
	if arguments == "" {
		return State{}, fmt.Errorf("%s cannot be applied: original patch arguments are missing", id)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return State{}, fmt.Errorf("%s patch arguments: %w", id, err)
	}
	previewToken := item.Meta["preview_token"]
	previewArgs := cloneAnyMap(args)
	previewArgs["accepted"] = false
	delete(previewArgs, "preview_token")
	previewEncoded, err := json.Marshal(previewArgs)
	if err != nil {
		return State{}, err
	}
	previewResult, err := s.Tools.RunTool(ctx, model.ToolCall{ID: "approval_preview_" + id, Name: "apply_patch", Arguments: previewEncoded})
	if err != nil {
		return State{}, err
	}
	if previewResult.Metadata != nil && previewResult.Metadata["preview_token"] != "" {
		previewToken = previewResult.Metadata["preview_token"]
	}
	if previewToken == "" {
		return State{}, fmt.Errorf("%s cannot be applied: preview_token is missing", id)
	}
	args["accepted"] = true
	args["preview_token"] = previewToken
	encoded, err := json.Marshal(args)
	if err != nil {
		return State{}, err
	}
	call := model.ToolCall{ID: "approval_" + id, Name: "apply_patch", Arguments: encoded}
	result, err := s.Tools.RunTool(ctx, call)
	if err != nil {
		return State{}, err
	}
	if err := s.logToolResult(ctx, call, result); err != nil {
		return State{}, err
	}
	return s.Load(ctx)
}

func (s *Service) logPermission(ctx context.Context, action string, id string, request permission.Request) error {
	if s.Log == nil {
		return nil
	}
	decision := permission.DecisionDeny
	if action == "approved" {
		decision = permission.DecisionAllow
	}
	payloadText := strings.Join([]string{string(request.Action), request.Subject, request.Reason, id, action}, "\x00")
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(payloadText)))
	audit := permission.AuditDecision{
		Action:   request.Action,
		Subject:  request.Subject,
		Reason:   request.Reason,
		Decision: decision,
		Actor:    "user",
		Payload:  payloadText,
		Digest:   digest,
		At:       s.now(),
	}
	return s.Log.Append(ctx, session.Event{
		ID:        s.eventID("perm"),
		SessionID: s.SessionID,
		Type:      session.EventPermissionRequested,
		At:        s.now(),
		Actor:     "user",
		Text:      action + " " + id,
		Payload: map[string]any{
			"approval": action,
			"artifact": id,
			"action":   string(request.Action),
			"subject":  request.Subject,
			"reason":   request.Reason,
			"audit": map[string]any{
				"actor":    audit.Actor,
				"decision": string(audit.Decision),
				"digest":   audit.Digest,
				"at":       audit.At.Format(time.RFC3339Nano),
			},
		},
	})
}

func (s *Service) logApprovalMode(ctx context.Context, mode permission.Mode) error {
	if s.Log == nil {
		return nil
	}
	return s.Log.Append(ctx, session.Event{
		ID:        s.eventID("approval"),
		SessionID: s.SessionID,
		Type:      session.EventPermissionRequested,
		At:        s.now(),
		Actor:     "user",
		Text:      "approval mode " + string(mode),
		Payload: map[string]any{
			"approval_mode": string(mode),
		},
	})
}

func (s *Service) logToolResult(ctx context.Context, call model.ToolCall, result ports.ToolResult) error {
	if s.Log == nil {
		return nil
	}
	payload := stringMetadataPayload(result.Metadata)
	if payload == nil {
		payload = map[string]any{}
	}
	payload["call_id"] = call.ID
	payload["name"] = call.Name
	payload["arguments"] = string(call.Arguments)
	if result.Artifact != nil {
		payload["artifact"] = artifactPayload(*result.Artifact)
	}
	return s.Log.Append(ctx, session.Event{
		ID:        s.eventID("tool"),
		SessionID: s.SessionID,
		Type:      session.EventTool,
		At:        s.now(),
		Actor:     "tool",
		Text:      result.Content,
		Payload:   payload,
	})
}

func (s *Service) LogToolResult(ctx context.Context, call model.ToolCall, result ports.ToolResult) error {
	return s.logToolResult(ctx, call, result)
}

func (s *Service) LogToolError(ctx context.Context, call model.ToolCall, err error) error {
	if s.Log == nil {
		return nil
	}
	message := err.Error()
	payload := map[string]any{
		"call_id":   call.ID,
		"name":      call.Name,
		"arguments": string(call.Arguments),
		"error":     message,
	}
	if request, ok := permission.ApprovalRequest(err); ok {
		payload["approval_required"] = true
		payload["action"] = string(request.Action)
		payload["subject"] = request.Subject
		payload["reason"] = request.Reason
		payload["state"] = "pending"
	}
	return s.Log.Append(ctx, session.Event{
		ID:        s.eventID("tool"),
		SessionID: s.SessionID,
		Type:      session.EventTool,
		At:        s.now(),
		Actor:     "tool",
		Text:      message,
		Payload:   payload,
	})
}

func (s *Service) LogToolSkipped(ctx context.Context, call model.ToolCall, blockedBy string) error {
	if s.Log == nil {
		return nil
	}
	name := call.Name
	if name == "" {
		name = "tool"
	}
	if blockedBy == "" {
		blockedBy = "another tool call"
	}
	message := fmt.Sprintf("%s was not run because %s is waiting for approval", name, blockedBy)
	return s.Log.Append(ctx, session.Event{
		ID:        s.eventID("tool"),
		SessionID: s.SessionID,
		Type:      session.EventTool,
		At:        s.now(),
		Actor:     "tool",
		Text:      message,
		Payload: map[string]any{
			"call_id":   call.ID,
			"name":      call.Name,
			"arguments": string(call.Arguments),
			"error":     message,
			"skipped":   true,
			"state":     "skipped",
		},
	})
}

func (s *Service) approvalMode() permission.Mode {
	if s.Approval == nil {
		return permission.ModeReadOnly
	}
	return s.Approval.Mode()
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) editorCommand() string {
	return strings.TrimSpace(s.EditorCommand)
}

func (s *Service) editorState() EditorState {
	command := s.editorCommand()
	state := EditorState{Command: command, DoubleEscToReturn: s.EditorDoubleEsc}
	if command == "" {
		return state
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		state.Error = "editor command is not configured"
		return state
	}
	if _, err := exec.LookPath(fields[0]); err != nil {
		state.Error = fmt.Sprintf("Neovim is required for file editing: %s not found", fields[0])
		return state
	}
	state.Available = true
	return state
}

func (s *Service) activeConversation() ConversationTarget {
	return normalizeConversationTarget(s.ActiveConversation)
}

func normalizeConversationTarget(target ConversationTarget) ConversationTarget {
	target.Kind = strings.TrimSpace(target.Kind)
	target.ID = strings.TrimSpace(target.ID)
	target.Title = strings.TrimSpace(target.Title)
	if target.Kind == "" {
		target.Kind = "main"
	}
	if target.ID == "" {
		target.ID = target.Kind
	}
	if target.Title == "" {
		if target.Kind == "main" {
			target.Title = "Main orchestrator"
		} else {
			target.Title = target.Kind + " " + target.ID
		}
	}
	return target
}

func (s *Service) ensureEditorAvailable() error {
	command := s.editorCommand()
	if command == "" {
		return nil
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return errors.New("editor command is not configured")
	}
	if _, err := exec.LookPath(fields[0]); err != nil {
		return fmt.Errorf("Neovim is required for file editing: %s not found", fields[0])
	}
	return nil
}

func (s *Service) eventID(prefix string) session.EventID {
	return session.EventID(fmt.Sprintf("%s-%d", prefix, s.now().UnixNano()))
}

func permissionRequestForItem(item Item) (permission.Request, bool) {
	if item.Kind == "approval" {
		action := permission.Action(item.Meta["action"])
		subject := item.Meta["subject"]
		reason := item.Meta["reason"]
		if action == "" || subject == "" {
			return permission.Request{}, false
		}
		return permission.Request{Action: action, Subject: subject, Reason: reason}, true
	}
	if item.Kind != "patch" {
		return permission.Request{}, false
	}
	subject := item.Meta["changed_files"]
	if subject == "" {
		return permission.Request{}, false
	}
	reason := "apply_patch"
	if digest := item.Meta["patch_digest"]; digest != "" {
		reason += ":" + digest
	}
	return permission.Request{Action: permission.ActionWrite, Subject: subject, Reason: reason}, true
}

func pendingApprovals(items []Item, decisions map[string]string) []ApprovalItem {
	var approvals []ApprovalItem
	for _, item := range items {
		if decisions != nil && decisions[item.ID] != "" {
			continue
		}
		request, ok := permissionRequestForItem(item)
		if !ok {
			continue
		}
		if item.Kind == "patch" && item.Meta["state"] != "" && item.Meta["state"] != "preview" {
			continue
		}
		if item.Kind == "approval" && item.Meta["state"] != "" && item.Meta["state"] != "pending" {
			continue
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = "Approval required"
		}
		approvals = append(approvals, ApprovalItem{
			ID:         item.ID,
			Title:      title,
			Body:       item.Body,
			Kind:       item.Kind,
			Action:     string(request.Action),
			Subject:    request.Subject,
			Reason:     request.Reason,
			ArtifactID: item.ID,
		})
	}
	return approvals
}

type ApprovalGate struct {
	mu     sync.Mutex
	mode   permission.Mode
	grants map[approvalKey]permission.Decision
}

type approvalKey struct {
	action  permission.Action
	subject string
	reason  string
}

func NewApprovalGate(mode permission.Mode) *ApprovalGate {
	if mode == "" {
		mode = permission.ModeAsk
	}
	return &ApprovalGate{mode: mode, grants: map[approvalKey]permission.Decision{}}
}

func (g *ApprovalGate) Decide(ctx context.Context, request permission.Request) (permission.Decision, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if g == nil {
		return permission.DecisionDeny, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if decision, ok := g.grants[keyFor(request)]; ok && decision == permission.DecisionDeny {
		return permission.DecisionDeny, nil
	}
	base := permission.PolicyForMode(g.mode).DecisionFor(request.Action)
	if base != permission.DecisionAsk {
		return base, nil
	}
	if decision, ok := g.grants[keyFor(request)]; ok {
		return decision, nil
	}
	return permission.DecisionAsk, nil
}

func (g *ApprovalGate) Mode() permission.Mode {
	if g == nil {
		return permission.ModeReadOnly
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode
}

func (g *ApprovalGate) SetMode(mode permission.Mode) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = mode
}

func (g *ApprovalGate) Approve(request permission.Request) {
	g.set(request, permission.DecisionAllow)
}

func (g *ApprovalGate) Reject(request permission.Request) {
	g.set(request, permission.DecisionDeny)
}

func (g *ApprovalGate) set(request permission.Request, decision permission.Decision) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.grants == nil {
		g.grants = map[approvalKey]permission.Decision{}
	}
	g.grants[keyFor(request)] = decision
}

func keyFor(request permission.Request) approvalKey {
	return approvalKey{action: request.Action, subject: request.Subject, reason: request.Reason}
}

func cloneAnyMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func DefaultCommands() []Command {
	commands := []Command{
		command("prompt.insert", "Insert prompt", "Prompt", "Start composing a normal prompt.", "i", "prompt", "composer", "insert"),
		command("prompt.send", "Send prompt", "Prompt", "Send the composer text to the current session.", "Enter", "send", "chat", "prompt"),
		command("shell.run", "Run shell command", "Shell", "Run a local shell command and open its output as a local-only artifact.", "!", "shell", "bash", "terminal", "command"),
		command("terminal.open", "Open terminal", "Shell", "Open the persistent local terminal.", ":term/:terminal/:t", "terminal", "term", "shell", "pty"),
		command("terminal.share", "Share terminal with agent", "Shell", "Enable direct terminal_read and terminal_write tools for the selected terminal.", ":st/:share-term/:share-terminal [n]", "terminal", "share", "context", "control"),
		command("terminal.share_output", "Attach terminal output", "Shell", "Attach recent terminal output to the next model turn without granting live terminal control.", ":sto/:share-output [n]", "terminal", "output", "attach", "context"),
		command("conversation.main", "Return to main chat", "Prompt", "Switch the center pane back to the main orchestrator conversation.", "b", "main", "back", "orchestrator"),
		command("buffer.list", "List buffers", "Buffers", "List main and agent conversations as Vim-style buffers.", ":ls/:buffers", "buffers", "conversation", "agents"),
		command("buffer.switch", "Switch buffer", "Buffers", "Switch to a main or agent conversation buffer.", ":b <buffer>", "buffer", "conversation", "agent"),
		command("buffer.next", "Next buffer", "Buffers", "Switch to the next conversation buffer.", ":bn/:bnext", "buffer", "next", "conversation"),
		command("buffer.previous", "Previous buffer", "Buffers", "Switch to the previous conversation buffer.", ":bp/:bprevious", "buffer", "previous", "conversation"),
		command("agent.followup", "Send to selected agent", "Agents", "Send the composer text as a directed follow-up to the selected agent.", ":agent", "agent", "follow up", "conversation"),
		command("agent.swarm", "Start swarm run", "Agents", "Send a prompt to the main orchestrator as a long-running swarm task.", ":s/:swarm", "swarm", "agent", "staged", "long running"),
		command("session.list", "List sessions", "Sessions", "Open prior sessions for this workspace.", ":sessions", "sessions", "history", "resume"),
		command("session.new", "New session", "Sessions", "Start a fresh session for the current workspace.", ":new", "new session", "workspace"),
		command("session.rename", "Rename session", "Sessions", "Rename the active session.", ":rename", "rename", "session", "title"),
		command("session.resume", "Resume session", "Sessions", "Resume a prior session by id.", ":resume", "resume", "session", "history"),
		command("tab.files", "Show files", "Files", "Switch the right pane to project files.", "1/:files", "files", "tree", "project"),
		command("tab.artifacts", "Show artifacts", "Artifacts", "Switch the right pane to chat artifacts and pending approvals.", "2/:artifacts", "artifacts", "approvals", "patches"),
		command("tab.git", "Show Git", "Git", "Switch the right pane to changed files and diffs.", "3/:git", "git", "diff", "status"),
		command("tab.term", "Show terminal", "Shell", "Switch the right pane to the persistent local terminal.", "4", "terminal", "shell", "right pane"),
		command("tab.ops", "Show operations", "Ops", "Switch the right pane to live runs, queued prompts, approvals, terminal sharing, and context.", "5/:ops", "ops", "operations", "jobs", "queue"),
		command("tab.next", "Next right tab", "Tabs", "Cycle to the next right-pane view.", "]/:tabn", "tab", "next", "right pane"),
		command("tab.previous", "Previous right tab", "Tabs", "Cycle to the previous right-pane view.", "[:tabp", "tab", "previous", "right pane"),
		command("file.edit", "Edit file", "Files", "Open a project file in external Neovim.", ":edit/:e <file>", "edit", "nvim", "file"),
		command("item.open", "Open selected item", "Files", "Open a file in $EDITOR or show a non-file item in detail.", "o", "open", "file", "artifact"),
		command("item.detail", "Inspect selected item", "Files", "Show full details for the selected message, agent, file, code block, or patch.", "d", "detail", "inspect"),
		command("item.copy", "Copy selected item", "Files", "Copy the selected message, code block, or artifact.", "y", "copy", "clipboard"),
		command("item.copy.full", "Copy with fences", "Files", "Copy the full selected code artifact, preserving code fences when available.", "Y", "copy", "fences", "clipboard"),
		command("item.copy.select", "Copy selection mode", "Files", "Temporarily release mouse capture so terminal selection and Cmd+C can copy visible text.", "v", "copy", "mouse", "selection", "cmd+c"),
		command("mouse.toggle", "Toggle mouse capture", "Files", "Toggle terminal mouse capture. Off = native click-drag selection works in chat. On = scroll wheel scrolls panes.", "m", "mouse", "scroll", "select", "copy"),
		command("approval.approve", "Approve selected action", "Approvals", "Approve and apply the selected pending tool, patch, or write action.", "a", "approve", "apply", "permission"),
		command("approval.reject", "Reject selected action", "Approvals", "Reject the selected pending tool, patch, or write action.", "r", "reject", "permission"),
		command("approval.read_only", "Set approval read-only", "Approvals", "Disable write and shell actions for this session.", ":approval read-only", "approval", "read only", "permissions"),
		command("approval.ask", "Set approval ask", "Approvals", "Ask before workspace writes and shell actions.", ":approval ask", "approval", "ask", "permissions"),
		command("approval.auto", "Set approval auto", "Approvals", "Auto-approve normal workspace writes for this session.", ":approval auto", "approval", "auto", "permissions"),
		command("approval.danger", "Confirm danger mode", "Approvals", "Approve all tool classes for this session after explicit confirmation.", ":danger confirm", "danger", "permissions"),
		command("context.compact", "Compact context", "Context", "Write a compact summary for the active session.", ":compact", "compact", "context"),
		command("context.inspect", "Inspect next context", "Context", "Show exactly what FreeCode will attach to the next model turn.", ":context", "context", "next prompt", "model input"),
		command("context.memory", "Show remembered state", "Context", "Show the structured context FreeCode will carry into future turns.", ":memory", "memory", "handoff", "state", "context"),
		command("context.cancel", "Cancel active run", "Context", "Cancel the active model/tool run and drop queued follow-ups.", ":cancel", "cancel", "interrupt", "stop"),
		command("context.noqueue", "Clear queued prompts", "Context", "Drop queued follow-up prompts without cancelling the active run.", ":noqueue", "queue", "clear", "drop"),
		command("diagnostics.doctor", "Run doctor", "Diagnostics", "Check config, workspace, git, editor, terminal, and provider setup.", ":doctor", "doctor", "diagnostics", "health"),
		command("diagnostics.debug_bundle", "Create debug bundle", "Diagnostics", "Show a redacted diagnostic bundle for bug reports.", ":debug-bundle", "debug", "bundle", "redact", "diagnostics"),
		command("mcp.status", "MCP status", "Tools", "Show MCP server state and exposed tool counts.", ":mcp status", "mcp", "tools", "status"),
		command("mcp.tools", "MCP tools", "Tools", "List MCP tools, mappings, visibility, and capabilities.", ":mcp tools", "mcp", "tools", "capabilities"),
		command("mcp.reload", "Reload MCP servers", "Tools", "Restart configured MCP servers and refresh their tool lists.", ":mcp reload", "mcp", "reload", "restart"),
		command("mcp.doctor", "MCP doctor", "Tools", "Show MCP configuration, startup, and permission diagnostics.", ":mcp doctor", "mcp", "doctor", "diagnostics"),
		command("debug.toggle", "Toggle debug mode", "Diagnostics", "Toggle raw model chunk diagnostics.", ":debug", "debug", "diagnostics", "chunks"),
		command("debug.on", "Enable debug mode", "Diagnostics", "Enable raw model chunk diagnostics.", ":debug on", "debug", "diagnostics", "chunks"),
		command("debug.off", "Disable debug mode", "Diagnostics", "Disable raw model chunk diagnostics.", ":debug off", "debug", "diagnostics", "chunks"),
		command("model.list", "List models", "Provider", "Show configured provider/model choices.", ":models/:model", "model", "provider", "switch"),
		command("model.use", "Use model", "Provider", "Switch active provider/model.", ":use <model>", "model", "provider", "switch", "use"),
		command("settings.open", "Settings", "Provider", "Show provider, model, approval, and editor settings.", ":settings", "settings", "provider", "model", "editor"),
		command("palette.open", "Command palette", "Help", "Open searchable commands and tutorial shortcuts.", "Ctrl+K", "help", "commands", "cheat sheet"),
		command("help.tutorial", "Tutorial game", "Help", "Play a guided keybinding tutorial built from the command registry.", ":tutorial/:tutor", "tutorial", "training", "onboarding", "game", "keybindings"),
		command("quit", "Quit", "Help", "Exit FreeCode.", "q", "quit", "exit"),
	}
	plainSyntax := map[string]string{
		"prompt.insert":         "i/:i <prompt>",
		"shell.run":             "!:! <command>",
		"terminal.share_output": ":sto [n]",
		"conversation.main":     "b/:main",
		"agent.followup":        ":agent <prompt>",
		"agent.swarm":           ":s/:swarm <prompt>",
		"item.open":             "o/:o f1[:line]|m1",
		"item.detail":           "d/:d p1|m1|a1",
		"item.copy":             "y c1|m1",
		"item.copy.full":        "Y c1|m1",
		"approval.approve":      "a p1",
		"approval.reject":       "r p1",
	}
	argKinds := map[string]CommandArgKind{
		"prompt.insert":         CommandArgPrompt,
		"shell.run":             CommandArgShell,
		"terminal.open":         CommandArgTerminal,
		"tab.term":              CommandArgTerminal,
		"terminal.share":        CommandArgTerminal,
		"terminal.share_output": CommandArgTerminal,
		"buffer.switch":         CommandArgConversation,
		"agent.followup":        CommandArgPrompt,
		"agent.swarm":           CommandArgPrompt,
		"session.rename":        CommandArgPrompt,
		"session.resume":        CommandArgSession,
		"file.edit":             CommandArgFile,
		"item.open":             CommandArgFile,
		"item.detail":           CommandArgFile,
		"item.copy":             CommandArgFile,
		"item.copy.full":        CommandArgFile,
		"approval.approve":      CommandArgApproval,
		"approval.reject":       CommandArgApproval,
		"approval.read_only":    CommandArgApproval,
		"approval.ask":          CommandArgApproval,
		"approval.auto":         CommandArgApproval,
		"approval.danger":       CommandArgApproval,
		"model.use":             CommandArgModel,
	}
	for i := range commands {
		if syntax := plainSyntax[commands[i].ID]; syntax != "" {
			commands[i].Key = syntax
		}
		if kind := argKinds[commands[i].ID]; kind != "" {
			commands[i].ArgKind = string(kind)
		}
	}
	return commands
}

func command(id string, title string, category string, description string, keybinding string, keywords ...string) Command {
	keybindings := splitKeybinding(keybinding)
	return Command{
		ID:             id,
		Title:          title,
		Category:       category,
		Description:    description,
		Keybinding:     keybinding,
		Aliases:        normalizeAliases(keybindings),
		Keybindings:    keybindings,
		Scopes:         []string{string(CommandScopeGlobal)},
		Key:            keybinding,
		Keywords:       keywords,
		Enabled:        true,
		PaletteVisible: true,
	}
}

func splitKeybinding(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '/' || r == ','
	})
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func FilterCommands(commands []Command, query string) []Command {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return append([]Command(nil), commands...)
	}
	var filtered []Command
	for _, command := range commands {
		if commandMatches(command, query) {
			filtered = append(filtered, command)
		}
	}
	return filtered
}

func commandMatches(command Command, query string) bool {
	haystack := []string{
		command.ID,
		command.Title,
		command.Category,
		command.Description,
		command.Keybinding,
		command.Key,
		command.DisabledReason,
	}
	haystack = append(haystack, command.Keywords...)
	for _, value := range haystack {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func itemFromTranscript(items []TranscriptItem, id string) (Item, bool) {
	for _, item := range items {
		if item.ID == id {
			title := strings.TrimSpace(firstNonEmpty(item.Title, item.Actor))
			if title == "" {
				title = "message"
			}
			meta := map[string]string{"actor": item.Actor}
			if item.Kind != "" {
				meta["transcript_kind"] = string(item.Kind)
			}
			if item.Status != "" {
				meta["status"] = item.Status
			}
			if item.ArtifactID != "" {
				meta["artifact_id"] = item.ArtifactID
			}
			if item.Streaming {
				meta["streaming"] = "true"
			}
			for key, value := range item.Meta {
				if value != "" {
					meta[key] = value
				}
			}
			return Item{
				ID:    item.ID,
				Kind:  "message",
				Title: title,
				Body:  item.Text,
				Meta:  meta,
			}, true
		}
	}
	return Item{}, false
}

func itemFromAgent(items []AgentItem, id string) (Item, bool) {
	for _, item := range items {
		if item.ID == id {
			title := strings.TrimSpace(strings.Join([]string{item.Name, item.Role, item.Status}, " "))
			if title == "" {
				title = "agent"
			}
			body := agentDetailBody(item)
			meta := map[string]string{
				"name":           item.Name,
				"role":           item.Role,
				"status":         item.Status,
				"task_id":        item.TaskID,
				"summary":        item.Summary,
				"blocked_reason": item.BlockedReason,
				"current_step":   item.CurrentStep,
			}
			for key, value := range item.Meta {
				if _, exists := meta[key]; !exists {
					meta[key] = value
				}
			}
			return Item{
				ID:    item.ID,
				Kind:  "agent",
				Title: title,
				Body:  body,
				Meta:  meta,
			}, true
		}
	}
	return Item{}, false
}

func itemFromWorkspaceFile(file WorkspaceFile, body string) (Item, bool) {
	if strings.TrimSpace(file.Path) == "" {
		return Item{}, false
	}
	kind := strings.TrimSpace(file.Kind)
	if kind == "" {
		kind = "file"
	}
	title := firstNonEmpty(file.StatusLine, file.Name, file.Path)
	if strings.TrimSpace(body) == "" {
		body = file.Path
		if file.StatusLine != "" {
			body = file.StatusLine + "\n" + file.Path
		}
	}
	return Item{
		ID:    file.ID,
		Kind:  kind,
		Title: title,
		Body:  body,
		URI:   "file:" + file.Path,
		Meta: map[string]string{
			"path":        file.Path,
			"status":      file.Status,
			"status_line": file.StatusLine,
		},
	}, true
}

func agentDetailBody(item AgentItem) string {
	var lines []string
	appendField := func(label string, value string) {
		if strings.TrimSpace(value) != "" {
			lines = append(lines, label+": "+strings.TrimSpace(value))
		}
	}
	appendList := func(label string, values []string) {
		if len(values) == 0 {
			return
		}
		lines = append(lines, label+":")
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				lines = append(lines, "  "+strings.TrimSpace(value))
			}
		}
	}
	appendField("Task", item.TaskID)
	appendField("Agent", item.Name)
	appendField("Role", item.Role)
	appendField("Status", item.Status)
	appendField("Current step", item.CurrentStep)
	appendField("Summary", firstNonEmpty(item.Summary, item.Text))
	appendField("Blocked", item.BlockedReason)
	appendList("Changed files", item.ChangedFiles)
	appendList("Tests run", item.TestsRun)
	appendList("Findings", item.Findings)
	appendList("Questions", item.Questions)
	if len(lines) == 0 {
		return strings.TrimSpace(item.Text)
	}
	return strings.Join(lines, "\n")
}

func FindItem(items []Item, id string) (Item, bool) {
	id, _ = SplitArtifactLine(id)
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return Item{}, false
}

func FindWorkspaceFile(files []WorkspaceFile, ref string) (WorkspaceFile, bool) {
	id, _ := SplitArtifactLine(ref)
	id = strings.TrimSpace(id)
	for _, file := range files {
		if id == file.ID || id == file.Path || id == file.Name {
			return file, true
		}
	}
	return WorkspaceFile{}, false
}

func NewSessionID(at time.Time) string {
	if at.IsZero() {
		at = time.Now()
	}
	return at.UTC().Format("20060102-150405-000000000")
}

func SplitArtifactLine(ref string) (string, int) {
	ref = strings.TrimSpace(ref)
	id, rawLine, ok := strings.Cut(ref, ":")
	if !ok {
		return ref, 0
	}
	line, err := strconv.Atoi(rawLine)
	if err != nil || line < 0 {
		return id, 0
	}
	return id, line
}

type codeBlock struct {
	Language string
	Body     string
	Fenced   string
}

func extractCodeBlocks(text string) []codeBlock {
	lines := strings.Split(text, "\n")
	var blocks []codeBlock
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		language := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
		var body []string
		fenced := []string{lines[i]}
		for j := i + 1; j < len(lines); j++ {
			fenced = append(fenced, lines[j])
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
				blocks = append(blocks, codeBlock{
					Language: language,
					Body:     strings.Join(body, "\n"),
					Fenced:   strings.Join(fenced, "\n"),
				})
				i = j
				break
			}
			body = append(body, lines[j])
		}
	}
	return blocks
}

func artifactFromPayload(payload map[string]any) (Item, bool) {
	if len(payload) == 0 {
		return Item{}, false
	}
	raw, ok := payload["artifact"].(map[string]any)
	if !ok {
		return Item{}, false
	}
	item := Item{
		ID:       stringValue(raw["id"]),
		Kind:     stringValue(raw["kind"]),
		Title:    stringValue(raw["title"]),
		Body:     stringValue(raw["body"]),
		URI:      stringValue(raw["uri"]),
		MIMEType: stringValue(raw["mime_type"]),
		Meta:     map[string]string{},
	}
	if item.Kind == "" {
		item.Kind = "artifact"
	}
	if item.Title == "" {
		item.Title = item.Kind
	}
	if metadata, ok := raw["metadata"].(map[string]any); ok {
		for key, value := range metadata {
			item.Meta[key] = stringValue(value)
		}
	}
	if name := stringValue(payload["name"]); name != "" {
		item.Meta["tool_call_name"] = name
	}
	if arguments := stringValue(payload["arguments"]); arguments != "" {
		item.Meta["tool_call_arguments"] = arguments
	}
	return item, true
}

func approvalFromToolEvent(number int, event session.Event) (Item, bool) {
	if event.Type != session.EventTool {
		return Item{}, false
	}
	name := stringValue(event.Payload["name"])
	errText := stringValue(event.Payload["error"])
	action := permission.Action(stringValue(event.Payload["action"]))
	subject := stringValue(event.Payload["subject"])
	reason := stringValue(event.Payload["reason"])
	if action == "" && name == "run_check" && strings.Contains(errText, "shell permission requires approval") {
		action = permission.ActionShell
		subject = runCheckCommand(event.Payload["arguments"])
		reason = "run_check"
	}
	if action == "" || !strings.Contains(errText, "permission requires approval") {
		return Item{}, false
	}
	if subject == "" {
		subject = firstNonEmpty(name, "tool")
	}
	if reason == "" {
		reason = name
	}
	id := fmt.Sprintf("u%d", number)
	return Item{
		ID:    id,
		Kind:  "approval",
		Title: fmt.Sprintf("Approve %s: %s", action, subject),
		Body:  strings.TrimSpace(event.Text),
		Meta: map[string]string{
			"action":              string(action),
			"subject":             subject,
			"reason":              reason,
			"state":               "pending",
			"tool_call_id":        firstNonEmpty(stringValue(event.Payload["call_id"]), stringValue(event.Payload["tool_call_id"]), stringValue(event.Payload["id"])),
			"tool_call_name":      name,
			"tool_call_error":     errText,
			"tool_call_arguments": stringValue(event.Payload["arguments"]),
		},
	}, true
}

func runCheckCommand(raw any) string {
	arguments := stringValue(raw)
	if strings.TrimSpace(arguments) == "" {
		return ""
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.Command)
}

func agentFromEvent(number int, event session.Event) AgentItem {
	meta := map[string]string{}
	for key, value := range event.Payload {
		if scalar := scalarString(value); scalar != "" {
			meta[key] = scalar
		}
	}
	summary := firstNonEmpty(
		stringValue(event.Payload["summary"]),
		stringValue(event.Payload["goal"]),
		event.Text,
	)
	status := firstNonEmpty(stringValue(event.Payload["status"]), meta["status"])
	blocked := firstNonEmpty(
		stringValue(event.Payload["blocked_reason"]),
		stringValue(event.Payload["blocker"]),
		stringValue(event.Payload["error"]),
	)
	if blocked == "" && (status == "blocked" || status == "failed") {
		blocked = strings.TrimSpace(event.Text)
	}
	return AgentItem{
		ID:            fmt.Sprintf("a%d", number),
		Name:          event.Actor,
		Role:          stringValue(event.Payload["role"]),
		Status:        status,
		TaskID:        stringValue(event.Payload["task_id"]),
		Summary:       summary,
		ChangedFiles:  stringListValue(event.Payload["changed_files"]),
		TestsRun:      stringListValue(event.Payload["tests_run"]),
		Findings:      stringListValue(event.Payload["findings"]),
		Questions:     firstNonEmptyList(stringListValue(event.Payload["open_questions"]), stringListValue(event.Payload["questions"])),
		BlockedReason: blocked,
		CurrentStep:   firstNonEmpty(stringValue(event.Payload["current_step"]), stringValue(event.Payload["step"])),
		Meta:          meta,
		Text:          event.Text,
	}
}

func agentIdentity(item AgentItem) string {
	if item.TaskID != "" {
		return "task:" + item.TaskID
	}
	return ""
}

func isSwarmLifecycleAgent(item AgentItem) bool {
	return item.Name == "swarm" && item.TaskID == ""
}

func mergeAgentItem(previous AgentItem, next AgentItem) AgentItem {
	next.ID = previous.ID
	next.Name = firstNonEmpty(next.Name, previous.Name)
	next.Role = firstNonEmpty(next.Role, previous.Role)
	next.Status = firstNonEmpty(next.Status, previous.Status)
	next.TaskID = firstNonEmpty(next.TaskID, previous.TaskID)
	next.Summary = firstNonEmpty(next.Summary, previous.Summary)
	next.BlockedReason = firstNonEmpty(next.BlockedReason, previous.BlockedReason)
	next.CurrentStep = firstNonEmpty(next.CurrentStep, previous.CurrentStep)
	next.Text = firstNonEmpty(next.Text, previous.Text)
	if len(next.ChangedFiles) == 0 {
		next.ChangedFiles = previous.ChangedFiles
	}
	if len(next.TestsRun) == 0 {
		next.TestsRun = previous.TestsRun
	}
	if len(next.Findings) == 0 {
		next.Findings = previous.Findings
	}
	if len(next.Questions) == 0 {
		next.Questions = previous.Questions
	}
	if len(previous.Meta) > 0 || len(next.Meta) > 0 {
		merged := map[string]string{}
		for key, value := range previous.Meta {
			merged[key] = value
		}
		for key, value := range next.Meta {
			merged[key] = value
		}
		next.Meta = merged
	}
	return next
}

func renderCommands(commands []Command) string {
	var lines []string
	for _, command := range commands {
		key := command.Key
		if key == "" {
			key = command.Keybinding
		}
		title := command.Title
		if title == "" {
			title = command.Description
		}
		lines = append(lines, fmt.Sprintf("%-18s %-22s %s", key, title, command.Description))
	}
	return strings.Join(lines, "\n")
}

func cleanGitStatusPath(line string) string {
	line = strings.TrimRight(line, "\r\n")
	if len(line) >= 3 && (line[2] == ' ' || line[2] == '\t') {
		line = strings.TrimSpace(line[3:])
	} else {
		line = strings.TrimSpace(line)
	}
	if _, after, ok := strings.Cut(line, " -> "); ok {
		line = after
	}
	return strings.Trim(line, `"`)
}

func gitStatusCode(line string) string {
	line = strings.TrimRight(line, "\n")
	if len(line) < 2 {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(line[:2])
}

func gitDiff(ctx context.Context, git ports.Git, path string) string {
	if git == nil || strings.TrimSpace(path) == "" {
		return ""
	}
	diff, err := git.Diff(ctx, []string{path})
	if err != nil {
		return err.Error()
	}
	if strings.TrimSpace(diff) == "" {
		return "No unstaged diff for " + path
	}
	return diff
}

func currentBranch(ctx context.Context, git ports.Git) string {
	if git == nil {
		return ""
	}
	status, err := git.Status(ctx)
	if err != nil {
		return ""
	}
	return status.Branch
}

func gitChangedSet(ctx context.Context, git ports.Git) map[string]bool {
	result := map[string]bool{}
	if git == nil {
		return result
	}
	status, err := git.Status(ctx)
	if err != nil {
		return result
	}
	for _, path := range status.ChangedFiles {
		clean := cleanGitStatusPath(path)
		if clean != "" {
			result[clean] = true
		}
	}
	return result
}

func fileFingerprint(root string, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	fullPath := path
	if !filepath.IsAbs(fullPath) && strings.TrimSpace(root) != "" {
		fullPath = filepath.Join(root, path)
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func changedAfterEdit(before map[string]bool, after map[string]bool) []string {
	if len(after) == 0 {
		return nil
	}
	same := mapsEqualBool(before, after)
	var changed []string
	for path := range after {
		if !before[path] || !same {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

func mapsEqualBool(a map[string]bool, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func defaultSessionTitle(root string, id session.ID) string {
	name := filepath.Base(strings.TrimSpace(root))
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = "workspace"
	}
	return name + " " + string(id)
}

func completionCandidates(state State) []CompletionCandidate {
	var out []CompletionCandidate
	seen := map[string]bool{}
	add := func(kind, value, label string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := kind + "\x00" + value
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, CompletionCandidate{Kind: kind, Value: value, Label: firstNonEmpty(label, value)})
	}
	for _, command := range state.Commands {
		key := firstNonEmpty(command.Keybinding, command.Key)
		if strings.HasPrefix(key, ":") {
			add("command", key, command.Title)
		}
	}
	for _, value := range []string{":sessions", ":new", ":rename", ":resume", ":ls", ":buffers", ":b", ":bn", ":bp", ":files", ":artifacts", ":git", ":ops", ":term", ":terminal", ":t", ":share-term", ":share-terminal", ":st", ":sto", ":share-output", ":tabn", ":tabp", ":Explore", ":edit", ":e", ":agent", ":s", ":swarm", ":!", ":settings", ":compact", ":context", ":memory", ":cancel", ":noqueue", ":approval"} {
		add("command", value, strings.TrimPrefix(value, ":"))
	}
	for _, summary := range state.Sessions {
		add("session", string(summary.ID), summary.Title)
	}
	for _, agent := range state.Agents {
		add("agent", agent.ID, firstNonEmpty(agent.Name, agent.Role, agent.Status))
	}
	for _, file := range state.Files {
		add("file", file.Path, file.Name)
	}
	for _, file := range state.GitFiles {
		add("git", file.Path, firstNonEmpty(file.StatusLine, file.Name))
	}
	return out
}

func countKind(items []Item, kind string) int {
	var count int
	for _, item := range items {
		if item.Kind == kind {
			count++
		}
	}
	return count
}

func artifactSortKey(id string) string {
	if id == "" {
		return "~"
	}
	prefix := id[:1]
	rawNumber := id[1:]
	number, err := strconv.Atoi(rawNumber)
	if err != nil {
		return id
	}
	return fmt.Sprintf("%s%08d", prefix, number)
}

func stringMetadataPayload(metadata map[string]string) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	payload := make(map[string]any, len(metadata))
	for key, value := range metadata {
		payload[key] = value
	}
	return payload
}

func artifactPayload(value artifact.Artifact) map[string]any {
	payload := map[string]any{
		"id":   value.ID.String(),
		"kind": string(value.Kind),
	}
	if value.Title != "" {
		payload["title"] = value.Title
	}
	if value.URI != "" {
		payload["uri"] = value.URI
	}
	if value.MIMEType != "" {
		payload["mime_type"] = value.MIMEType
	}
	if value.Body != "" {
		payload["body"] = value.Body
	}
	if len(value.Metadata) > 0 {
		metadata := make(map[string]any, len(value.Metadata))
		for key, item := range value.Metadata {
			metadata[key] = item
		}
		payload["metadata"] = metadata
	}
	return payload
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

func codeTitle(language string, number int) string {
	if language == "" {
		return fmt.Sprintf("code block %d", number)
	}
	return fmt.Sprintf("%s code block %d", language, number)
}

func codeMIME(language string) string {
	if language == "" {
		return "text/plain"
	}
	return "text/x-" + language
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}

// assistantDiagnosticHint extracts a human-readable summary from the
// orchestrator's diagnostics payload (logged on empty / no-response model
// turns). Returns "" when no diagnostics were attached.
func assistantDiagnosticHint(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	diag, ok := payload["diagnostics"].(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	if reason := stringValue(diag["finish_reason"]); reason != "" {
		parts = append(parts, "finish="+reason)
	}
	if dropped := intValue(diag["dropped_calls"]); dropped > 0 {
		parts = append(parts, fmt.Sprintf("dropped %d malformed tool calls", dropped))
	}
	if len(parts) == 0 {
		if chunks := intValue(diag["chunk_count"]); chunks > 0 {
			parts = append(parts, fmt.Sprintf("got %d chunks but no usable content", chunks))
		}
	}
	return strings.Join(parts, "; ")
}

func intValue(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func isReasoningPayload(payload map[string]any) bool {
	if len(payload) == 0 {
		return false
	}
	if v, ok := payload["reasoning"].(bool); ok && v {
		return true
	}
	return false
}

// assistantPayloadHasToolCalls reports whether an EventAssistantMessage
// event was logged for an assistant turn that called tools (vs a turn
// that produced no text and no tool_calls — the second case is a real
// "empty response" we want to surface to the user).
func assistantPayloadHasToolCalls(payload map[string]any) bool {
	if len(payload) == 0 {
		return false
	}
	calls, ok := payload["tool_calls"].([]any)
	if ok && len(calls) > 0 {
		return true
	}
	// orchestrator may also encode as []map[string]any.
	if mapped, ok := payload["tool_calls"].([]map[string]any); ok && len(mapped) > 0 {
		return true
	}
	return false
}

func mapStringAny(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	default:
		return nil
	}
}

func transcriptEventMeta(event session.Event) map[string]string {
	meta := map[string]string{}
	for _, key := range []string{"agent", "agent_id", "task_session", "task_id", "role", "status", "name", "call_id", "arguments", "error"} {
		if value := scalarString(event.Payload[key]); value != "" {
			meta[key] = value
		}
	}
	return meta
}

func compactTranscriptToolRequests(items []TranscriptItem) []TranscriptItem {
	if len(items) == 0 {
		return items
	}
	resolved := map[string]bool{}
	for _, item := range items {
		if item.Status == "requested" {
			continue
		}
		if callID := firstNonEmpty(item.Meta["call_id"], item.Meta["tool_call_id"]); callID != "" {
			resolved[callID] = true
		}
	}
	if len(resolved) == 0 {
		return items
	}
	out := make([]TranscriptItem, 0, len(items))
	for _, item := range items {
		if item.Kind == TranscriptTool && item.Status == "requested" {
			if callID := firstNonEmpty(item.Meta["tool_call_id"], item.Meta["call_id"]); callID != "" && resolved[callID] {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func cloneStringMap(input map[string]string) map[string]string {
	output := map[string]string{}
	for key, value := range input {
		output[key] = value
	}
	return output
}

func conversationTranscript(items []TranscriptItem, agents []AgentItem, target ConversationTarget) []TranscriptItem {
	target = normalizeConversationTarget(target)
	if target.Kind != "agent" {
		filtered := make([]TranscriptItem, 0, len(items))
		for _, item := range items {
			if item.Kind == TranscriptAgent || item.Meta["task_session"] == "" {
				filtered = append(filtered, item)
			}
		}
		return filtered
	}
	var selected AgentItem
	for _, item := range agents {
		if item.ID == target.ID || item.TaskID == target.ID || item.Name == target.ID {
			selected = item
			break
		}
	}
	filtered := make([]TranscriptItem, 0, len(items))
	for _, item := range items {
		if item.Meta["agent_id"] == target.ID {
			filtered = append(filtered, item)
			continue
		}
		if selected.ID == "" {
			continue
		}
		if item.Meta["task_session"] != "" && item.Meta["task_session"] == selected.TaskID {
			filtered = append(filtered, item)
			continue
		}
		if item.Meta["task_id"] != "" && item.Meta["task_id"] == selected.TaskID {
			filtered = append(filtered, item)
			continue
		}
		if item.Meta["agent"] != "" && item.Meta["agent"] == selected.Name {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 && selected.ID != "" {
		filtered = append(filtered, TranscriptItem{
			ID:     selected.ID,
			Kind:   TranscriptAgent,
			Actor:  selected.Name,
			Title:  agentTranscriptTitle(selected),
			Text:   firstNonEmpty(selected.Summary, selected.Text),
			Status: selected.Status,
			Meta:   selected.Meta,
		})
	}
	return filtered
}

func agentTranscriptTitle(item AgentItem) string {
	parts := compactStrings([]string{item.Name, item.Role, item.Status})
	if len(parts) == 0 {
		return "agent"
	}
	return strings.Join(parts, " ")
}

func scalarString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func stringListValue(value any) []string {
	switch typed := value.(type) {
	case []string:
		return compactStrings(typed)
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if scalar := scalarString(item); scalar != "" {
				values = append(values, scalar)
			}
		}
		return compactStrings(values)
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		fields := strings.FieldsFunc(typed, func(r rune) bool {
			return r == ',' || r == '\n' || r == '\t'
		})
		return compactStrings(fields)
	default:
		return nil
	}
}

func compactStrings(values []string) []string {
	var result []string
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstNonEmptyList(values ...[]string) []string {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}
