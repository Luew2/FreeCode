package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	tomlconfig "github.com/Luew2/FreeCode/internal/adapters/config/toml"
	envsecrets "github.com/Luew2/FreeCode/internal/adapters/secrets/env"
	jsonllog "github.com/Luew2/FreeCode/internal/adapters/session/jsonl"
	"github.com/Luew2/FreeCode/internal/adapters/tui"
	"github.com/Luew2/FreeCode/internal/adapters/tui2"
	"github.com/Luew2/FreeCode/internal/adapters/workspace/gitcli"
	"github.com/Luew2/FreeCode/internal/adapters/workspace/localfs"
	"github.com/Luew2/FreeCode/internal/app/bench"
	"github.com/Luew2/FreeCode/internal/app/bootstrap"
	"github.com/Luew2/FreeCode/internal/app/commands"
	"github.com/Luew2/FreeCode/internal/app/swarm"
	"github.com/Luew2/FreeCode/internal/core/permission"
	"github.com/Luew2/FreeCode/internal/ports"
	"golang.org/x/term"
)

func Run(args []string, stdout io.Writer, stderr io.Writer) int {
	return RunWithIO(args, strings.NewReader(""), stdout, stderr)
}

func RunWithIO(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		return runTUI(nil, stdin, stdout, stderr)
	}
	if strings.HasPrefix(args[0], "-") && args[0] != "-h" && args[0] != "--help" {
		return runTUI(args, stdin, stdout, stderr)
	}

	switch args[0] {
	case "help", "-h", "--help":
		_ = commands.PrintUsage(stdout)
		return 0
	case "version":
		if !hasExactArity(args, 1, stderr) {
			return 2
		}
		if err := commands.PrintVersion(stdout, commands.DefaultVersionInfo()); err != nil {
			fmt.Fprintf(stderr, "version: %v\n", err)
			return 1
		}
		return 0
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "debug-bundle":
		return runDebugBundle(args[1:], stdout, stderr)
	case "bench":
		return runBench(args[1:], stdout, stderr)
	case "compact":
		return runCompact(args[1:], stdout, stderr)
	case "tui":
		return runTUI(args[1:], stdin, stdout, stderr)
	case "ask":
		return runAsk(args[1:], stdout, stderr)
	case "swarm":
		return runSwarm(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "diff":
		return runDiff(args[1:], stdout, stderr)
	case "provider":
		return runProvider(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		_ = commands.PrintUsage(stderr)
		return 2
	}
}

func runDoctor(args []string, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "doctor accepts flags only\n")
		return 2
	}
	status, err := buildDoctorStatus(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "doctor: %v\n", err)
		return 1
	}
	if err := commands.PrintDoctor(stdout, status); err != nil {
		fmt.Fprintf(stderr, "doctor: %v\n", err)
		return 1
	}
	return 0
}

func runDebugBundle(args []string, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	var sessionPath string
	var sessionID string
	var outPath string
	fs := flag.NewFlagSet("debug-bundle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	fs.StringVar(&sessionPath, "session", filepath.Join(".freecode", "sessions", "latest.jsonl"), "session JSONL path")
	fs.StringVar(&sessionID, "session-id", "default", "session id")
	fs.StringVar(&outPath, "out", "", "write bundle to file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "debug-bundle accepts flags only\n")
		return 2
	}
	cwd, _ := os.Getwd()
	var writer io.Writer = stdout
	var file *os.File
	if outPath != "" {
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil && filepath.Dir(outPath) != "." {
			fmt.Fprintf(stderr, "debug-bundle: %v\n", err)
			return 1
		}
		var err error
		file, err = os.Create(outPath)
		if err != nil {
			fmt.Fprintf(stderr, "debug-bundle: %v\n", err)
			return 1
		}
		defer file.Close()
		writer = file
	}
	if err := commands.WriteDebugBundle(context.Background(), writer, commands.DebugBundleOptions{
		WorkDir:     cwd,
		ConfigPath:  configPath,
		SessionPath: sessionPath,
		SessionID:   sessionID,
	}); err != nil {
		fmt.Fprintf(stderr, "debug-bundle: %v\n", err)
		return 1
	}
	if outPath != "" {
		fmt.Fprintf(stdout, "debug bundle written to %s\n", outPath)
	}
	return 0
}

func runCompact(args []string, stdout io.Writer, stderr io.Writer) int {
	var sessionPath string
	var sessionID string
	var maxTokens int
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&sessionPath, "session", filepath.Join(".freecode", "sessions", "latest.jsonl"), "session JSONL path")
	fs.StringVar(&sessionID, "session-id", "default", "session id")
	fs.IntVar(&maxTokens, "max-tokens", 4096, "maximum estimated tokens in compacted summary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "compact accepts flags only\n")
		return 2
	}
	if err := commands.Compact(context.Background(), stdout, jsonllog.New(sessionPath), commands.CompactOptions{
		SessionID: sessionID,
		MaxTokens: maxTokens,
	}); err != nil {
		fmt.Fprintf(stderr, "compact: %v\n", err)
		return 1
	}
	return 0
}

