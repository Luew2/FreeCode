package builtin

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Luew2/FreeCode/internal/core/artifact"
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/ports"
)

const applyPatchName = "apply_patch"

type ApplyPatch struct {
	fs       ports.FileSystem
	gate     ports.PermissionGate
	mu       sync.Mutex
	applyMu  sync.Mutex
	counter  int
	previews *PreviewStore
	now      func() time.Time
}

func NewApplyPatch(fs ports.FileSystem, gate ports.PermissionGate) *ApplyPatch {
	return &ApplyPatch{fs: fs, gate: gate, previews: NewPreviewStore(256, 30*time.Minute), now: time.Now}
}

func (t *ApplyPatch) ToolSpec() model.ToolSpec {
	return model.ToolSpec{
		Name:        applyPatchName,
		Description: "Preview or apply exact-text workspace patches. Calls preview by default; set accepted true only after reviewing the preview.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"summary": map[string]any{"type": "string"},
				"accepted": map[string]any{
					"type":        "boolean",
					"description": "When false or omitted, preview only. When true, apply the exact patch after preview/review.",
				},
				"preview_token": map[string]any{
					"type":        "string",
					"description": "Required with accepted=true. Must match the preview_token returned by a prior preview of the exact same patch.",
				},
				"changes": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":     map[string]any{"type": "string"},
							"old_text": map[string]any{"type": "string"},
							"new_text": map[string]any{"type": "string"},
						},
						"required": []string{"path", "new_text"},
					},
				},
			},
			"required": []string{"changes"},
		},
	}
}

func (t *ApplyPatch) Run(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	if t == nil || t.fs == nil {
		return ports.ToolResult{}, errors.New("apply_patch is not configured")
	}

	var args applyPatchArgs
	if err := decodeArgs(call.Arguments, &args); err != nil {
		return ports.ToolResult{}, err
	}
	if len(args.Changes) == 0 {
		return ports.ToolResult{}, errors.New("changes are required")
	}

	plan, err := t.plan(ctx, args)
	if err != nil {
		return ports.ToolResult{}, err
	}

	id := t.nextPatchID()
	summary := strings.TrimSpace(args.Summary)
	if summary == "" {
		summary = fmt.Sprintf("Patch %d change(s)", len(args.Changes))
	}
	body := renderPatchArtifact(plan)
	digest := patchDigest(plan)
	now := t.clock()
	state := "preview"
	if args.Accepted {
		if err := t.authorize(ctx, plan.files, digest); err != nil {
			return ports.ToolResult{}, err
		}
		t.applyMu.Lock()
		defer t.applyMu.Unlock()
		entry, err := t.consumePreview(args.PreviewToken, digest, plan.files, now)
		if err != nil {
			return ports.ToolResult{}, err
		}
		if err := t.revalidate(ctx, plan); err != nil {
			return ports.ToolResult{}, fmt.Errorf("%w; no files were changed by FreeCode", err)
		}
		if err := t.preflightWrites(ctx, plan); err != nil {
			return ports.ToolResult{}, fmt.Errorf("%w; no files were changed by FreeCode", err)
		}
		if err := t.writePlan(ctx, plan); err != nil {
			return ports.ToolResult{}, err
		}
		args.PreviewToken = entry.Token
		state = "applied"
	} else {
		entry := t.storePreview(digest, plan.files, call.ID, now)
		args.PreviewToken = entry.Token
	}

	content := fmt.Sprintf("%s patch %s\nchanged files:\n%s", state, id.String(), strings.Join(plan.files, "\n"))
	if args.PreviewToken != "" {
		content += "\npreview_token: " + args.PreviewToken
	}
	content += "\n\n" + body
	metadata := map[string]string{
		"patch_id":      id.String(),
		"changed_files": strings.Join(plan.files, ","),
		"change_count":  fmt.Sprintf("%d", len(args.Changes)),
		"patch_digest":  digest,
		"state":         state,
	}
	if args.PreviewToken != "" {
		metadata["preview_token"] = args.PreviewToken
		if entry, ok := t.preview(args.PreviewToken); ok {
			metadata["preview_expires_at"] = entry.ExpiresAt.Format(time.RFC3339)
			metadata["preview_call_id"] = entry.CallID
		}
	}
	return ports.ToolResult{
		CallID:  call.ID,
		Content: content,
		Artifact: &artifact.Artifact{
			ID:        id,
			Kind:      artifact.KindPatch,
			Title:     summary,
			Body:      body,
			MIMEType:  "text/x-patch",
			URI:       "patch:" + id.String(),
			CreatedAt: time.Now(),
			Metadata:  metadata,
		},
		Metadata: metadata,
	}, nil
}

