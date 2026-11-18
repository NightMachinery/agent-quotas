package native

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// TargetOptions tune target resolution.
type TargetOptions struct {
	// Frontmost asks for blind typing into whatever holds the keyboard at
	// resume time. Never chosen otherwise.
	Frontmost bool
	// Outside is set when the session was picked from another shell: the
	// caller's TMUX_PANE is not the session's, so the pane is found by
	// transcript instead.
	Outside bool
}

// ResolveTargets is h-agent-auto-continue-targets: how a resume reaches this
// session, in order of how little each needs. A Codex thread is queued by
// id; a background Claude Code session is reached through a scratch attach;
// a tmux pane needs no window manager; a kitty window needs kitty's remote
// control. The error says what was tried.
func (e *Engine) ResolveTargets(ctx context.Context, agent, id, transcript string, env map[string]string, opts TargetOptions) ([]string, error) {
	if opts.Frontmost {
		return []string{"frontmost"}, nil
	}
	if agent == "codex" {
		return []string{"codex:" + id}, nil
	}
	if agent == "claude" {
		if _, ok := e.ClaudeBgFind(ctx, id); ok {
			return []string{"claude-bg:" + id}, nil
		}
	}

	var tried []string
	live, liveErr := e.Lister.Live(ctx)
	if liveErr != nil {
		live = nil
	}
	pane := env["TMUX_PANE"]
	switch {
	case opts.Outside:
		found := ""
		if transcript != "" {
			for _, r := range e.TmuxPanes(ctx, live) {
				if r.Transcript == transcript {
					found = r.Pane
					break
				}
			}
		}
		if found != "" {
			return []string{"tmux:" + found}, nil
		}
		tried = append(tried, "no tmux pane holds this session")
	case pane != "":
		if paneHolds(e.TmuxPanes(ctx, live), pane) {
			return []string{"tmux:" + pane}, nil
		}
		tried = append(tried, fmt.Sprintf("tmux pane %s is not listed as holding a live agent session", pane))
	default:
		tried = append(tried, "not inside tmux")
	}

	if transcript != "" {
		pairs, _ := e.KittyPairs(ctx, env, live)
		for _, p := range pairs {
			if p.Transcript == transcript {
				return []string{"kitty:" + strconv.Itoa(p.Window)}, nil
			}
		}
		tried = append(tried, "no kitty window shows this session")
	} else {
		tried = append(tried, "no transcript yet, so no kitty window can be matched")
	}

	return nil, fmt.Errorf("no way to reach this session: %s. Run with --frontmost to type wherever the keyboard focus is at the time", strings.Join(tried, "; "))
}
