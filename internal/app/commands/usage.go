package commands

import (
	"fmt"
	"io"
)

func PrintUsage(w io.Writer) error {
	_, err := fmt.Fprintf(w, `%s is a terminal coding workbench.

Usage:
  %s [--plain] [--config PATH] [--session PATH] [--approval read-only|ask|auto|danger]
  %s tui [--plain] [--config PATH] [--session PATH] [--approval read-only|ask|auto|danger]
  %s version
  %s doctor
  %s bench [--task all|TASK]
  %s compact [--session PATH] [--session-id ID] [--max-tokens N]
  %s status
  %s diff [PATH ...]
  %s ask [--config PATH] [--session PATH] [--max-steps N] [--max-input-tokens N] [--max-output-tokens N] [--approval read-only|ask|auto|danger] [--allow-writes] "question"
  %s swarm [--config PATH] [--session PATH] [--max-steps N] [--max-input-tokens N] [--max-output-tokens N] [--approval read-only|ask|auto|danger] "task"
  %s provider add --name NAME --base-url URL --api-key-env ENV --model MODEL --protocol openai-chat|anthropic-messages|auto [--context-window N] [--max-output-tokens N] [--config PATH] [--skip-probe]
  %s provider list [--config PATH]
`, AppName, AppName, AppName, AppName, AppName, AppName, AppName, AppName, AppName, AppName, AppName, AppName, AppName)
	return err
}
