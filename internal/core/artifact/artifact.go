package artifact

import (
	"fmt"
	"time"
)

type Kind string

const (
	KindMessage    Kind = "message"
	KindCodeBlock  Kind = "code_block"
	KindFile       Kind = "file"
	KindPatch      Kind = "patch"
	KindToolResult Kind = "tool_result"
	KindAgent      Kind = "agent"
)

type ID struct {
	Kind   Kind
	Number int
}

func NewID(kind Kind, number int) ID {
	return ID{Kind: kind, Number: number}
}

func (id ID) String() string {
	prefix := Prefix(id.Kind)
	if prefix == "" || id.Number <= 0 {
		return string(id.Kind)
	}
	return fmt.Sprintf("%s%d", prefix, id.Number)
}

func Prefix(kind Kind) string {
	switch kind {
	case KindMessage:
		return "m"
	case KindCodeBlock:
		return "c"
	case KindFile:
		return "f"
	case KindPatch:
		return "p"
	case KindToolResult:
		return "r"
	case KindAgent:
		return "a"
	default:
		return ""
	}
}

type Artifact struct {
	ID        ID
	Kind      Kind
	Title     string
	Body      string
	MIMEType  string
	URI       string
	CreatedAt time.Time
	Metadata  map[string]string
}

type Patch struct {
	ID      ID
	Summary string
	Diff    string
	Files   []string
}
