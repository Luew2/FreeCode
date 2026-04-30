package localfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Luew2/FreeCode/internal/ports"
)

var _ ports.Workspace = (*Workspace)(nil)
var _ ports.FileSystem = (*FileSystem)(nil)

type Workspace struct {
	root string
	fs   *FileSystem
}

func New(root string) (*Workspace, error) {
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	files := &FileSystem{root: abs}
	return &Workspace{root: abs, fs: files}, nil
}

func (w *Workspace) Root() string {
	return w.root
}

func (w *Workspace) FileSystem() ports.FileSystem {
	return w.fs
}

func (w *Workspace) Git() ports.Git {
	return noopGit{}
}

func (w *Workspace) Editor() ports.Editor {
	return noopEditor{}
}

type FileSystem struct {
	root string
}

func (f *FileSystem) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolved, _, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(resolved)
}

func (f *FileSystem) WriteFile(ctx context.Context, path string, contents []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resolved, _, err := f.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return err
	}
	return os.WriteFile(resolved, contents, 0o600)
}

func (f *FileSystem) RemoveFile(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resolved, _, err := f.resolve(path)
	if err != nil {
		return err
	}
	return os.Remove(resolved)
}

func (f *FileSystem) RemoveDir(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resolved, _, err := f.resolve(path)
	if err != nil {
		return err
	}
	return os.Remove(resolved)
}

func (f *FileSystem) ListFiles(ctx context.Context, pattern string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	files := []string{}
	err := filepath.WalkDir(f.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == f.root {
			return nil
		}

		rel, err := filepath.Rel(f.root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			switch rel {
			case ".git", ".freecode":
				return filepath.SkipDir
			}
			return nil
		}
		if pattern != "" && !matchesPattern(rel, pattern) {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func (f *FileSystem) resolve(path string) (string, string, error) {
	if f == nil || f.root == "" {
		return "", "", errors.New("workspace root is not configured")
	}
	if strings.TrimSpace(path) == "" {
		return "", "", errors.New("path is required")
	}

	var candidate string
	if filepath.IsAbs(path) {
		candidate = filepath.Clean(path)
	} else {
		candidate = filepath.Join(f.root, filepath.Clean(path))
	}

	resolved := candidate
	if evaluated, err := filepath.EvalSymlinks(candidate); err == nil {
		resolved = evaluated
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	} else {
		existingParent := filepath.Dir(candidate)
		for {
			if _, statErr := os.Stat(existingParent); statErr == nil {
				break
			}
			next := filepath.Dir(existingParent)
			if next == existingParent {
				return "", "", err
			}
			existingParent = next
		}
		parent, parentErr := filepath.EvalSymlinks(existingParent)
		if parentErr != nil {
			return "", "", parentErr
		}
		suffix, suffixErr := filepath.Rel(existingParent, candidate)
		if suffixErr != nil {
			return "", "", suffixErr
		}
		resolved = filepath.Join(parent, suffix)
	}

	rel, err := filepath.Rel(f.root, resolved)
	if err != nil {
		return "", "", err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("path %q is outside workspace", path)
	}
	return resolved, filepath.ToSlash(rel), nil
}

func matchesPattern(path string, pattern string) bool {
	if ok, err := filepath.Match(pattern, path); err == nil && ok {
		return true
	}
	base := filepath.Base(path)
	if ok, err := filepath.Match(pattern, base); err == nil && ok {
		return true
	}
	return strings.Contains(path, pattern)
}

type noopGit struct{}

func (noopGit) Status(ctx context.Context) (ports.GitStatus, error) {
	if err := ctx.Err(); err != nil {
		return ports.GitStatus{}, err
	}
	return ports.GitStatus{Clean: true}, nil
}

func (noopGit) Diff(ctx context.Context, paths []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", nil
}

type noopEditor struct{}

func (noopEditor) Open(ctx context.Context, path string, line int) error {
	return ctx.Err()
}
