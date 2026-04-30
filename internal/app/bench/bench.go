package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Luew2/FreeCode/internal/app/commands"
	"github.com/Luew2/FreeCode/internal/app/swarm"
	"github.com/Luew2/FreeCode/internal/app/workbench"
	"github.com/Luew2/FreeCode/internal/core/agent"
	"github.com/Luew2/FreeCode/internal/core/artifact"
	"github.com/Luew2/FreeCode/internal/core/config"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
	"github.com/Luew2/FreeCode/internal/ports"
)

type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
)

type Task struct {
	Name string
	Run  func(context.Context) error
}

type Options struct {
	Task  string
	Tasks []Task
	Now   func() time.Time
}

type Result struct {
	TaskName string
	Status   Status
	Duration time.Duration
	Error    string
}

func Run(ctx context.Context, opts Options) ([]Result, error) {
	tasks := opts.Tasks
	if len(tasks) == 0 {
		tasks = DefaultTasks()
	}
	if opts.Task != "" && opts.Task != "all" {
		var selected []Task
		for _, task := range tasks {
			if task.Name == opts.Task {
				selected = append(selected, task)
				break
			}
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("unknown benchmark task %q", opts.Task)
		}
		tasks = selected
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	results := make([]Result, 0, len(tasks))
	for _, task := range tasks {
		start := now()
		result := Result{TaskName: task.Name, Status: StatusPass}
		if task.Run == nil {
			result.Status = StatusFail
			result.Error = "task runner is not configured"
		} else if err := task.Run(ctx); err != nil {
			result.Status = StatusFail
			result.Error = err.Error()
		}
		result.Duration = now().Sub(start)
		results = append(results, result)
	}
	return results, nil
}

func DefaultTasks() []Task {
	return []Task{
		{Name: "add-provider", Run: benchmarkAddProvider},
		{Name: "copy-code-block", Run: benchmarkCopyCodeBlock},
		{Name: "open-changed-file", Run: benchmarkOpenChangedFile},
		{Name: "review-patch", Run: benchmarkReviewPatch},
		{Name: "toggle-auto-approval", Run: benchmarkToggleAutoApproval},
		{Name: "swarm-mocked-agents", Run: benchmarkSwarmMockedAgents},
		{Name: "recover-failed-edit", Run: benchmarkRecoverFailedEdit},
	}
}

func TaskNames() []string {
	tasks := DefaultTasks()
	names := make([]string, 0, len(tasks))
	for _, task := range tasks {
		names = append(names, task.Name)
	}
	return names
}

func AllPassed(results []Result) bool {
	for _, result := range results {
		if result.Status != StatusPass {
			return false
		}
	}
	return true
}

func FormatResults(w io.Writer, results []Result) error {
	if _, err := fmt.Fprintln(w, "freecode bench"); err != nil {
		return err
	}
	passed := 0
	for _, result := range results {
		if result.Status == StatusPass {
			passed++
		}
		line := fmt.Sprintf("%s %s %s", strings.ToUpper(string(result.Status)), result.TaskName, roundDuration(result.Duration))
		if result.Error != "" {
			line += " " + result.Error
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	status := "PASS"
	if passed != len(results) {
		status = "FAIL"
	}
	_, err := fmt.Fprintf(w, "%s %d/%d\n", status, passed, len(results))
	return err
}

func roundDuration(duration time.Duration) time.Duration {
	if duration < time.Millisecond {
		return duration.Round(time.Microsecond)
	}
	return duration.Round(time.Millisecond)
}

func benchmarkAddProvider(ctx context.Context) error {
	store := &memoryConfigStore{settings: config.DefaultSettings()}
	var out strings.Builder
	err := commands.AddProvider(ctx, &out, store, nil, commands.ProviderAddOptions{
		Name:      "arbitrary",
		BaseURL:   "https://models.example.test/v1",
		APIKeyEnv: "ARBITRARY_API_KEY",
		Model:     "coder",
		Protocol:  commands.ProviderProtocolOpenAIChat,
		SkipProbe: true,
	})
	if err != nil {
		return err
	}
	settings, err := store.Load(ctx)
	if err != nil {
		return err
	}
	ref := model.NewRef("arbitrary", "coder")
	provider, ok := settings.Providers["arbitrary"]
	if !ok {
		return errors.New("provider was not stored")
	}
	if provider.Secret.Name != "ARBITRARY_API_KEY" {
		return fmt.Errorf("secret ref = %q, want env var name", provider.Secret.Name)
	}
	if _, ok := settings.Models[ref]; !ok {
		return errors.New("model was not stored")
	}
	if settings.ActiveModel != ref {
		return fmt.Errorf("active model = %s, want %s", settings.ActiveModel, ref)
	}
	if strings.Contains(out.String(), "sk-") {
		return errors.New("provider output leaked an API key-shaped value")
	}
	return nil
}

func benchmarkCopyCodeBlock(ctx context.Context) error {
	log := newMemoryLog()
	log.mustAppend(session.Event{
		ID:        "e1",
		SessionID: "bench",
		Type:      session.EventAssistantMessage,
		Actor:     "assistant",
		Text:      "Use this:\n```go\nfmt.Println(\"bench\")\n```",
	})
	clipboard := &fakeClipboard{}
	service := &workbench.Service{
		Log:       log,
		Clipboard: clipboard,
		SessionID: "bench",
	}
	state, err := service.Copy(ctx, "c1", false)
	if err != nil {
		return err
	}
	if state.Notice != "copied c1" {
		return fmt.Errorf("notice = %q, want copied c1", state.Notice)
	}
	if clipboard.text != "fmt.Println(\"bench\")" {
		return fmt.Errorf("clipboard = %q, want code block body", clipboard.text)
	}
	return nil
}

func benchmarkOpenChangedFile(ctx context.Context) error {
	editor := &fakeEditor{}
	service := &workbench.Service{
		Git:       fakeGit{status: ports.GitStatus{Branch: "main", ChangedFiles: []string{"M  README.md"}}},
		Editor:    editor,
		SessionID: "bench",
	}
	state, err := service.Open(ctx, "f1:17")
	if err != nil {
		return err
	}
	if state.Notice != "opened f1" {
		return fmt.Errorf("notice = %q, want opened f1", state.Notice)
	}
	if editor.path != "README.md" || editor.line != 17 {
		return fmt.Errorf("editor opened %s:%d, want README.md:17", editor.path, editor.line)
	}
	return nil
}

func benchmarkReviewPatch(ctx context.Context) error {
	log := newMemoryLog()
	log.mustAppend(patchEvent("bench", "call_preview", `{"summary":"edit","changes":[{"path":"README.md","old_text":"old\n","new_text":"new\n"}]}`, "preview-token", "preview", "--- a/README.md\n+++ b/README.md\n@@\n-old\n+new\n"))
	service := &workbench.Service{Log: log, SessionID: "bench"}
	state, err := service.Detail(ctx, "p1")
	if err != nil {
		return err
	}
	if state.Detail.Kind != "patch" || !strings.Contains(state.Detail.Body, "+new") {
		return fmt.Errorf("patch detail = %#v, want reviewable patch", state.Detail)
	}
	if state.Detail.Meta["state"] != "preview" {
		return fmt.Errorf("patch state = %q, want preview", state.Detail.Meta["state"])
	}
	return nil
}

func benchmarkToggleAutoApproval(ctx context.Context) error {
	gate := workbench.NewApprovalGate(permission.ModeReadOnly)
	service := &workbench.Service{Approval: gate, SessionID: "bench"}
	state, err := service.SetApproval(ctx, permission.ModeAuto)
	if err != nil {
		return err
	}
	if state.Approval != permission.ModeAuto || !strings.Contains(state.Notice, "auto") {
		return fmt.Errorf("approval state = %s notice %q, want auto", state.Approval, state.Notice)
	}
	checks := []struct {
		action permission.Action
		want   permission.Decision
	}{
		{permission.ActionWrite, permission.DecisionAllow},
		{permission.ActionNetwork, permission.DecisionAsk},
		{permission.ActionDestructiveGit, permission.DecisionDeny},
	}
	for _, check := range checks {
		got, err := gate.Decide(ctx, permission.Request{Action: check.action, Subject: "bench", Reason: "benchmark"})
		if err != nil {
			return err
		}
		if got != check.want {
			return fmt.Errorf("%s decision = %s, want %s", check.action, got, check.want)
		}
	}
	return nil
}

func benchmarkSwarmMockedAgents(ctx context.Context) error {
	log := newMemoryLog()
	useCase := swarm.UseCase{
		Runner: fakeAgentRunner{},
		Log:    log,
		Now:    fixedNow,
	}
	response, err := useCase.Run(ctx, swarm.Request{
		SessionID: "bench",
		Goal:      "mock a swarm task",
		Approval:  permission.ModeAuto,
		MaxSteps:  3,
	})
	if err != nil {
		return err
	}
	if response.Status != agent.StatusCompleted {
		return fmt.Errorf("swarm status = %s, want completed", response.Status)
	}
	if len(response.Results) != 5 {
		return fmt.Errorf("swarm results = %d, want 5", len(response.Results))
	}
	for _, result := range response.Results {
		if result.Status != agent.StatusCompleted {
			return fmt.Errorf("%s result = %s, want completed", result.Role, result.Status)
		}
	}
	if len(log.events) < 12 {
		return fmt.Errorf("logged events = %d, want visible swarm trace", len(log.events))
	}
	return nil
}

func benchmarkRecoverFailedEdit(ctx context.Context) error {
	log := newMemoryLog()
	tools := &recoveringPatchTools{files: map[string]string{"README.md": "changed elsewhere\n"}}
	service := &workbench.Service{
		Log:       log,
		Tools:     tools,
		Approval:  workbench.NewApprovalGate(permission.ModeAuto),
		SessionID: "bench",
		Now:       fixedNow,
	}

	log.mustAppend(patchEvent("bench", "call_stale", `{"summary":"edit","changes":[{"path":"README.md","old_text":"old\n","new_text":"new\n"}]}`, "stale-token", "preview", "--- a/README.md\n+++ b/README.md\n@@\n-old\n+new\n"))
	if _, err := service.Approve(ctx, "p1"); err == nil {
		return errors.New("stale edit unexpectedly applied")
	}
	if tools.files["README.md"] != "changed elsewhere\n" {
		return fmt.Errorf("failed edit changed README.md to %q", tools.files["README.md"])
	}

	log.mustAppend(patchEvent("bench", "call_recovery", `{"summary":"recover","changes":[{"path":"README.md","old_text":"changed elsewhere\n","new_text":"recovered\n"}]}`, "recovery-token", "preview", "--- a/README.md\n+++ b/README.md\n@@\n-changed elsewhere\n+recovered\n"))
	state, err := service.Approve(ctx, "p2")
	if err != nil {
		return err
	}
	if tools.files["README.md"] != "recovered\n" {
		return fmt.Errorf("recovery content = %q, want recovered", tools.files["README.md"])
	}
	if state.Notice != "approved and applied p2" {
		return fmt.Errorf("notice = %q, want approved recovery", state.Notice)
	}
	return nil
}

func patchEvent(sessionID session.ID, callID string, arguments string, previewToken string, state string, body string) session.Event {
	return session.Event{
		ID:        session.EventID(callID),
		SessionID: sessionID,
		Type:      session.EventTool,
		Actor:     "tool",
		Text:      state + " patch p1",
		Payload: map[string]any{
			"call_id":   callID,
			"name":      "apply_patch",
			"arguments": arguments,
			"artifact": map[string]any{
				"kind":      "patch",
				"title":     "benchmark patch",
				"body":      body,
				"uri":       "patch:p1",
				"mime_type": "text/x-patch",
				"metadata": map[string]any{
					"state":         state,
					"changed_files": "README.md",
					"preview_token": previewToken,
					"patch_digest":  previewToken + "-digest",
				},
			},
		},
	}
}

type memoryConfigStore struct {
	mu       sync.Mutex
	settings config.Settings
}

func (s *memoryConfigStore) Load(ctx context.Context) (config.Settings, error) {
	if err := ctx.Err(); err != nil {
		return config.Settings{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings.Version == 0 {
		s.settings = config.DefaultSettings()
	}
	return cloneSettings(s.settings), nil
}

func (s *memoryConfigStore) Save(ctx context.Context, settings config.Settings) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = cloneSettings(settings)
	return nil
}

func cloneSettings(settings config.Settings) config.Settings {
	clone := settings
	clone.Providers = map[model.ProviderID]model.Provider{}
	for key, value := range settings.Providers {
		clone.Providers[key] = value
	}
	clone.Models = map[model.Ref]model.Model{}
	for key, value := range settings.Models {
		clone.Models[key] = value
	}
	clone.Agents = append([]agent.Definition(nil), settings.Agents...)
	return clone
}

type memoryLog struct {
	mu     sync.Mutex
	events []session.Event
}

func newMemoryLog() *memoryLog {
	return &memoryLog{}
}

func (l *memoryLog) mustAppend(event session.Event) {
	l.events = append(l.events, event)
}

func (l *memoryLog) Append(ctx context.Context, event session.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
	return nil
}

func (l *memoryLog) Stream(ctx context.Context, id session.ID) (<-chan session.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	events := append([]session.Event(nil), l.events...)
	l.mu.Unlock()

	ch := make(chan session.Event)
	go func() {
		defer close(ch)
		for _, event := range events {
			if id == "" || event.SessionID == id {
				ch <- event
			}
		}
	}()
	return ch, nil
}

type fakeClipboard struct {
	text string
}

func (c *fakeClipboard) Copy(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.text = text
	return nil
}

type fakeEditor struct {
	path string
	line int
}

func (e *fakeEditor) Open(ctx context.Context, path string, line int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.path = path
	e.line = line
	return nil
}

type fakeGit struct {
	status ports.GitStatus
	diff   string
}

func (g fakeGit) Status(ctx context.Context) (ports.GitStatus, error) {
	if err := ctx.Err(); err != nil {
		return ports.GitStatus{}, err
	}
	return g.status, nil
}

func (g fakeGit) Diff(ctx context.Context, paths []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return g.diff, nil
}

type fakeAgentRunner struct{}

func (fakeAgentRunner) RunAgent(ctx context.Context, task agent.Task) (agent.Result, error) {
	if err := ctx.Err(); err != nil {
		return agent.Result{}, err
	}
	result := agent.Result{
		TaskID:  task.ID,
		Role:    task.Role,
		Status:  agent.StatusCompleted,
		Summary: string(task.Role) + " completed",
	}
	if task.Role == agent.RoleWorker {
		result.ChangedFiles = []string{"README.md"}
	}
	if task.Role == agent.RoleVerifier {
		result.TestsRun = []string{"mock verification"}
	}
	return result, nil
}

type recoveringPatchTools struct {
	files map[string]string
}

func (t *recoveringPatchTools) Tools() []model.ToolSpec {
	return []model.ToolSpec{{Name: "apply_patch"}}
}

func (t *recoveringPatchTools) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ports.ToolResult{}, err
	}
	if call.Name != "apply_patch" {
		return ports.ToolResult{}, fmt.Errorf("unknown tool %q", call.Name)
	}
	var args struct {
		Accepted     bool   `json:"accepted"`
		PreviewToken string `json:"preview_token"`
		Changes      []struct {
			Path    string `json:"path"`
			OldText string `json:"old_text"`
			NewText string `json:"new_text"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return ports.ToolResult{}, err
	}
	if len(args.Changes) != 1 {
		return ports.ToolResult{}, fmt.Errorf("changes = %d, want 1", len(args.Changes))
	}
	change := args.Changes[0]
	if t.files[change.Path] != change.OldText {
		return ports.ToolResult{}, fmt.Errorf("stale patch for %s: old_text was not found", change.Path)
	}
	if !args.Accepted {
		metadata := map[string]string{
			"state":         "preview",
			"changed_files": change.Path,
			"preview_token": "bench-preview",
		}
		return ports.ToolResult{
			CallID:   call.ID,
			Content:  "preview patch p1\nchanged files:\n" + change.Path + "\npreview_token: bench-preview",
			Metadata: metadata,
			Artifact: &artifact.Artifact{
				ID:       artifact.NewID(artifact.KindPatch, 1),
				Kind:     artifact.KindPatch,
				Title:    "preview benchmark patch",
				Body:     "--- a/" + change.Path + "\n+++ b/" + change.Path + "\n",
				MIMEType: "text/x-patch",
				URI:      "patch:p1",
				Metadata: metadata,
			},
		}, nil
	}
	if args.PreviewToken == "" {
		return ports.ToolResult{}, errors.New("preview token is required")
	}
	t.files[change.Path] = change.NewText
	metadata := map[string]string{
		"state":         "applied",
		"changed_files": change.Path,
	}
	return ports.ToolResult{
		CallID:   call.ID,
		Content:  "applied patch p2\nchanged files:\n" + change.Path,
		Metadata: metadata,
		Artifact: &artifact.Artifact{
			ID:       artifact.NewID(artifact.KindPatch, 2),
			Kind:     artifact.KindPatch,
			Title:    "applied benchmark patch",
			Body:     "--- a/" + change.Path + "\n+++ b/" + change.Path + "\n",
			MIMEType: "text/x-patch",
			URI:      "patch:p2",
			Metadata: metadata,
		},
	}, nil
}

func fixedNow() time.Time {
	return time.Unix(1700000000, 0).UTC()
}