func (t *ApplyPatch) clock() time.Time {
	if t != nil && t.now != nil {
		return t.now().UTC()
	}
	return time.Now().UTC()
}

type applyPatchArgs struct {
	Summary      string        `json:"summary"`
	Accepted     bool          `json:"accepted"`
	PreviewToken string        `json:"preview_token"`
	Changes      []patchChange `json:"changes"`
}

type patchChange struct {
	Path    string  `json:"path"`
	OldText *string `json:"old_text"`
	NewText *string `json:"new_text"`
}

type patchPlan struct {
	before map[string]string
	after  map[string]string
	exists map[string]bool
	files  []string
}

func (t *ApplyPatch) plan(ctx context.Context, args applyPatchArgs) (patchPlan, error) {
	plan := patchPlan{
		before: map[string]string{},
		after:  map[string]string{},
		exists: map[string]bool{},
	}
	changed := map[string]bool{}

	for i, change := range args.Changes {
		path, err := cleanToolPath(change.Path)
		if err != nil {
			return patchPlan{}, fmt.Errorf("change %d path: %w", i+1, err)
		}
		if change.NewText == nil {
			return patchPlan{}, fmt.Errorf("change %d new_text is required", i+1)
		}

		current, loaded := plan.after[path]
		if !loaded {
			data, readErr := t.fs.ReadFile(ctx, path)
			if readErr != nil {
				if change.OldText != nil {
					return patchPlan{}, fmt.Errorf("read %s: %w", path, readErr)
				}
				if !errors.Is(readErr, os.ErrNotExist) {
					return patchPlan{}, fmt.Errorf("read %s: %w", path, readErr)
				}
				current = ""
			} else {
				if change.OldText == nil {
					return patchPlan{}, fmt.Errorf("create %s: file already exists", path)
				}
				current = string(data)
				plan.exists[path] = true
			}
			plan.before[path] = current
			plan.after[path] = current
		} else if change.OldText == nil {
			return patchPlan{}, fmt.Errorf("create %s: duplicate create change", path)
		}

		if change.OldText == nil {
			plan.after[path] = *change.NewText
			changed[path] = true
			continue
		}
		oldText := *change.OldText
		if oldText == "" {
			return patchPlan{}, fmt.Errorf("change %d old_text must be non-empty for replacements", i+1)
		}
		occurrences := strings.Count(plan.after[path], oldText)
		switch occurrences {
		case 0:
			return patchPlan{}, fmt.Errorf("stale patch for %s: old_text was not found", path)
		case 1:
			plan.after[path] = strings.Replace(plan.after[path], oldText, *change.NewText, 1)
			changed[path] = true
		default:
			return patchPlan{}, fmt.Errorf("ambiguous patch for %s: old_text matched %d times", path, occurrences)
		}
	}

	for path := range changed {
		if plan.before[path] != plan.after[path] {
			plan.files = append(plan.files, path)
		}
	}
	if len(plan.files) == 0 {
		return patchPlan{}, errors.New("patch made no changes")
	}
	sort.Strings(plan.files)
	return plan, nil
}

