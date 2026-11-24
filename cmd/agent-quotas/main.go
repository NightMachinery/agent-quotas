// Command agent-quotas is the daemon that owns every coding-agent account's
// quota, and the CLI that reads it. See docs/design.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/client"
	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/daemon"
	"github.com/NightMachinery/agent-quotas/internal/launchd"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

// Exit codes.
const (
	exitOK         = 0
	exitFail       = 1
	exitUsage      = 2
	exitDaemonDown = 3
)

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string) int
}

var commands []command

func init() {
	commands = []command{
		{"serve", "run the daemon in the foreground (launchd runs this)", cmdServe},
		{"status", "show accounts: status [--json] [--refresh] [--direct] [ACCOUNT|PROVIDER...]", cmdStatus},
		{"refresh", "force a read of the named accounts: refresh [ACCOUNT|PROVIDER...]", cmdRefresh},
		{"transitions", "verdict transitions: transitions [--since EPOCH|--hours N] [--json]", cmdTransitions},
		{"health", "whether the daemon answers, and its version", cmdHealth},
		{"claude-code-usage", "the old claude-code-usage report and JSON", cmdClaudeCodeUsage},
		{"codex-status", "the old codex-status report and JSON", cmdCodexStatus},
		{"agy-status", "the old agy-status report and JSON", cmdAgyStatus},
		{"agy-auth", "where an agy sign-in is stored, offline: agy-auth ACCOUNT | --dir GEMINI_ROOT", cmdAgyAuth},
		{"agent-status", "the combined report: agent-status [--json] [claude codex agy]", cmdAgentStatus},
		{"continue", "auto-continue registrations: continue on|off|status|list|check|stopped", cmdContinue},
		{"keep", "keeper cache policy: keep status|set|standby|compact [AGENT_ID]", cmdKeep},
		{"hook", "agent hook entry point: hook claude|agy [PANE] < payload", cmdHook},
		{"install", "install and start the launchd agent: install [--print]", cmdInstall},
		{"uninstall", "stop and remove the launchd agent", cmdUninstall},
		{"restart", "restart the launchd agent (after a config change)", cmdRestart},
		{"config", "config path|show|init [--force]", cmdConfig},
		{"version", "print the version", func(context.Context, []string) int { fmt.Println(version); return 0 }},
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: agent-quotas COMMAND [ARGS]")
	fmt.Fprintln(w)
	for _, c := range commands {
		fmt.Fprintf(w, "  %-18s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "A non-empty deusvult (set by the shell's deus alias) means --refresh.")
}

func main() {
	daemon.Version = version
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(exitUsage)
	}
	name := os.Args[1]
	if name == "-h" || name == "--help" || name == "help" {
		usage(os.Stdout)
		return
	}
	for _, c := range commands {
		if c.name == name {
			os.Exit(c.run(ctx, os.Args[2:]))
		}
	}
	fmt.Fprintf(os.Stderr, "agent-quotas: unknown command %q\n", name)
	usage(os.Stderr)
	os.Exit(exitUsage)
}

// parseInterspersed parses flags that may appear before or after positional
// arguments, which the flag package alone does not allow.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("agent-quotas "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// deus reports whether the caller asked for a forced refresh through the
// shell's deus alias.
func deus() bool { return os.Getenv("deusvult") != "" }

func loadConfig() (*config.Config, error) { return config.Load(config.Path()) }

func newClient(timeout time.Duration) *client.Client {
	return client.New(config.SocketPath(), timeout)
}

func daemonDown(err error) int {
	fmt.Fprintf(os.Stderr, "agent-quotas: daemon not running (socket %s); start it with: agent-quotas install (or agent-quotas serve), or pass --direct\n", config.SocketPath())
	return exitDaemonDown
}

// clientErr prints err and returns the exit code it deserves.
func clientErr(err error) int {
	if errors.Is(err, client.ErrDaemonDown) {
		return daemonDown(err)
	}
	fmt.Fprintf(os.Stderr, "agent-quotas: %v\n", err)
	return exitFail
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: %v\n", err)
		return exitFail
	}
	return exitOK
}

// ---- serve ----

func cmdServe(ctx context.Context, args []string) int {
	fs := newFlags("serve")
	socket := fs.String("socket", config.SocketPath(), "unix socket path")
	if _, err := parseInterspersed(fs, args); err != nil {
		return exitUsage
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: config: %v\n", err)
		return exitFail
	}
	if err := serve(ctx, cfg, *socket); err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: %v\n", err)
		return exitFail
	}
	return exitOK
}

// ---- status / refresh / transitions / health ----

type readFlags struct {
	json    *bool
	refresh *bool
	direct  *bool
}

func addReadFlags(fs *flag.FlagSet) readFlags {
	return readFlags{
		json:    fs.Bool("json", false, "print JSON"),
		refresh: fs.Bool("refresh", false, "bypass the cache for the named accounts (also: deusvult=y)"),
		direct:  fs.Bool("direct", false, "read in this process instead of asking the daemon"),
	}
}

// readDocument gets the selected accounts from the daemon, or directly.
func readDocument(ctx context.Context, selectors []string, refresh, direct bool) (model.Document, int) {
	if direct {
		cfg, err := loadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-quotas: config: %v\n", err)
			return model.Document{}, exitFail
		}
		doc, err := directDocument(ctx, cfg, selectors, refresh)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-quotas: %v\n", err)
			return model.Document{}, exitFail
		}
		return doc, exitOK
	}
	doc, err := newClient(5*time.Minute).Accounts(ctx, selectors, refresh)
	if err != nil {
		return doc, clientErr(err)
	}
	return doc, exitOK
}

