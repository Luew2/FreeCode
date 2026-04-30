package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

type DebugBundleOptions struct {
	WorkDir     string
	ConfigPath  string
	SessionPath string
	SessionID   string
	MaxBytes    int64
	Now         func() time.Time
}

func WriteDebugBundle(ctx context.Context, w io.Writer, opts DebugBundleOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 256 * 1024
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	if _, err := fmt.Fprintf(w, "FreeCode debug bundle\ncreated: %s\ncwd: %s\nsession_id: %s\n\n", now().UTC().Format(time.RFC3339), opts.WorkDir, opts.SessionID); err != nil {
		return err
	}
	if opts.ConfigPath != "" {
		if err := writeRedactedFile(ctx, w, "config", opts.ConfigPath, opts.MaxBytes); err != nil {
			return err
		}
	}
	if opts.SessionPath != "" {
		if err := writeRedactedFile(ctx, w, "session", opts.SessionPath, opts.MaxBytes); err != nil {
			return err
		}
	}
	return nil
}

func writeRedactedFile(ctx context.Context, w io.Writer, label string, path string, limit int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		_, err = fmt.Fprintf(w, "## %s\n%s not found\n\n", label, path)
		return err
	}
	if err != nil {
		_, writeErr := fmt.Fprintf(w, "## %s\n%s: %v\n\n", label, path, err)
		return writeErr
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
	}
	body := RedactSecrets(string(data))
	if truncated {
		body += "\n[truncated]\n"
	}
	_, err = fmt.Fprintf(w, "## %s\npath: %s\n```text\n%s\n```\n\n", label, path, strings.TrimSpace(body))
	return err
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization:[ \t]*bearer[ \t]+)[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)(api[_-]?key["' \t:=]+)[A-Za-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`(?i)(secret["' \t:=]+)[A-Za-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`(?i)(password["' \t:=]+)[^ \t\r\n"'` + "`" + `]+`),
	regexp.MustCompile(`\b(lilac_sk_)[A-Za-z0-9]+`),
	regexp.MustCompile(`\b(sk-[A-Za-z0-9]{8})[A-Za-z0-9]+`),
}

func RedactSecrets(text string) string {
	out := text
	for _, pattern := range secretPatterns {
		out = pattern.ReplaceAllString(out, "${1}[REDACTED]")
	}
	return out
}