func (t *ApplyPatch) revalidate(ctx context.Context, plan patchPlan) error {
	for _, path := range plan.files {
		data, err := t.fs.ReadFile(ctx, path)
		if !plan.exists[path] {
			if err == nil {
				return fmt.Errorf("stale patch for %s: file was created before apply", path)
			}
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if string(data) != plan.before[path] {
			return fmt.Errorf("stale patch for %s: file changed before apply", path)
		}
	}
	return nil
}

func (t *ApplyPatch) preflightWrites(ctx context.Context, plan patchPlan) error {
	for i, left := range plan.files {
		for _, right := range plan.files[i+1:] {
			if isPathAncestor(left, right) || isPathAncestor(right, left) {
				return fmt.Errorf("patch has conflicting paths %q and %q", left, right)
			}
		}

		parts := strings.Split(left, "/")
		for i := 1; i < len(parts); i++ {
			parent := strings.Join(parts[:i], "/")
			if data, err := t.fs.ReadFile(ctx, parent); err == nil && data != nil {
				return fmt.Errorf("cannot write %s: parent path %s is a file", left, parent)
			}
		}
	}
	return nil
}

func (t *ApplyPatch) writePlan(ctx context.Context, plan patchPlan) error {
	written := []string{}
	for _, path := range plan.files {
		if err := t.fs.WriteFile(ctx, path, []byte(plan.after[path])); err != nil {
			return t.rollback(ctx, plan, append(written, path), fmt.Errorf("write %s: %w", path, err))
		}
		written = append(written, path)
	}
	return nil
}

func (t *ApplyPatch) rollback(ctx context.Context, plan patchPlan, written []string, writeErr error) error {
	var rollbackErrors []string
	createdDirs := map[string]bool{}
	for i := len(written) - 1; i >= 0; i-- {
		path := written[i]
		var err error
		if plan.exists[path] {
			err = t.fs.WriteFile(ctx, path, []byte(plan.before[path]))
		} else {
			for _, dir := range parentDirs(path) {
				createdDirs[dir] = true
			}
			err = t.fs.RemoveFile(ctx, path)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		if err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Sprintf("%s: %v", path, err))
		}
	}
	dirs := make([]string, 0, len(createdDirs))
	for dir := range createdDirs {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/")
	})
	for _, dir := range dirs {
		err := t.fs.RemoveDir(ctx, dir)
		if err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrExist) {
			continue
		}
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("%s: %v", dir, err))
	}
	if len(rollbackErrors) > 0 {
		return fmt.Errorf("%w; rollback failed: %s", writeErr, strings.Join(rollbackErrors, "; "))
	}
	return writeErr
}

type PreviewEntry struct {
	Token     string
	Digest    string
	Files     []string
	CallID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type PreviewStore struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	entries map[string]PreviewEntry
	order   []string
}

func NewPreviewStore(max int, ttl time.Duration) *PreviewStore {
	if max <= 0 {
		max = 256
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &PreviewStore{max: max, ttl: ttl, entries: map[string]PreviewEntry{}}
}

func (s *PreviewStore) Store(digest string, files []string, callID string, now time.Time) PreviewEntry {
	if s == nil {
		s = NewPreviewStore(256, 30*time.Minute)
	}
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	token := randomPreviewToken()
	entry := PreviewEntry{
		Token:     token,
		Digest:    digest,
		Files:     append([]string(nil), files...),
		CallID:    callID,
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}
	s.entries[token] = entry
	s.order = append(s.order, token)
	s.pruneLocked(now)
	return entry
}

func (s *PreviewStore) Consume(token string, digest string, files []string, now time.Time) (PreviewEntry, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return PreviewEntry{}, errors.New("accepted patch requires preview_token from a prior preview")
	}
	if s == nil {
		return PreviewEntry{}, errors.New("accepted patch has no prior preview")
	}
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[token]
	if !ok {
		s.pruneLocked(now)
		return PreviewEntry{}, fmt.Errorf("unknown preview_token %q", token)
	}
	delete(s.entries, token)
	s.removeOrderLocked(token)
	if entry.Digest != digest || !sameStringSet(entry.Files, files) {
		return PreviewEntry{}, errors.New("accepted patch does not match preview_token")
	}
	if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
		return PreviewEntry{}, fmt.Errorf("preview_token %q expired at %s", token, entry.ExpiresAt.Format(time.RFC3339))
	}
	s.pruneLocked(now)
	return entry, nil
}

