package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Luew2/FreeCode/internal/app/workbench"
	"github.com/Luew2/FreeCode/internal/core/permission"
)

func TestWorkbenchRendersAndDispatchesCopy(t *testing.T) {
	controller := &fakeController{
		state: workbench.State{
			Model:    "local/coder",
			Branch:   "main",
			Approval: permission.ModeAsk,
			Mode:     "NORMAL",
			Agents:   []workbench.AgentItem{{ID: "a1", Name: "worker", Status: "completed"}},
			Artifacts: []workbench.Item{{
				ID:    "c1",
				Kind:  "code",
				Title: "go code block 1",
				Body:  "fmt.Println(\"hi\")",
				Meta:  map[string]string{"fenced": "```go\nfmt.Println(\"hi\")\n```"},
			}},
		},
	}
	var out bytes.Buffer

	err := Run(context.Background(), Options{
		In:        strings.NewReader("Y c1\nq\n"),
		Out:       &out,
		Workbench: controller,
		Width:     90,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(out.String(), "c1   code") {
		t.Fatalf("output = %q, want code artifact", out.String())
	}
	if !strings.Contains(out.String(), "a1 worker completed") {
		t.Fatalf("output = %q, want agent status", out.String())
	}
	if controller.copiedID != "c1" || !controller.copiedFenced {
		t.Fatalf("copy = %q fenced=%v, want c1 fenced", controller.copiedID, controller.copiedFenced)
	}
}

func TestWorkbenchApprovalModeIsPassedToSubmit(t *testing.T) {
	controller := &fakeController{state: workbench.State{Approval: permission.ModeReadOnly, Mode: "NORMAL"}}
	var out bytes.Buffer

	err := Run(context.Background(), Options{
		In:        strings.NewReader("ctrl-a\nctrl-a\ni patch README\nq\n"),
		Out:       &out,
		Workbench: controller,
		Width:     90,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if controller.submitted.Text != "patch README" {
		t.Fatalf("submitted text = %q, want prompt", controller.submitted.Text)
	}
	if controller.submitted.Approval != permission.ModeAuto {
		t.Fatalf("approval = %q, want auto", controller.submitted.Approval)
	}
	if controller.submitted.Swarm {
		t.Fatalf("swarm = true, want false")
	}
	if !strings.Contains(out.String(), "approval:auto") {
		t.Fatalf("output = %q, want visible auto approval", out.String())
	}
}

func TestWorkbenchAcceptsColonSendAgentAndOpenAliases(t *testing.T) {
	controller := &fakeController{state: workbench.State{Approval: permission.ModeAsk, Mode: "NORMAL"}}
	var out bytes.Buffer

	err := Run(context.Background(), Options{
		In:        strings.NewReader(":i hello\n:agent implement thing\n:s ship it\n:! echo ok\n:o m1\nq\n"),
		Out:       &out,
		Workbench: controller,
		Width:     90,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if controller.submissions[0].Text != "hello" || controller.submissions[0].Swarm {
		t.Fatalf("first submission = %#v, want normal prompt", controller.submissions[0])
	}
	if controller.submissions[1].Text != "implement thing" || controller.submissions[1].Swarm {
		t.Fatalf("second submission = %#v, want directed non-swarm prompt", controller.submissions[1])
	}
	if controller.submissions[2].Text != "ship it" || !controller.submissions[2].Swarm {
		t.Fatalf("third submission = %#v, want swarm prompt", controller.submissions[2])
	}
	if controller.shellCommand != "echo ok" {
		t.Fatalf("shell command = %q, want echo ok", controller.shellCommand)
	}
	if controller.opened != "m1" {
		t.Fatalf("opened = %q, want m1", controller.opened)
	}
}

func TestWorkbenchDispatchesOpenAndApprove(t *testing.T) {
	controller := &fakeController{state: workbench.State{
		Approval: permission.ModeAsk,
		Mode:     "NORMAL",
		Artifacts: []workbench.Item{{
			ID:    "p1",
			Kind:  "patch",
			Title: "preview",
			Meta:  map[string]string{"changed_files": "README.md"},
		}},
	}}
	var out bytes.Buffer

	err := Run(context.Background(), Options{
		In:        strings.NewReader("o f1:42\na p1\nq\n"),
		Out:       &out,
		Workbench: controller,
		Width:     90,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if controller.opened != "f1:42" {
		t.Fatalf("opened = %q, want f1:42", controller.opened)
	}
	if controller.approved != "p1" {
		t.Fatalf("approved = %q, want p1", controller.approved)
	}
}

func TestWorkbenchCompactsAndRequiresDangerConfirmation(t *testing.T) {
	controller := &fakeController{state: workbench.State{Approval: permission.ModeAsk, Mode: "NORMAL"}}
	var out bytes.Buffer

	err := Run(context.Background(), Options{
		In:        strings.NewReader(":approval danger\n:danger\n:compact\n:danger confirm\nq\n"),
		Out:       &out,
		Workbench: controller,
		Width:     90,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !controller.compacted {
		t.Fatalf("compacted = false, want true")
	}
	if controller.state.Approval != permission.ModeDanger {
		t.Fatalf("approval = %q, want danger after confirmation", controller.state.Approval)
	}
	output := out.String()
	if !strings.Contains(output, "type :danger confirm") || !strings.Contains(output, "approval:danger") {
		t.Fatalf("output = %q, want danger prompt and final danger state", output)
	}
}

type fakeController struct {
	state        workbench.State
	submitted    workbench.SubmitRequest
	submissions  []workbench.SubmitRequest
	copiedID     string
	copiedFenced bool
	opened       string
	approved     string
	compacted    bool
	shellCommand string
}

func (c *fakeController) Load(ctx context.Context) (workbench.State, error) {
	return c.state, nil
}

func (c *fakeController) SubmitPrompt(ctx context.Context, request workbench.SubmitRequest) (workbench.State, error) {
	c.submitted = request
	c.submissions = append(c.submissions, request)
	c.state.Notice = "prompt sent"
	c.state.Approval = request.Approval
	return c.state, nil
}

func (c *fakeController) Copy(ctx context.Context, id string, withFences bool) (workbench.State, error) {
	c.copiedID = id
	c.copiedFenced = withFences
	c.state.Notice = "copied " + id
	return c.state, nil
}

func (c *fakeController) Open(ctx context.Context, ref string) (workbench.State, error) {
	c.opened = ref
	c.state.Notice = "opened " + ref
	return c.state, nil
}

func (c *fakeController) Detail(ctx context.Context, id string) (workbench.State, error) {
	c.state.Detail = workbench.Item{ID: id, Title: "detail"}
	return c.state, nil
}

func (c *fakeController) Approve(ctx context.Context, id string) (workbench.State, error) {
	c.approved = id
	c.state.Notice = "approved " + id
	return c.state, nil
}

func (c *fakeController) Reject(ctx context.Context, id string) (workbench.State, error) {
	c.state.Notice = "rejected " + id
	return c.state, nil
}

func (c *fakeController) SetApproval(ctx context.Context, mode permission.Mode) (workbench.State, error) {
	c.state.Approval = mode
	c.state.Notice = "approval: " + string(mode)
	return c.state, nil
}

func (c *fakeController) Compact(ctx context.Context) (workbench.State, error) {
	c.compacted = true
	c.state.Notice = "compacted context"
	return c.state, nil
}

func (c *fakeController) Palette(ctx context.Context) (workbench.State, error) {
	c.state.Detail = workbench.Item{ID: "commands", Title: "Command Palette"}
	return c.state, nil
}

func (c *fakeController) RunShell(ctx context.Context, command string) (workbench.State, error) {
	c.shellCommand = command
	c.state.Detail = workbench.Item{ID: "sh1", Kind: "shell", Title: "! " + command, Body: "ok"}
	return c.state, nil
}
