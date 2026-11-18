package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/agy"
	"github.com/NightMachinery/agent-quotas/internal/claude"
	"github.com/NightMachinery/agent-quotas/internal/codex"
	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
	"github.com/NightMachinery/agent-quotas/internal/render"
)

// buildReaders returns one reader per provider.
func buildReaders(cfg *config.Config) []model.Reader {
	return []model.Reader{
		claude.NewReader(cfg, procx.Exec{}, &http.Client{}),
		codex.NewReader(cfg, procx.Exec{}),
		agy.NewReader(cfg, procx.Exec{}),
	}
}

func claudeUsableFor(s model.Snapshot, family string) (bool, error) {
	return claude.UsableFor(s, family)
}
func codexUsableFor(s model.Snapshot) (bool, error)         { return codex.UsableFor(s) }
func codexActiveAlias(home string) (string, error)          { return codex.ActiveAlias(home) }
func agyUsableFor(s model.Snapshot, m string) (bool, error) { return agy.UsableFor(s, m) }

// agyEnvelope is the Antigravity section of agent-status.
func agyEnvelope(cfg *config.Config, snaps []model.Snapshot) (json.RawMessage, map[string]string, int) {
	rows := natives(snaps)
	if len(rows) == 0 {
		return nil, nil, 1
	}
	return agy.LegacyJSON(rows), agy.CacheFiles(cfg), agy.ExitCode(rows)
}

// ---- agy-status ----

func cmdAgyStatus(ctx context.Context, args []string) int {
	fs := newFlags("agy-status")
	rf := addReadFlags(fs)
	account := fs.String("account", "", "one account")
	colorMode := fs.String("color", os.Getenv("agy_status_color"), "auto|always|never")
	absolute := fs.String("absolute", os.Getenv("agy_status_absolute_p"), "auto|y|n")
	columns := fs.Int("columns", 0, "terminal width (default: $agy_status_columns, $COLUMNS, 80)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: config: %v\n", err)
		return exitFail
	}
	var selectors []string
	if *account != "" {
		pos = append(pos, *account)
	}
	for _, a := range pos {
		selectors = append(selectors, model.ID(model.Agy, a))
	}
	snaps, code := providerSnapshots(ctx, model.Agy, selectors, *rf.refresh || deus(), *rf.direct)
	if code != exitOK {
		return code
	}
	rows := natives(snaps)
	if *rf.json {
		os.Stdout.Write(agy.LegacyJSON(rows))
		return agy.ExitCode(rows)
	}
	cols := *columns
	if cols <= 0 {
		if _, err := fmt.Sscan(os.Getenv("agy_status_columns"), &cols); err != nil || cols <= 0 {
			cols = terminalColumns(80)
		}
	}
	mode, abs := *colorMode, *absolute
	if mode == "" {
		mode = "auto"
	}
	if abs == "" {
		abs = "auto"
	}
	opts := render.Options{Columns: cols, Color: render.AgyColor(mode, stdoutIsTerminal()), Absolute: abs, Now: time.Now()}
	if err := render.RenderAgy(os.Stdout, rows, agy.CacheFiles(cfg), opts); err != nil {
		fmt.Fprintf(os.Stderr, "agy-status: %v\n", err)
		return exitFail
	}
	return agy.ExitCode(rows)
}
