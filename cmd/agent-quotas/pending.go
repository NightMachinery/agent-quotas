package main

// Temporary stand-ins until the provider readers and the auto-continue
// engines are wired in (replaced by readers.go, compat.go and continue.go).

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/daemon"
	"github.com/NightMachinery/agent-quotas/internal/model"
)

func buildReaders(cfg *config.Config) []model.Reader { return nil }

func attachEngines(ctx context.Context, cfg *config.Config, d *daemon.Daemon, srv *daemon.Server, logger *log.Logger) (func(context.Context), error) {
	return nil, nil
}

func notWired(name string) func(context.Context, []string) int {
	return func(context.Context, []string) int {
		fmt.Fprintf(os.Stderr, "agent-quotas: %s is not wired yet\n", name)
		return exitFail
	}
}

var (
	cmdClaudeCodeUsage = notWired("claude-code-usage")
	cmdCodexStatus     = notWired("codex-status")
	cmdAgyStatus       = notWired("agy-status")
	cmdAgentStatus     = notWired("agent-status")
	cmdContinue        = notWired("continue")
	cmdHook            = notWired("hook")
)
