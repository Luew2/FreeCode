package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/ports"
)

var _ ports.ToolRegistry = (*ReadOnly)(nil)

const (
	defaultMaxFiles         = 500
	defaultMaxReadBytes     = 64 * 1024
	defaultMaxSearchResults = 50
)

type ReadOnly struct {
	fs               ports.FileSystem
	gate             ports.PermissionGate
	maxFiles         int
	maxReadBytes     int
	maxSearchResults int
}

// NewReadOnly builds an unrestricted read registry. All reads are
// allowed; use NewReadOnlyWithGate when reads must obey a permission
// policy or path scopes.
func NewReadOnly(fs ports.FileSystem) *ReadOnly {
	return &ReadOnly{
		fs:               fs,
		maxFiles:         defaultMaxFiles,
		maxReadBytes:     defaultMaxReadBytes,
		maxSearchResults: defaultMaxSearchResults,
	}
}

// NewReadOnlyWithGate wires a permission gate into the read tools. The gate
// is consulted for permission.ActionRead before each read, list, or search
// operation. List/search results are also filtered against the gate so a
// session with AllowedPaths or DeniedPaths cannot enumerate or grep paths
// outside its scope.
func NewReadOnlyWithGate(fs ports.FileSystem, gate ports.PermissionGate) *ReadOnly {
	tools := NewReadOnly(fs)
	tools.gate = gate
	return tools
}

func (r *ReadOnly) Tools() []model.ToolSpec {
	return []model.ToolSpec{
		{
			Name:        "list_files",
			Description: "List files in the workspace. Optional pattern filters by glob, basename glob, or substring.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "read_file",
			Description: "Read a UTF-8 text file from the workspace by relative path.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "search_text",
			Description: "Search workspace text files for a query. Optional path filters files by glob, basename glob, or substring.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
					"path":  map[string]any{"type": "string"},
				},
				"required": []string{"query"},
			},
		},
	}
}

func (r *ReadOnly) RunTool(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	if r == nil || r.fs == nil {
		return ports.ToolResult{}, errors.New("read tools are not configured")
	}

	switch call.Name {
	case "list_files":
		return r.listFiles(ctx, call)
	case "read_file":
		return r.readFile(ctx, call)
	case "search_text":
		return r.searchText(ctx, call)
	default:
		return ports.ToolResult{}, fmt.Errorf("unknown tool %q", call.Name)
	}
}

type listFilesArgs struct {
	Pattern string `json:"pattern"`
}

func (r *ReadOnly) listFiles(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	var args listFilesArgs
	if err := decodeArgs(call.Arguments, &args); err != nil {
		return ports.ToolResult{}, err
	}
	if err := r.checkRead(ctx, ""); err != nil {
		return ports.ToolResult{}, err
	}
	files, err := r.fs.ListFiles(ctx, args.Pattern)
	if err != nil {
		return ports.ToolResult{}, err
	}
	files, err = r.filterReadable(ctx, files)
	if err != nil {
		return ports.ToolResult{}, err
	}
	limit := r.limitFiles()
	truncated := false
	if len(files) > limit {
		files = files[:limit]
		truncated = true
	}

	content := strings.Join(files, "\n")
	if content == "" {
		content = "no files found"
	}
	metadata := map[string]string{"count": fmt.Sprintf("%d", len(files))}
	if truncated {
		metadata["truncated"] = "true"
		content += "\n[truncated]"
	}
	return ports.ToolResult{CallID: call.ID, Content: content, Metadata: metadata}, nil
}

type readFileArgs struct {
	Path string `json:"path"`
}

func (r *ReadOnly) readFile(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	var args readFileArgs
	if err := decodeArgs(call.Arguments, &args); err != nil {
		return ports.ToolResult{}, err
	}
	if strings.TrimSpace(args.Path) == "" {
		return ports.ToolResult{}, errors.New("path is required")
	}
	if err := r.checkRead(ctx, args.Path); err != nil {
		return ports.ToolResult{}, err
	}
	data, err := r.fs.ReadFile(ctx, args.Path)
	if err != nil {
		return ports.ToolResult{}, err
	}
	limit := r.limitReadBytes()
	truncated := false
	if len(data) > limit {
		data = data[:limit]
		truncated = true
	}

	content := string(data)
	metadata := map[string]string{
		"path": filepath.ToSlash(args.Path),
	}
	if truncated {
		metadata["truncated"] = "true"
		content += "\n[truncated]"
	}
	return ports.ToolResult{CallID: call.ID, Content: content, Metadata: metadata}, nil
}

