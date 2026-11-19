package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/client"
	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const continueUsage = `usage: agent-quotas continue on|off|status [PASEO_AGENT_ID] [--home DIR] [--poll SECONDS] [--frontmost]
       agent-quotas continue list [--prune] [--paseo]
       agent-quotas continue stopped [--usable] [--home DIR]
       agent-quotas continue check [PASEO_AGENT_ID|SCOPE]

Inside a Paseo agent (PASEO_AGENT_ID set) or given an agent UUID, on/off/status
act on that Paseo agent; otherwise on the native Claude Code, Codex or
Antigravity session this shell belongs to.

Paseo auto-continue resumes a quota-failed work turn once when quota returns.
Leave it enabled after finishing work. Use off to opt out of future resumes.`

// cmdContinue registers sessions for auto-continue. Registration runs in
// this process, since it needs the caller's environment; the daemon is then
// asked to check the registration (it also finds it on its own).
func cmdContinue(ctx context.Context, args []string) int {
	fs := newFlags("continue")
	home := fs.String("home", "", "Paseo daemon home (default: $PASEO_HOME or ~/.paseo)")
	poll := fs.Int("poll", 300, "Paseo: safety-check interval in seconds (at least 30)")
	frontmost := fs.Bool("frontmost", false, "native: resume by typing into whatever holds the keyboard")
	usable := fs.Bool("usable", false, "stopped: only agents whose seat has quota left now")
	prune := fs.Bool("prune", false, "list: forget native registrations whose session ended")
	paseoOnly := fs.Bool("paseo", false, "list: only the Paseo rows (agent id, home, agent/profile, status; tab-separated)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, continueUsage) }
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) == 0 {
		fmt.Fprintln(os.Stderr, continueUsage)
		return exitUsage
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-quotas: config: %v\n", err)
		return exitFail
	}
	action, rest := pos[0], pos[1:]
	target := ""
	if len(rest) > 0 {
		target = rest[0]
	}
	eng := paseo.New(cfg, quotaAdapter{cfg: cfg, snap: daemonSnapshots, maxAge: engineQuotaMaxAge})
	paseoTarget := target
	if paseoTarget == "" {
		paseoTarget = os.Getenv("PASEO_AGENT_ID")
	}
	isPaseo := paseoTarget != "" && (target == "" || uuidRE.MatchString(target))

	switch action {
	case "on", "off", "status":
		if !isPaseo {
			return nativeCommand(ctx, cfg, action, nativeOptions{frontmost: *frontmost})
		}
		if err := paseo.CheckRemote(os.Getenv("PASEO_HOST")); err != nil {
			return paseoErr(err)
		}
		var out string
		switch action {
		case "on":
			out, err = eng.On(ctx, paseoTarget, *home, *poll)
		case "off":
			out, err = eng.Off(ctx, paseoTarget, *home)
		default:
			out, err = eng.Status(ctx, paseoTarget, *home)
		}
		if err != nil {
			return paseoErr(err)
		}
		fmt.Println(out)
		if action == "on" {
			path, perr := eng.RegistrationPath(paseoTarget, *home)
			if perr == nil {
				askCheck(ctx, ContinueCheckRequest{Kind: "paseo", Path: path})
			}
		}
		return exitOK
	case "list":
		rows, err := eng.List()
		if err != nil {
			return paseoErr(err)
		}
		for _, row := range rows {
			fmt.Println(row)
		}
		if *paseoOnly {
			return exitOK
		}
		return nativeList(ctx, cfg, *prune)
	case "stopped":
		ids, err := eng.Stopped(ctx, *home, *usable, warnStderr)
		for _, id := range ids {
			fmt.Println(id)
		}
		if err != nil {
			return paseoErr(err)
		}
		return exitOK
	case "check":
		req := ContinueCheckRequest{Kind: "native", Scope: target}
		if isPaseo {
			path, err := eng.RegistrationPath(paseoTarget, *home)
			if err != nil {
				return paseoErr(err)
			}
			req = ContinueCheckRequest{Kind: "paseo", Path: path}
		}
		if err := newClient(time.Minute).Do(ctx, "POST", "/v1/continue/check", req, nil); err != nil {
			return clientErr(err)
		}
		fmt.Println("check queued")
		return exitOK
	}
	fmt.Fprintln(os.Stderr, continueUsage)
	return exitUsage
}

func paseoErr(err error) int {
	var pe *paseo.Error
	if errors.As(err, &pe) {
		fmt.Fprintln(os.Stderr, "paseo-auto-continue: "+err.Error())
	} else {
		fmt.Fprintln(os.Stderr, "paseo-auto-continue: state/check unavailable; no message sent ("+err.Error()+")")
	}
	return exitFail
}

// askCheck asks the daemon to check a registration now. A daemon that is
// not running finds the registration when it starts, so that is a note,
// not a failure.
func askCheck(ctx context.Context, req ContinueCheckRequest) {
	err := newClient(30*time.Second).Do(ctx, "POST", "/v1/continue/check", req, nil)
	if errors.Is(err, client.ErrDaemonDown) {
		fmt.Fprintln(os.Stderr, "note: the agent-quotas daemon is not running; the registration is saved and is checked once it starts ("+
			strings.TrimSpace("agent-quotas install")+")")
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "note: could not ask the daemon to check now: "+err.Error())
	}
}

type nativeOptions struct {
	frontmost bool
}

var _ = config.Path