func (s *PreviewStore) Get(token string, now time.Time) (PreviewEntry, bool) {
	if s == nil {
		return PreviewEntry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now.UTC())
	entry, ok := s.entries[strings.TrimSpace(token)]
	return entry, ok
}

func (s *PreviewStore) pruneLocked(now time.Time) {
	for len(s.order) > 0 {
		token := s.order[0]
		entry, ok := s.entries[token]
		if !ok || (!entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt)) || len(s.order) > s.max {
			delete(s.entries, token)
			s.order = s.order[1:]
			continue
		}
		break
	}
}

func (s *PreviewStore) removeOrderLocked(token string) {
	for i, value := range s.order {
		if value == token {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}

func (t *ApplyPatch) storePreview(digest string, files []string, callID string, now time.Time) PreviewEntry {
	if t.previews == nil {
		t.previews = NewPreviewStore(256, 30*time.Minute)
	}
	return t.previews.Store(digest, files, callID, now)
}

func (t *ApplyPatch) consumePreview(token string, digest string, files []string, now time.Time) (PreviewEntry, error) {
	if t.previews == nil {
		return PreviewEntry{}, errors.New("accepted patch has no prior preview")
	}
	return t.previews.Consume(token, digest, files, now)
}

func (t *ApplyPatch) preview(token string) (PreviewEntry, bool) {
	if t == nil || t.previews == nil {
		return PreviewEntry{}, false
	}
	return t.previews.Get(token, t.clock())
}

func (t *ApplyPatch) authorize(ctx context.Context, files []string, digest string) error {
	if t.gate == nil {
		return errors.New("write permission denied: permission gate is not configured")
	}
	reason := "apply_patch"
	if digest != "" {
		reason += ":" + digest
	}
	decision, err := t.gate.Decide(ctx, permission.Request{
		Action:  permission.ActionWrite,
		Subject: strings.Join(files, ","),
		Reason:  reason,
	})
	if err != nil {
		return err
	}
	switch decision {
	case permission.DecisionAllow:
		return nil
	case permission.DecisionAsk:
		return errors.New("write permission requires approval")
	default:
		return errors.New("write permission denied")
	}
}

func isPathAncestor(parent string, child string) bool {
	parent = strings.TrimSuffix(parent, "/")
	child = strings.TrimSuffix(child, "/")
	return child != parent && strings.HasPrefix(child, parent+"/")
}

func parentDirs(path string) []string {
	dir := filepath.ToSlash(filepath.Dir(path))
	if dir == "." || dir == "/" {
		return nil
	}
	var dirs []string
	for dir != "." && dir != "/" {
		dirs = append(dirs, dir)
		next := filepath.ToSlash(filepath.Dir(dir))
		if next == dir {
			break
		}
		dir = next
	}
	return dirs
}

func patchDigest(plan patchPlan) string {
	var b strings.Builder
	for _, file := range plan.files {
		b.WriteString(file)
		b.WriteByte(0)
		b.WriteString(plan.before[file])
		b.WriteByte(0)
		b.WriteString(plan.after[file])
		b.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func sameStringSet(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func randomPreviewToken() string {
	var bytes [16]byte
	if _, err := crand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
}

func (t *ApplyPatch) nextPatchID() artifact.ID {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counter++
	return artifact.NewID(artifact.KindPatch, t.counter)
}

func cleanToolPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("%q must be relative to the workspace", path)
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside workspace", path)
	}
	return filepath.ToSlash(clean), nil
}

func renderPatchArtifact(plan patchPlan) string {
	var b strings.Builder
	for _, path := range plan.files {
		b.WriteString("--- a/")
		b.WriteString(path)
		b.WriteByte('\n')
		b.WriteString("+++ b/")
		b.WriteString(path)
		b.WriteByte('\n')
		b.WriteString("@@\n")
		for _, line := range splitPatchLines(plan.before[path]) {
			b.WriteByte('-')
			b.WriteString(line)
			b.WriteByte('\n')
		}
		for _, line := range splitPatchLines(plan.after[path]) {
			b.WriteByte('+')
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func splitPatchLines(value string) []string {
	lines := strings.Split(value, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