type searchTextArgs struct {
	Query string `json:"query"`
	Path  string `json:"path"`
}

func (r *ReadOnly) searchText(ctx context.Context, call model.ToolCall) (ports.ToolResult, error) {
	var args searchTextArgs
	if err := decodeArgs(call.Arguments, &args); err != nil {
		return ports.ToolResult{}, err
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return ports.ToolResult{}, errors.New("query is required")
	}
	if err := r.checkRead(ctx, ""); err != nil {
		return ports.ToolResult{}, err
	}

	files, err := r.fs.ListFiles(ctx, args.Path)
	if err != nil {
		return ports.ToolResult{}, err
	}
	files, err = r.filterReadable(ctx, files)
	if err != nil {
		return ports.ToolResult{}, err
	}

	results := []string{}
	maxResults := r.limitSearchResults()
	for _, file := range files {
		data, err := r.fs.ReadFile(ctx, file)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := scanner.Text()
			if strings.Contains(line, query) {
				results = append(results, fmt.Sprintf("%s:%d:%s", file, lineNumber, strings.TrimSpace(line)))
				if len(results) >= maxResults {
					return ports.ToolResult{
						CallID:   call.ID,
						Content:  strings.Join(append(results, "[truncated]"), "\n"),
						Metadata: map[string]string{"count": fmt.Sprintf("%d", len(results)), "truncated": "true"},
					}, nil
				}
			}
		}
	}

	content := strings.Join(results, "\n")
	if content == "" {
		content = "no matches found"
	}
	return ports.ToolResult{
		CallID:   call.ID,
		Content:  content,
		Metadata: map[string]string{"count": fmt.Sprintf("%d", len(results))},
	}, nil
}

func decodeArgs(data []byte, target any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode tool arguments: %w", err)
	}
	return nil
}

func (r *ReadOnly) limitFiles() int {
	if r.maxFiles <= 0 {
		return defaultMaxFiles
	}
	return r.maxFiles
}

func (r *ReadOnly) limitReadBytes() int {
	if r.maxReadBytes <= 0 {
		return defaultMaxReadBytes
	}
	return r.maxReadBytes
}

func (r *ReadOnly) limitSearchResults() int {
	if r.maxSearchResults <= 0 {
		return defaultMaxSearchResults
	}
	return r.maxSearchResults
}

// checkRead consults the gate for a read action against subject. A nil
// gate means "no policy configured" and reads are allowed. Allow returns
// nil; ask returns "approval required"; deny returns "permission denied".
func (r *ReadOnly) checkRead(ctx context.Context, subject string) error {
	if r == nil || r.gate == nil {
		return nil
	}
	decision, err := r.gate.Decide(ctx, permission.Request{
		Action:  permission.ActionRead,
		Subject: subject,
		Reason:  "read",
	})
	if err != nil {
		return err
	}
	switch decision {
	case permission.DecisionAllow:
		return nil
	case permission.DecisionAsk:
		return errors.New("read permission requires approval")
	default:
		return errors.New("read permission denied")
	}
}

// filterReadable drops entries the gate would reject for ActionRead. Used
// by list_files and search_text so a path-scoped session cannot enumerate
// files outside its scope. Hard errors propagate; per-path Decide errors
// drop the path silently (treat as "not readable").
func (r *ReadOnly) filterReadable(ctx context.Context, paths []string) ([]string, error) {
	if r == nil || r.gate == nil {
		return paths, nil
	}
	allowed := make([]string, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		decision, err := r.gate.Decide(ctx, permission.Request{
			Action:  permission.ActionRead,
			Subject: path,
			Reason:  "list",
		})
		if err != nil {
			// A gate error other than ctx-cancellation is treated as deny:
			// fail closed, do not leak paths the policy cannot evaluate.
			continue
		}
		if decision == permission.DecisionAllow {
			allowed = append(allowed, path)
		}
	}
	return allowed, nil
}
