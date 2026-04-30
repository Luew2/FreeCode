package agent

import (
	"github.com/Luew2/FreeCode/internal/core/model"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/core/session"
)

type ID string

type Role string

const (
	RoleOrchestrator Role = "orchestrator"
	RoleExplorer     Role = "explorer"
	RoleWorker       Role = "worker"
	RoleReviewer     Role = "reviewer"
	RoleVerifier     Role = "verifier"
	RoleSummarizer   Role = "summarizer"
)

type WorkspaceMode string

const (
	WorkspaceSameTree    WorkspaceMode = "same-tree"
	WorkspaceGitWorktree WorkspaceMode = "git-worktree"
	WorkspaceReadOnly    WorkspaceMode = "readonly"
)

type Flow string

const (
	FlowDirect          Flow = "direct"
	FlowArchitectEditor Flow = "architect-editor"
)

type AutonomyMode string

const (
	AutonomyInteractive AutonomyMode = "interactive"
	AutonomySwarm       AutonomyMode = "swarm"
)

type Definition struct {
	Name        string
	Role        Role
	Description string
	Model       model.Ref
	Permissions permission.Policy
	Flow        Flow
	MaxSteps    int
}

type Budget struct {
	MaxSteps   int
	MaxTokens  int
	MaxCostUSD float64
}

func DefaultBudget() Budget {
	return Budget{MaxSteps: 12, MaxTokens: 100000}
}

type HandoffRequirements struct {
	ChangedFiles  bool
	TestsRun      bool
	OpenQuestions bool
}

type Autonomy struct {
	Mode             AutonomyMode
	Approval         permission.Mode
	StopForQuestions bool
}

type Task struct {
	ID           ID
	Goal         string
	Role         Role
	Agent        string
	Workspace    WorkspaceMode
	AllowedPaths []string
	DeniedPaths  []string
	Permissions  permission.Policy
	Autonomy     Autonomy
	Budget       Budget
	Handoff      HandoffRequirements
}

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusBlocked   Status = "blocked"
	StatusFailed    Status = "failed"
)

type Result struct {
	TaskID        ID
	Role          Role
	Status        Status
	Summary       string
	ChangedFiles  []string
	TestsRun      []string
	Findings      []string
	OpenQuestions []string
}

type Trace struct {
	ParentSession session.ID
	TaskSession   session.ID
	Actor         string
}

func DefaultDefinitions() []Definition {
	return []Definition{
		{
			Name:        "orchestrator",
			Role:        RoleOrchestrator,
			Description: "Primary interactive agent that coordinates bounded work.",
			Permissions: permission.DefaultPolicy(),
			Flow:        FlowDirect,
			MaxSteps:    24,
		},
		{
			Name:        "explorer",
			Role:        RoleExplorer,
			Description: "Read-only codebase analysis agent.",
			Permissions: readOnlyPolicy(),
			Flow:        FlowDirect,
			MaxSteps:    12,
		},
		{
			Name:        "worker",
			Role:        RoleWorker,
			Description: "Scoped implementation agent with explicit write grants.",
			Permissions: permission.DefaultPolicy(),
			Flow:        FlowDirect,
			MaxSteps:    12,
		},
		{
			Name:        "reviewer",
			Role:        RoleReviewer,
			Description: "Review-only agent that reports findings before summary.",
			Permissions: readOnlyPolicy(),
			Flow:        FlowDirect,
			MaxSteps:    12,
		},
		{
			Name:        "verifier",
			Role:        RoleVerifier,
			Description: "Runs focused checks with no write permission.",
			Permissions: verifierPolicy(),
			Flow:        FlowDirect,
			MaxSteps:    8,
		},
		{
			Name:        "summarizer",
			Role:        RoleSummarizer,
			Description: "Compacts session state without tool access by default.",
			Permissions: noToolPolicy(),
			Flow:        FlowDirect,
			MaxSteps:    4,
		},
	}
}

func readOnlyPolicy() permission.Policy {
	policy := permission.DefaultPolicy()
	policy.Write = permission.DecisionDeny
	policy.Shell = permission.DecisionDeny
	policy.Network = permission.DecisionDeny
	return policy
}

func verifierPolicy() permission.Policy {
	policy := readOnlyPolicy()
	policy.Shell = permission.DecisionAsk
	return policy
}

func noToolPolicy() permission.Policy {
	return permission.Policy{
		Read:           permission.DecisionDeny,
		Write:          permission.DecisionDeny,
		Shell:          permission.DecisionDeny,
		Network:        permission.DecisionDeny,
		DestructiveGit: permission.DecisionDeny,
	}
}
