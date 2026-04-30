package ports

import "context"

type Workspace interface {
	Root() string
	FileSystem() FileSystem
	Git() Git
	Editor() Editor
}

type FileSystem interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, contents []byte) error
	RemoveFile(ctx context.Context, path string) error
	RemoveDir(ctx context.Context, path string) error
	ListFiles(ctx context.Context, pattern string) ([]string, error)
}

type Git interface {
	Status(ctx context.Context) (GitStatus, error)
	Diff(ctx context.Context, paths []string) (string, error)
}

type GitStatus struct {
	Branch       string
	Clean        bool
	ChangedFiles []string
}

type Editor interface {
	Open(ctx context.Context, path string, line int) error
}
