package permission

import "testing"

func TestDefaultPolicy(t *testing.T) {
	policy := DefaultPolicy()

	tests := map[Action]Decision{
		ActionRead:           DecisionAllow,
		ActionWrite:          DecisionAsk,
		ActionShell:          DecisionAsk,
		ActionNetwork:        DecisionDeny,
		ActionDestructiveGit: DecisionDeny,
	}

	for action, want := range tests {
		if got := policy.DecisionFor(action); got != want {
			t.Fatalf("DecisionFor(%q) = %q, want %q", action, got, want)
		}
	}
}

func TestPolicyForMode(t *testing.T) {
	tests := []struct {
		name        string
		mode        Mode
		wantWrite   Decision
		wantShell   Decision
		wantNetwork Decision
	}{
		{name: "read only", mode: ModeReadOnly, wantWrite: DecisionDeny, wantShell: DecisionDeny, wantNetwork: DecisionDeny},
		{name: "ask", mode: ModeAsk, wantWrite: DecisionAsk, wantShell: DecisionAsk, wantNetwork: DecisionAsk},
		{name: "auto", mode: ModeAuto, wantWrite: DecisionAllow, wantShell: DecisionAsk, wantNetwork: DecisionAsk},
		{name: "danger", mode: ModeDanger, wantWrite: DecisionAllow, wantShell: DecisionAllow, wantNetwork: DecisionAllow},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := PolicyForMode(test.mode)
			if got := policy.DecisionFor(ActionRead); got != DecisionAllow {
				t.Fatalf("read decision = %q, want allow", got)
			}
			if got := policy.DecisionFor(ActionWrite); got != test.wantWrite {
				t.Fatalf("write decision = %q, want %q", got, test.wantWrite)
			}
			if got := policy.DecisionFor(ActionShell); got != test.wantShell {
				t.Fatalf("shell decision = %q, want %q", got, test.wantShell)
			}
			if got := policy.DecisionFor(ActionNetwork); got != test.wantNetwork {
				t.Fatalf("network decision = %q, want %q", got, test.wantNetwork)
			}
		})
	}
}

func TestModeCycleSkipsDanger(t *testing.T) {
	if got := ModeReadOnly.Cycle(); got != ModeAsk {
		t.Fatalf("read-only cycle = %q, want ask", got)
	}
	if got := ModeAsk.Cycle(); got != ModeAuto {
		t.Fatalf("ask cycle = %q, want auto", got)
	}
	if got := ModeAuto.Cycle(); got != ModeReadOnly {
		t.Fatalf("auto cycle = %q, want read-only", got)
	}
	if got := ModeDanger.Cycle(); got != ModeReadOnly {
		t.Fatalf("danger cycle = %q, want read-only", got)
	}
}

func TestMergePolicyWithMode(t *testing.T) {
	base := DefaultPolicy()
	auto := MergePolicyWithMode(base, ModeAuto)
	if got := auto.DecisionFor(ActionWrite); got != DecisionAllow {
		t.Fatalf("auto write = %q, want allow", got)
	}

	denied := MergePolicyWithMode(Policy{Write: DecisionDeny}, ModeDanger)
	if got := denied.DecisionFor(ActionWrite); got != DecisionDeny {
		t.Fatalf("explicit deny write = %q, want deny", got)
	}

	readOnly := MergePolicyWithMode(Policy{Write: DecisionAllow}, ModeReadOnly)
	if got := readOnly.DecisionFor(ActionWrite); got != DecisionDeny {
		t.Fatalf("read-only write = %q, want deny", got)
	}
}