func runBench(args []string, stdout io.Writer, stderr io.Writer) int {
	var task string
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "usage: freecode bench [--task all|TASK]\n\n")
		_, _ = fmt.Fprintf(fs.Output(), "Tasks: all, %s\n", strings.Join(benchTaskNames(), ", "))
	}
	fs.StringVar(&task, "task", "all", "benchmark task name or all")
	if hasHelpArg(args) {
		fs.SetOutput(stdout)
		fs.Usage()
		return 0
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "bench accepts flags only\n")
		return 2
	}

	results, err := bench.Run(context.Background(), bench.Options{Task: task, Tasks: benchTasks()})
	if err != nil {
		fmt.Fprintf(stderr, "bench: %v\n", err)
		return 2
	}
	if err := bench.FormatResults(stdout, results); err != nil {
		fmt.Fprintf(stderr, "bench: %v\n", err)
		return 1
	}
	if !bench.AllPassed(results) {
		return 1
	}
	return 0
}

func runAsk(args []string, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	var sessionPath string
	var maxSteps int
	var maxInputTokens int
	var maxOutputTokens int
	var allowWrites bool
	var approvalValue string
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	fs.StringVar(&sessionPath, "session", filepath.Join(".freecode", "sessions", "latest.jsonl"), "session JSONL path")
	fs.IntVar(&maxSteps, "max-steps", 0, "maximum model/tool loop steps")
	fs.IntVar(&maxInputTokens, "max-input-tokens", 0, "maximum estimated input tokens before compaction")
	fs.IntVar(&maxOutputTokens, "max-output-tokens", 0, "maximum output tokens requested from the model")
	fs.StringVar(&approvalValue, "approval", string(permission.ModeReadOnly), "approval mode: read-only, ask, auto, or danger")
	fs.BoolVar(&allowWrites, "allow-writes", false, "enable workspace write tools for this ask")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		fmt.Fprintf(stderr, "ask requires a question\n")
		return 2
	}

	ctx := context.Background()
	approvalMode, err := permission.ParseMode(approvalValue)
	if err != nil {
		fmt.Fprintf(stderr, "ask: %v\n", err)
		return 2
	}
	if allowWrites && !hasFlag(args, "approval") {
		approvalMode = permission.ModeAuto
	}
	rt, err := bootstrap.Build(ctx, bootstrap.Options{
		ConfigPath:      configPath,
		SessionPath:     sessionPath,
		ApprovalMode:    approvalMode,
		StartNewSession: false,
	})
	if err != nil {
		fmt.Fprintf(stderr, "ask: %v\n", err)
		return 1
	}
	defer rt.Close()
	deps, err := rt.AskDependencies(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "ask: %v\n", err)
		return 1
	}

	err = commands.Ask(ctx, stdout, deps, commands.AskOptions{
		Question:        question,
		MaxSteps:        maxSteps,
		MaxInputTokens:  maxInputTokens,
		MaxOutputTokens: maxOutputTokens,
	})
	if err != nil {
		fmt.Fprintf(stderr, "ask: %v\n", err)
		return 1
	}
	return 0
}

