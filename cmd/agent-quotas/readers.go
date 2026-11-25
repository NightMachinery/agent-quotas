package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
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
	flat := fs.Bool("flat", false, "print one account's flat bucket array (the account: --account, else $AGY_ACCOUNT_ID, else m)")
	cacheOnly := fs.Bool("cache-only", false, "read the statusline caches in this process and never start agy: a missing or stale cache is an error")
	maxAge := fs.String("max-age", "", "with --cache-only or --direct: ignore cached buckets older than this many seconds, negative for any age (default: statusline_max_age_s)")
	credits := fs.Bool("credits", false, "also print each signed-in account's credit balance (agy -p /credits: an agy start-up per account)")
	colorMode := fs.String("color", os.Getenv("agy_status_color"), "auto|always|never")
	absolute := fs.String("absolute", os.Getenv("agy_status_absolute_p"), "auto|y|n")
	columns := fs.Int("columns", 0, "terminal width (default: $agy_status_columns, $COLUMNS, 80)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	if *account != "" {
		pos = append(pos, *account)
	}
	var problem string
	switch {
	case *flat && *rf.json:
		problem = "--flat and --json are two different documents; pick one"
	case *credits && (*rf.json || *flat):
		problem = "--credits belongs to the human report, not to --json or --flat"
	case *maxAge != "" && !*cacheOnly && !*rf.direct:
		problem = "--max-age needs --cache-only or --direct (the daemon uses statusline_max_age_s from its config)"
	case *flat && len(pos) > 1:
		problem = "--flat reads one account"
	}
	if problem != "" {
		fmt.Fprintln(os.Stderr, "agent-quotas agy-status: "+problem)
		return exitUsage
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: config: %v\n", err)
		return exitFail
	}
	if *flat && len(pos) == 0 {
		name := os.Getenv("AGY_ACCOUNT_ID")
		if name == "" {
			name = "m"
		}
		pos = []string{name}
	}
	var selectors []string
	for _, a := range pos {
		selectors = append(selectors, model.ID(model.Agy, a))
	}
	refresh := *rf.refresh || deus()
	var snaps []model.Snapshot
	if *cacheOnly || *rf.direct {
		if *maxAge != "" {
			n, err := strconv.Atoi(*maxAge)
			if err != nil {
				fmt.Fprintf(os.Stderr, "agent-quotas agy-status: --max-age is not a whole number: %q\n", *maxAge)
				return exitUsage
			}
			cfg.Agy.StatuslineMaxAgeS = &n
		}
		r := agy.NewReader(cfg, procx.Exec{})
		r.CacheOnly = *cacheOnly
		doc, err := readWith(ctx, []model.Reader{r}, selectors, refresh)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-quotas: %v\n", err)
			return exitFail
		}
		snaps = doc.Accounts
	} else {
		var code int
		if snaps, code = providerSnapshots(ctx, model.Agy, selectors, refresh, false); code != exitOK {
			return code
		}
	}
	if *flat {
		return printFlat(snaps)
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
	code := agy.ExitCode(rows)
	if *credits && printCredits(ctx, os.Stdout, agy.NewReader(cfg, procx.Exec{}), snaps) != exitOK {
		code = exitFail
	}
	return code
}

// printFlat prints the one account's flat bucket array, with the reading's
// notes (where the numbers came from) on stderr as the zsh engine put them.
func printFlat(snaps []model.Snapshot) int {
	if len(snaps) != 1 {
		fmt.Fprintln(os.Stderr, "agy-status: --flat found no such account")
		return exitFail
	}
	s := snaps[0]
	for _, n := range s.Notes {
		fmt.Fprintln(os.Stderr, n.Text)
	}
	if len(s.Native) == 0 {
		msg := "no reading"
		if s.Error != nil {
			msg = *s.Error
		}
		fmt.Fprintf(os.Stderr, "agy-status: %s: %s\n", s.Name, msg)
		return exitFail
	}
	out, err := agy.FlatJSON(s.Native)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agy-status: %s: %v\n", s.Name, err)
		return exitFail
	}
	os.Stdout.Write(out)
	return exitOK
}

// creditsReader is the part of agy.Reader printCredits needs.
type creditsReader interface {
	Credits(ctx context.Context, name string) ([]agy.CreditRow, error)
}

// printCredits appends each signed-in account's credit balance to the human
// report, one line per row with the account in front. The accounts run in
// parallel, since each is an agy start-up. It returns exitFail when any
// account's balance could not be read; that account's line says why.
func printCredits(ctx context.Context, w io.Writer, r creditsReader, snaps []model.Snapshot) int {
	type result struct {
		rows []agy.CreditRow
		err  error
	}
	var names []string
	for _, s := range snaps {
		if s.Verdict != model.SignedOut {
			names = append(names, s.Name)
		}
	}
	results := make([]result, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := r.Credits(ctx, name)
			results[i] = result{rows, err}
		}()
	}
	wg.Wait()
	width := 0
	for _, n := range names {
		width = max(width, len(n))
	}
	fmt.Fprintln(w, "Credits (agy -p /credits)")
	code := exitOK
	for i, name := range names {
		if err := results[i].err; err != nil {
			fmt.Fprintf(w, "  %-*s  credits failed: %v\n", width, name, err)
			code = exitFail
			continue
		}
		for _, row := range results[i].rows {
			if row.Label == "" {
				fmt.Fprintf(w, "  %-*s  %s\n", width, name, row.Value)
				continue
			}
			fmt.Fprintf(w, "  %-*s  %s: %s\n", width, name, row.Label, row.Value)
		}
	}
	return code
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
