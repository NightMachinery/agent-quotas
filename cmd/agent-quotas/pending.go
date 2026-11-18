package main

// Temporary stand-ins until the remaining readers and the native engine are
// wired in.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/NightMachinery/agent-quotas/internal/claude"
	"github.com/NightMachinery/agent-quotas/internal/codex"
	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/daemon"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
	"net/http"
)

func buildReaders(cfg *config.Config) []model.Reader {
	return []model.Reader{claude.NewReader(cfg, procx.Exec{}, &http.Client{}), codex.NewReader(cfg, procx.Exec{})}
}

func claudeUsableFor(s model.Snapshot, family string) (bool, error) {
	return claude.UsableFor(s, family)
}
func codexUsableFor(s model.Snapshot) (bool, error) { return codex.UsableFor(s) }
func codexActiveAlias(home string) (string, error)  { return codex.ActiveAlias(home) }
func agyUsableFor(s model.Snapshot, m string) (bool, error) {
	return false, errors.New("agy not wired")
}

type nativeScheduler struct{}

func (*nativeScheduler) run(context.Context)         {}
func (*nativeScheduler) transition(model.Transition) {}
func (*nativeScheduler) request(string)              {}

type nativeEngine struct{}

func attachNative(cfg *config.Config, d *daemon.Daemon, q quotaAdapter, l *log.Logger) (*nativeScheduler, *nativeEngine) {
	return nil, nil
}
func nativeListing(*nativeEngine) any                     { return nil }
func attachNativeRoutes(*daemon.Server, *nativeScheduler) {}
func nativeCommand(ctx context.Context, cfg *config.Config, action string, o nativeOptions) int {
	fmt.Fprintln(os.Stderr, "native auto-continue not wired yet")
	return exitFail
}
func nativeList(ctx context.Context, cfg *config.Config, prune bool) int { return exitOK }

func notWired(name string) func(context.Context, []string) int {
	return func(context.Context, []string) int {
		fmt.Fprintf(os.Stderr, "agent-quotas: %s is not wired yet\n", name)
		return exitFail
	}
}

var (
	cmdAgyStatus   = notWired("agy-status")
	cmdAgentStatus = notWired("agent-status")
	cmdHook        = notWired("hook")
)