func runTUI(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	var sessionPath string
	var approvalValue string
	var plain bool
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	fs.StringVar(&sessionPath, "session", filepath.Join(".freecode", "sessions", "latest.jsonl"), "session JSONL path")
	fs.StringVar(&approvalValue, "approval", string(permission.ModeAsk), "approval mode: read-only, ask, auto, or danger")
	fs.BoolVar(&plain, "plain", false, "use the line-based fallback UI")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "tui accepts flags only\n")
		return 2
	}

	ctx := context.Background()
	approvalMode, err := permission.ParseMode(approvalValue)
	if err != nil {
		fmt.Fprintf(stderr, "tui: %v\n", err)
		return 2
	}
	rt, err := bootstrap.Build(ctx, bootstrap.Options{
		ConfigPath:        configPath,
		SessionPath:       sessionPath,
		ApprovalMode:      approvalMode,
		StartNewSession:   !hasFlag(args, "session"),
		ClipboardTerminal: stdout,
	})
	if err != nil {
		fmt.Fprintf(stderr, "tui: %v\n", err)
		return 1
	}
	defer rt.Close()
	if err := rt.RegisterLaunchSession(ctx); err != nil {
		fmt.Fprintf(stderr, "tui: %v\n", err)
		return 1
	}
	workbenchService, err := rt.Workbench(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "tui: %v\n", err)
		return 1
	}

	if plain || !isTerminal(stdout) {
		err = tui.Run(ctx, tui.Options{
			In:        stdin,
			Out:       stdout,
			Workbench: workbenchService,
		})
	} else {
		err = tui2.Run(ctx, tui2.Options{
			In:        stdin,
			Out:       stdout,
			Workbench: workbenchService,
			AltScreen: true,
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "tui: %v\n", err)
		return 1
	}
	return 0
}

func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

func runSwarm(args []string, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	var sessionPath string
	var approvalValue string
	var maxSteps int
	var maxInputTokens int
	var maxOutputTokens int
	fs := flag.NewFlagSet("swarm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	fs.StringVar(&sessionPath, "session", filepath.Join(".freecode", "sessions", "latest.jsonl"), "session JSONL path")
	fs.StringVar(&approvalValue, "approval", string(permission.ModeAsk), "approval mode: read-only, ask, auto, or danger")
	fs.IntVar(&maxSteps, "max-steps", 0, "maximum steps per swarm agent")
	fs.IntVar(&maxInputTokens, "max-input-tokens", 0, "maximum estimated input tokens before compaction")
	fs.IntVar(&maxOutputTokens, "max-output-tokens", 0, "maximum output tokens requested from the model")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" {
		fmt.Fprintf(stderr, "swarm requires a task\n")
		return 2
	}

	ctx := context.Background()
	approvalMode, err := permission.ParseMode(approvalValue)
	if err != nil {
		fmt.Fprintf(stderr, "swarm: %v\n", err)
		return 2
	}
	rt, err := bootstrap.Build(ctx, bootstrap.Options{
		ConfigPath:      configPath,
		SessionPath:     sessionPath,
		ApprovalMode:    approvalMode,
		StartNewSession: !hasFlag(args, "session"),
	})
	if err != nil {
		fmt.Fprintf(stderr, "swarm: %v\n", err)
		return 1
	}
	defer rt.Close()
	if err := rt.RegisterLaunchSession(ctx); err != nil {
		fmt.Fprintf(stderr, "swarm: %v\n", err)
		return 1
	}
	if maxInputTokens > 0 {
		rt.ContextBudget.MaxInputTokens = maxInputTokens
	}
	if maxOutputTokens > 0 {
		rt.ContextBudget.MaxOutputTokens = maxOutputTokens
	}
	useCase, err := rt.SwarmUseCase(ctx, rt.EventLog, rt.SessionID)
	if err != nil {
		fmt.Fprintf(stderr, "swarm: %v\n", err)
		return 1
	}
	response, err := useCase.Run(ctx, swarm.Request{
		SessionID: rt.SessionID,
		Goal:      goal,
		Approval:  approvalMode,
		MaxSteps:  maxSteps,
	})
	if err != nil {
		fmt.Fprintf(stderr, "swarm: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "swarm %s\n", response.Status)
	for _, result := range response.Results {
		fmt.Fprintf(stdout, "- %s %s: %s\n", result.Role, result.Status, strings.TrimSpace(result.Summary))
	}
	return 0
}

func runStatus(args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "status accepts no arguments\n")
		return 2
	}
	git, err := gitForCWD()
	if err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}
	if err := commands.PrintStatus(context.Background(), stdout, git); err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}
	return 0
}

