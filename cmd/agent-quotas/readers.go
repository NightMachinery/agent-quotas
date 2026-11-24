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

// ---- agy-auth ----

// cmdAgyAuth prints where an account's sign-in is stored (file-present,
// keychain or missing) and exits 0 when it is stored, 1 when it is not. It
// reads only local state, needs no daemon and never opens a credential. The
// shell's agy helpers call it with --dir, the root they resolved themselves,
// so the sign-in rule (agy.StoredAuth) is written only once.
func cmdAgyAuth(ctx context.Context, args []string) int {
	fs := newFlags("agy-auth")
	dir := fs.String("dir", "", "the account's Gemini root, instead of a configured ACCOUNT")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	var a agy.Account
	switch {
	case *dir != "" && len(pos) == 0:
		a = agy.ResolveAccount(config.AgyAccount{GeminiDir: *dir})
	case *dir == "" && len(pos) == 1:
		cfg, err := loadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-quotas: config: %v\n", err)
			return exitFail
		}
		found := false
		for _, c := range cfg.Agy.Accounts {
			if c.Name == pos[0] {
				a, found = agy.ResolveAccount(c), true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "agent-quotas agy-auth: no agy account %q in the config\n", pos[0])
			return exitUsage
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: agent-quotas agy-auth ACCOUNT | --dir GEMINI_ROOT")
		return exitUsage
	}
	kind := agy.StoredAuth(ctx, procx.Exec{}, a)
	fmt.Println(kind)
	if kind == agy.AuthMissing || kind == agy.AuthUnknown {
		return exitFail
	}
	return exitOK
}
