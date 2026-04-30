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
	maxFiles         int
	maxReadBytes     int
	maxSearchResults int
}

func NewReadOnly(fs ports.FileSystem) *ReadOnly {
	return &ReadOnly{
		fs:               fs,
		maxFiles:         defaultMaxFiles,
		maxReadBytes:     defaultMaxReadBytes,
		maxSearchResults: defaultMaxSearchResults,
	}
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
	files, err := r.fs.ListFiles(ctx, args.Pattern)
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

	files, err := r.fs.ListFiles(ctx, args.Path)
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