func runDiff(args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	git, err := gitForCWD()
	if err != nil {
		fmt.Fprintf(stderr, "diff: %v\n", err)
		return 1
	}
	if err := commands.PrintDiff(context.Background(), stdout, git, fs.Args()); err != nil {
		fmt.Fprintf(stderr, "diff: %v\n", err)
		return 1
	}
	return 0
}

func gitForCWD() (ports.Git, error) {
	workspace, err := localfs.New(".")
	if err != nil {
		return nil, err
	}
	return gitcli.New(workspace.Root())
}

func hasExactArity(args []string, want int, stderr io.Writer) bool {
	if len(args) == want {
		return true
	}
	fmt.Fprintf(stderr, "%s accepts no extra arguments\n", args[0])
	return false
}

func hasFlag(args []string, name string) bool {
	long := "--" + name
	for _, arg := range args {
		if arg == long || strings.HasPrefix(arg, long+"=") {
			return true
		}
	}
	return false
}

func hasHelpArg(args []string) bool {
	for _, arg := range args {
		if arg == "help" || arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func runProvider(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		_ = printProviderUsage(stderr)
		return 2
	}

	switch args[0] {
	case "add":
		return runProviderAdd(args[1:], stdout, stderr)
	case "list":
		return runProviderList(args[1:], stdout, stderr)
	case "use":
		return runProviderUse(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_ = printProviderUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown provider command %q\n\n", args[0])
		_ = printProviderUsage(stderr)
		return 2
	}
}

func runProviderUse(args []string, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	fs := flag.NewFlagSet("provider use", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: provider use <name|provider/model>\n")
		return 2
	}
	store := tomlconfig.New(configPath)
	if err := commands.UseProvider(context.Background(), stdout, store, fs.Arg(0)); err != nil {
		fmt.Fprintf(stderr, "provider use: %v\n", err)
		return 1
	}
	return 0
}

func runProviderAdd(args []string, stdout io.Writer, stderr io.Writer) int {
	var opts commands.ProviderAddOptions
	var configPath string
	fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.Name, "name", "", "provider name")
	fs.StringVar(&opts.BaseURL, "base-url", "", "provider base URL")
	fs.StringVar(&opts.APIKeyEnv, "api-key-env", "", "environment variable containing the API key")
	fs.StringVar(&opts.Model, "model", "", "provider model id")
	fs.StringVar(&opts.Protocol, "protocol", commands.ProviderProtocolAuto, "provider protocol: openai-chat, anthropic-messages, or auto")
	fs.IntVar(&opts.ContextWindow, "context-window", 0, "model context window in tokens")
	fs.IntVar(&opts.MaxOutputTokens, "max-output-tokens", 0, "maximum output tokens for model requests")
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	fs.BoolVar(&opts.SkipProbe, "skip-probe", false, "skip protocol probe")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "provider add accepts flags only\n")
		return 2
	}

	store := tomlconfig.New(configPath)
	secrets := envsecrets.New()
	probe := newProtocolProbeChain(secrets)
	if err := commands.AddProvider(context.Background(), stdout, store, probe, opts); err != nil {
		fmt.Fprintf(stderr, "provider add: %v\n", err)
		return 1
	}
	return 0
}

func runProviderList(args []string, stdout io.Writer, stderr io.Writer) int {
	var configPath string
	fs := flag.NewFlagSet("provider list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&configPath, "config", tomlconfig.DefaultPath, "config path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "provider list accepts flags only\n")
		return 2
	}

	if err := commands.ListProviders(context.Background(), stdout, tomlconfig.New(configPath)); err != nil {
		fmt.Fprintf(stderr, "provider list: %v\n", err)
		return 1
	}
	return 0
}

func printProviderUsage(w io.Writer) error {
	_, err := fmt.Fprintf(w, `Usage:
  %s provider add --name NAME --base-url URL --api-key-env ENV --model MODEL --protocol openai-chat|anthropic-messages|auto [--context-window N] [--max-output-tokens N] [--config PATH] [--skip-probe]
  %s provider list [--config PATH]
  %s provider use NAME|PROVIDER/MODEL [--config PATH]
`, commands.AppName, commands.AppName, commands.AppName)
	return err
}