func cmdStatus(ctx context.Context, args []string) int {
	fs := newFlags("status")
	rf := addReadFlags(fs)
	sel, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	doc, code := readDocument(ctx, sel, *rf.refresh || deus(), *rf.direct)
	if code != exitOK {
		return code
	}
	if *rf.json {
		return printJSON(doc)
	}
	writeStatus(os.Stdout, doc, time.Now())
	return exitOK
}

func cmdRefresh(ctx context.Context, args []string) int {
	fs := newFlags("refresh")
	asJSON := fs.Bool("json", false, "print JSON")
	sel, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	doc, err := newClient(5*time.Minute).Refresh(ctx, sel)
	if err != nil {
		return clientErr(err)
	}
	if *asJSON {
		return printJSON(doc)
	}
	writeStatus(os.Stdout, doc, time.Now())
	return exitOK
}

func cmdTransitions(ctx context.Context, args []string) int {
	fs := newFlags("transitions")
	since := fs.Float64("since", 0, "epoch seconds")
	hours := fs.Float64("hours", 24, "look back this many hours (when --since is not given)")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parseInterspersed(fs, args); err != nil {
		return exitUsage
	}
	from := *since
	if from == 0 {
		from = model.Unix(time.Now().Add(-time.Duration(*hours * float64(time.Hour))))
	}
	ts, err := newClient(time.Minute).Transitions(ctx, from)
	if err != nil {
		return clientErr(err)
	}
	if *asJSON {
		return printJSON(map[string]any{"schema_version": model.SchemaVersion, "transitions": ts})
	}
	for _, t := range ts {
		line := fmt.Sprintf("%s  %-16s %s -> %s", model.Time(t.At).Local().Format("2006-01-02 15:04:05"), t.ID, t.From, t.To)
		if len(t.Reasons) > 0 {
			line += "  (" + strings.Join(t.Reasons, "; ") + ")"
		}
		fmt.Println(line)
	}
	return exitOK
}

func cmdHealth(ctx context.Context, args []string) int {
	h, err := newClient(10 * time.Second).Health(ctx)
	if err != nil {
		return clientErr(err)
	}
	return printJSON(h)
}

// ---- install / uninstall / restart ----

func cmdInstall(ctx context.Context, args []string) int {
	fs := newFlags("install")
	printOnly := fs.Bool("print", false, "print the plist instead of installing it")
	binary := fs.String("binary", "", "path of the agent-quotas binary to run (default: this one)")
	if _, err := parseInterspersed(fs, args); err != nil {
		return exitUsage
	}
	bin := *binary
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-quotas: %v\n", err)
			return exitFail
		}
		bin, _ = filepath.EvalSymlinks(exe)
	}
	home := config.Home()
	spec := launchd.Spec{Label: launchd.Label, Binary: bin,
		LogPath: filepath.Join(config.StateDir(), "daemon.log"),
		Path:    launchd.SearchPath(bin, launchd.LookPath, home), Home: home}
	if *printOnly {
		text, err := launchd.Render(spec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-quotas: %v\n", err)
			return exitFail
		}
		fmt.Print(text)
		return exitOK
	}
	if err := daemon.EnsurePrivateDir(config.StateDir()); err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: %v\n", err)
		return exitFail
	}
	path, err := launchd.Install(ctx, procx.Exec{}, spec, home)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: install: %v\n", err)
		return exitFail
	}
	fmt.Printf("installed %s and started %s\n", path, launchd.Label)
	return exitOK
}

func cmdUninstall(ctx context.Context, args []string) int {
	if err := launchd.Uninstall(ctx, procx.Exec{}, config.Home()); err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: uninstall: %v\n", err)
		return exitFail
	}
	fmt.Println("uninstalled " + launchd.Label)
	return exitOK
}

func cmdRestart(ctx context.Context, args []string) int {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), launchd.Label)
	if _, err := (procx.Exec{}).Run(ctx, procx.Cmd{Argv: []string{"launchctl", "kickstart", "-k", target}, Timeout: 30 * time.Second}); err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: restart: %v\n", err)
		return exitFail
	}
	fmt.Println("restarted " + launchd.Label)
	return exitOK
}

// ---- config ----

func cmdConfig(ctx context.Context, args []string) int {
	fs := newFlags("config")
	force := fs.Bool("force", false, "with init: overwrite an existing file")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: agent-quotas config path|show|init [--force]")
		return exitUsage
	}
	switch pos[0] {
	case "path":
		fmt.Println(config.Path())
		return exitOK
	case "show":
		cfg, err := loadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-quotas: %v\n", err)
			return exitFail
		}
		return printJSON(cfg)
	case "init":
		cfg := config.Discover(os.Getenv)
		data, _ := json.MarshalIndent(cfg, "", "  ")
		data = append(data, '\n')
		path := config.Path()
		if _, err := os.Stat(path); err == nil && !*force {
			fmt.Fprintf(os.Stderr, "agent-quotas: %s exists; not overwriting (use --force). Proposed config:\n", path)
			os.Stdout.Write(data)
			return exitFail
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "agent-quotas: %v\n", err)
			return exitFail
		}
		if err := daemon.WriteFileAtomic(path, data, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "agent-quotas: %v\n", err)
			return exitFail
		}
		os.Stdout.Write(data)
		fmt.Fprintf(os.Stderr, "wrote %s; review it, then: agent-quotas restart\n", path)
		return exitOK
	}
	fmt.Fprintln(os.Stderr, "usage: agent-quotas config path|show|init [--force]")
	return exitUsage
}
