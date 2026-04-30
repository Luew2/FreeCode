package permission

import "fmt"

// Action is a capability FreeCode may need to perform while helping a user.
type Action string

const (
	ActionRead           Action = "read"
	ActionWrite          Action = "write"
	ActionShell          Action = "shell"
	ActionNetwork        Action = "network"
	ActionDestructiveGit Action = "destructive_git"
)

// Decision describes how a permission request should be handled.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionAsk   Decision = "ask"
	DecisionDeny  Decision = "deny"
)

// Policy is protocol-neutral and adapter-neutral. It describes permission
// posture, not how an approval is collected or enforced.
type Policy struct {
	Read           Decision
	Write          Decision
	Shell          Decision
	Network        Decision
	DestructiveGit Decision
	AllowedPaths   []string
	DeniedPaths    []string
}

type Mode string

const (
	ModeReadOnly Mode = "read-only"
	ModeAsk      Mode = "ask"
	ModeAuto     Mode = "auto"
	ModeDanger   Mode = "danger"
)

// Request is passed to a PermissionGate before a tool or agent performs work.
type Request struct {
	Action  Action
	Subject string
	Reason  string
}

func ParseMode(value string) (Mode, error) {
	switch Mode(value) {
	case "", ModeReadOnly:
		return ModeReadOnly, nil
	case ModeAsk:
		return ModeAsk, nil
	case ModeAuto:
		return ModeAuto, nil
	case ModeDanger:
		return ModeDanger, nil
	default:
		return "", fmt.Errorf("unknown approval mode %q", value)
	}
}

func DefaultPolicy() Policy {
	return Policy{
		Read:           DecisionAllow,
		Write:          DecisionAsk,
		Shell:          DecisionAsk,
		Network:        DecisionDeny,
		DestructiveGit: DecisionDeny,
	}
}

func PolicyForMode(mode Mode) Policy {
	switch mode {
	case ModeReadOnly:
		return Policy{
			Read:           DecisionAllow,
			Write:          DecisionDeny,
			Shell:          DecisionDeny,
			Network:        DecisionDeny,
			DestructiveGit: DecisionDeny,
		}
	case ModeAsk:
		return Policy{
			Read:           DecisionAllow,
			Write:          DecisionAsk,
			Shell:          DecisionAsk,
			Network:        DecisionAsk,
			DestructiveGit: DecisionDeny,
		}
	case ModeAuto:
		return Policy{
			Read:           DecisionAllow,
			Write:          DecisionAllow,
			Shell:          DecisionAsk,
			Network:        DecisionAsk,
			DestructiveGit: DecisionDeny,
		}
	case ModeDanger:
		return Policy{
			Read:           DecisionAllow,
			Write:          DecisionAllow,
			Shell:          DecisionAllow,
			Network:        DecisionAllow,
			DestructiveGit: DecisionAllow,
		}
	default:
		return DefaultPolicy()
	}
}

func MergePolicyWithMode(base Policy, mode Mode) Policy {
	modePolicy := PolicyForMode(mode)
	base.Read = mergeDecision(base.Read, modePolicy.Read)
	base.Write = mergeDecision(base.Write, modePolicy.Write)
	base.Shell = mergeDecision(base.Shell, modePolicy.Shell)
	base.Network = mergeDecision(base.Network, modePolicy.Network)
	base.DestructiveGit = mergeDecision(base.DestructiveGit, modePolicy.DestructiveGit)
	return base
}

func mergeDecision(base Decision, mode Decision) Decision {
	if base == DecisionDeny || mode == DecisionDeny {
		return DecisionDeny
	}
	if mode == DecisionAllow {
		return DecisionAllow
	}
	if base == "" {
		return mode
	}
	return base
}

func (m Mode) Cycle() Mode {
	switch m {
	case ModeReadOnly:
		return ModeAsk
	case ModeAsk:
		return ModeAuto
	default:
		return ModeReadOnly
	}
}

func (p Policy) DecisionFor(action Action) Decision {
	switch action {
	case ActionRead:
		return fallbackDecision(p.Read, DecisionAllow)
	case ActionWrite:
		return fallbackDecision(p.Write, DecisionAsk)
	case ActionShell:
		return fallbackDecision(p.Shell, DecisionAsk)
	case ActionNetwork:
		return fallbackDecision(p.Network, DecisionDeny)
	case ActionDestructiveGit:
		return fallbackDecision(p.DestructiveGit, DecisionDeny)
	default:
		return DecisionAsk
	}
}

func fallbackDecision(value Decision, fallback Decision) Decision {
	if value == "" {
		return fallback
	}
	return value
}