func buildDoctorStatus(configPath string) (commands.DoctorStatus, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return commands.DoctorStatus{}, err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return commands.DoctorStatus{}, err
	}

	_, goModOK := findUp(absCWD, "go.mod")
	gitPath, gitOK := findUp(absCWD, ".git")
	configStore := tomlconfig.New(configPath)
	settings, configErr := configStore.Load(context.Background())
	activeModel := settings.ActiveModel.String()
	activeProvider, providerOK := settings.Providers[settings.ActiveModel.Provider]
	apiKeyOK := false
	if providerOK && activeProvider.Secret.Name != "" {
		apiKeyOK = os.Getenv(activeProvider.Secret.Name) != ""
	}
	editor := settings.EditorCommand
	if strings.TrimSpace(editor) == "" {
		editor = "nvim"
	}
	editorCommand := "nvim"
	if fields := strings.Fields(editor); len(fields) > 0 {
		editorCommand = fields[0]
	}
	_, editorErr := exec.LookPath(editorCommand)

	checks := []commands.DoctorCheck{
		{
			Name:   "go.mod",
			OK:     goModOK,
			Detail: foundDetail(goModOK, "found", "not found"),
		},
		{
			Name:   "git",
			OK:     gitOK,
			Detail: foundDetail(gitOK, gitPath, "not found"),
		},
		{
			Name:   "config",
			OK:     configErr == nil,
			Detail: doctorErrDetail(configErr, configPath),
		},
		{
			Name:   "active provider",
			OK:     providerOK && activeModel != "",
			Detail: foundDetail(providerOK && activeModel != "", activeModel, "not configured"),
		},
		{
			Name:   "api key",
			OK:     apiKeyOK || (providerOK && activeProvider.Secret.Name == ""),
			Detail: apiKeyDetail(providerOK, activeProvider.Secret.Name, apiKeyOK),
		},
		{
			Name:   "editor",
			OK:     editorErr == nil,
			Detail: doctorErrDetail(editorErr, editor),
		},
		{
			Name:   "terminal",
			OK:     term.IsTerminal(int(os.Stdout.Fd())) || term.IsTerminal(int(os.Stdin.Fd())),
			Detail: foundDetail(term.IsTerminal(int(os.Stdout.Fd())) || term.IsTerminal(int(os.Stdin.Fd())), "tty available", "not running on a tty"),
		},
	}
	if settings.MCP.Enabled {
		for name, server := range settings.MCP.Servers {
			if !server.Enabled {
				continue
			}
			_, err := exec.LookPath(server.Command)
			detail := "stdio " + server.Command
			if err != nil {
				detail = err.Error()
			}
			checks = append(checks, commands.DoctorCheck{
				Name:   "mcp " + name,
				OK:     err == nil,
				Detail: detail,
			})
		}
	} else {
		checks = append(checks, commands.DoctorCheck{Name: "mcp", OK: true, Detail: "disabled"})
	}

	return commands.DoctorStatus{
		Version:     commands.DefaultVersionInfo(),
		WorkDir:     absCWD,
		ConfigPath:  configPath,
		ActiveModel: activeModel,
		Approval:    string(settings.Permissions.Write),
		Runtime: commands.RuntimeStatus{
			GoVersion: runtime.Version(),
			GOOS:      runtime.GOOS,
			GOARCH:    runtime.GOARCH,
		},
		Checks: checks,
	}, nil
}

func doctorErrDetail(err error, ok string) string {
	if err == nil {
		return ok
	}
	return err.Error()
}

func apiKeyDetail(providerOK bool, envName string, ok bool) string {
	if !providerOK {
		return "active provider is not configured"
	}
	if envName == "" {
		return "provider does not require an API key"
	}
	if ok {
		return envName + " is set"
	}
	return envName + " is not set"
}

func findUp(start string, name string) (string, bool) {
	dir := filepath.Clean(start)
	for {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func foundDetail(ok bool, yes string, no string) string {
	if ok {
		return yes
	}
	return no
}
