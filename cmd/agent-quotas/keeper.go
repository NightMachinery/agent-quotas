package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/client"
	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/daemon"
	"github.com/NightMachinery/agent-quotas/internal/keeper"
	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/paseo"
)

// keeperSeats resolves an agent's identity to its seat from the store.
type keeperSeats struct {
	q    quotaAdapter
	snap func(ctx context.Context, id string) (model.Snapshot, error)
}

// Seat implements keeper.Seats.
func (s keeperSeats) Seat(ctx context.Context, id paseo.Identity) (keeper.Seat, error) {
	var account string
	switch id.Agent {
	case model.Claude:
		account = model.ID(model.Claude, id.Profile)
	case model.Codex:
		a, err := s.q.codexAccount(id.NativeHome)
		if err != nil {
			return keeper.Seat{}, err
		}
		account = a
	default:
		return keeper.Seat{}, fmt.Errorf("no seat for agent kind %q", id.Agent)
	}
	snap, err := s.snap(ctx, account)
	if err != nil {
		return keeper.Seat{Account: account, Verdict: model.Unknown}, nil
	}
	verdict := snap.Verdict
	if verdict == "" {
		verdict = model.Unknown
	}
	return keeper.Seat{Account: account, Verdict: verdict, Overage: claudeOverage(snap)}, nil
}

// claudeOverage reports whether a Claude seat runs on extra usage: an
// account-wide window is at 100% while extra usage is enabled and its spend
// limit is not reached.
func claudeOverage(s model.Snapshot) bool {
	if s.Provider != model.Claude {
		return false
	}
	exhausted := false
	for _, w := range s.Windows {
		if w.Scope == model.ScopeAccount && w.Blocking {
			exhausted = true
		}
	}
	if !exhausted || len(s.Native) == 0 {
		return false
	}
	var row struct {
		ExtraUsage *struct {
			IsEnabled         bool `json:"is_enabled"`
			SpendLimitReached bool `json:"spend_limit_reached"`
		} `json:"extra_usage"`
	}
	if err := json.Unmarshal(s.Native, &row); err != nil || row.ExtraUsage == nil {
		return false
	}
	return row.ExtraUsage.IsEnabled && !row.ExtraUsage.SpendLimitReached
}

// attachKeeper adds the keeper's API to the daemon and returns its loop
// (which only observes, for the read API, while the keeper is disabled).
func attachKeeper(cfg *config.Config, d *daemon.Daemon, srv *daemon.Server, quotas quotaAdapter, logger *log.Logger, autoContinueChanged func(string)) func(context.Context) {
	eng := paseo.New(cfg, quotas)
	seats := keeperSeats{q: quotas, snap: func(ctx context.Context, id string) (model.Snapshot, error) {
		return d.Get(ctx, id, false)
	}}
	k := keeper.New(cfg, eng, paseo.CLI{Binary: cfg.Paseo.Binary}, seats, logger)
	k.AutoContinueChanged = autoContinueChanged
	k.Register(srv)
	if cfg.KeeperEnabled() {
		logger.Printf("keeper enabled (default policy %s)", cfg.Keeper.DefaultPolicy)
	} else {
		logger.Printf("keeper disabled (keeper.enabled is false): cache actions disabled")
	}
	return k.Run
}

// ---- keep ----

const keepUsage = `usage: agent-quotas keep status [AGENT_ID] [--all] [--json] [--home DIR]
       agent-quotas keep set [AGENT_ID] --cache off|compact|warm [--warm-hours N]
       agent-quotas keep standby [AGENT_ID]
       agent-quotas keep compact [AGENT_ID] [--wait]

The keeper's cache policy for idle Paseo agents. AGENT_ID defaults to
PASEO_AGENT_ID (status with neither lists the agents with a file; --all adds
every agent Paseo lists). Needs the daemon.`

func cmdKeep(ctx context.Context, args []string) int {
	fs := newFlags("keep")
	asJSON := fs.Bool("json", false, "print JSON")
	all := fs.Bool("all", false, "status: every agent Paseo lists, not only those with a file")
	home := fs.String("home", "", "Paseo daemon home (default: the daemon's first)")
	cache := fs.String("cache", "", "set: off, compact or warm")
	warmHours := fs.Float64("warm-hours", 0, "set: hours to keep warming after the last real turn")
	wait := fs.Bool("wait", false, "compact: wait until the compaction ends")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, keepUsage) }
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) == 0 || len(pos) > 2 {
		fmt.Fprintln(os.Stderr, keepUsage)
		return exitUsage
	}
	action := pos[0]
	id, setBy := "", "cli"
	if len(pos) == 2 {
		id = pos[1]
	} else if env := os.Getenv("PASEO_AGENT_ID"); env != "" {
		id, setBy = env, "agent"
	}
	q := url.Values{}
	if *home != "" {
		q.Set("home", *home)
	}
	c := newClient(3 * time.Minute)
	agentPath := func(suffix string) string {
		p := "/v1/agents/" + url.PathEscape(id) + suffix
		if len(q) > 0 {
			p += "?" + q.Encode()
		}
		return p
	}
	needID := func() bool {
		if id == "" {
			fmt.Fprintln(os.Stderr, "agent-quotas keep: pass an agent id or run inside a Paseo agent")
			return false
		}
		return true
	}
	switch action {
	case "status":
		if id == "" || *all {
			if *all {
				q.Set("all", "1")
			}
			path := "/v1/agents"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var doc keeper.ListDocument
			if err := c.Do(ctx, "GET", path, nil, &doc); err != nil {
				return clientErr(err)
			}
			if *asJSON {
				return printJSON(doc)
			}
			state := "disabled"
			if doc.Enabled {
				state = "enabled"
			}
			fmt.Printf("keeper: %s; %d agent(s)\n", state, len(doc.Agents))
			for _, v := range doc.Agents {
				writeKeepView(v)
			}
			return exitOK
		}
		var doc keeper.Document
		if err := c.Do(ctx, "GET", agentPath(""), nil, &doc); err != nil {
			return clientErr(err)
		}
		if *asJSON {
			return printJSON(doc)
		}
		writeKeepView(doc.Agent)
		return exitOK
	case "set", "standby":
		if !needID() {
			return exitUsage
		}
		suffix, body := "/standby", keeper.PolicyRequest{SetBy: setBy}
		if action == "set" {
			if *cache == "" {
				fmt.Fprintln(os.Stderr, keepUsage)
				return exitUsage
			}
			suffix, body = "/policy", keeper.PolicyRequest{Cache: *cache, WarmHours: *warmHours, SetBy: setBy}
		}
		var doc keeper.Document
		if err := c.Do(ctx, "POST", agentPath(suffix), body, &doc); err != nil {
			return clientErr(err)
		}
		if *asJSON {
			return printJSON(doc)
		}
		writeKeepView(doc.Agent)
		return exitOK
	case "compact":
		if !needID() {
			return exitUsage
		}
		var res keeper.CompactResult
		if err := c.Do(ctx, "POST", agentPath("/compact"), struct{}{}, &res); err != nil {
			return clientErr(err)
		}
		if !*wait {
			if *asJSON {
				return printJSON(res)
			}
			fmt.Printf("compaction %s %s\n", res.CompactionID, res.State)
			return exitOK
		}
		doc, err := waitCompaction(ctx, newClient(3*time.Minute), agentPath(""), res.CompactionID)
		if err != nil {
			return clientErr(err)
		}
		if *asJSON {
			return printJSON(doc)
		}
		comp := compactionOf(doc.Agent)
		if comp == nil || comp.ID != res.CompactionID {
			fmt.Printf("compaction %s: no longer the latest\n", res.CompactionID)
			return exitFail
		}
		line := fmt.Sprintf("compaction %s %s", comp.ID, comp.State)
		if comp.Error != "" {
			line += " (" + comp.Error + ")"
		}
		fmt.Println(line)
		if comp.State != keeper.StateCompleted {
			return exitFail
		}
		return exitOK
	}
	fmt.Fprintln(os.Stderr, keepUsage)
	return exitUsage
}

func compactionOf(v keeper.View) *keeper.Compaction {
	if v.Cache == nil {
		return nil
	}
	return v.Cache.Compaction
}

// waitCompaction long-polls until the compaction is terminal, at most the
// compaction timeout plus a margin.
func waitCompaction(ctx context.Context, c *client.Client, agentPath, cid string) (keeper.Document, error) {
	sep := "?"
	if strings.Contains(agentPath, "?") {
		sep = "&"
	}
	path := agentPath + sep + "wait_compaction=" + url.QueryEscape(cid) + "&timeout=120"
	deadline := time.Now().Add(25 * time.Minute)
	for {
		var doc keeper.Document
		if err := c.Do(ctx, "GET", path, nil, &doc); err != nil {
			return doc, err
		}
		comp := compactionOf(doc.Agent)
		if comp == nil || comp.ID != cid || comp.Terminal() || time.Now().After(deadline) {
			return doc, nil
		}
		if ctx.Err() != nil {
			return doc, ctx.Err()
		}
	}
}

func fmtTime(t *float64) string {
	if t == nil {
		return "-"
	}
	return model.Time(*t).Local().Format("01-02 15:04")
}

func writeKeepView(v keeper.View) {
	title := ""
	if v.Title != nil {
		title = " " + *v.Title
	}
	fmt.Printf("%s%s [%s] %s\n", v.ID, title, v.Provider, v.Status)
	policy, phase := keeper.PolicyOff, keeper.PhaseOff
	if v.Cache != nil {
		policy, phase = v.Cache.Policy, v.Cache.Phase
	}
	line := "  policy " + policy + " (" + phase + ")"
	if v.WarmUntil != nil {
		line += ", warm until " + fmtTime(v.WarmUntil)
	}
	at := ""
	if v.NextAction.At != nil {
		at = " at " + fmtTime(v.NextAction.At)
	}
	fmt.Printf("%s; next: %s%s (%s)\n", line, v.NextAction.Kind, at, v.NextAction.Reason)
	ctxText := "unknown"
	if v.ContextTokens != nil {
		ctxText = fmt.Sprintf("%dk", *v.ContextTokens/1000)
	}
	fmt.Printf("  context %s (min %dk, big %v); cache expires %s (ttl %d s, %s); last activity %s (%s)\n",
		ctxText, v.MinContextTokens/1000, v.Big, fmtTime(v.CacheExpiresAt), v.TTLSeconds, v.TTLReason,
		fmtTime(v.LastActivityAt), v.LastActivitySource)
	extra := []string{}
	if v.Seat != nil {
		extra = append(extra, "seat "+v.Seat.Account+" "+v.Seat.Verdict)
	}
	if comp := compactionOf(v); comp != nil {
		s := "compaction " + comp.ID + " " + comp.State
		if comp.Error != "" {
			s += " (" + comp.Error + ")"
		}
		extra = append(extra, s)
	}
	if v.PingsSinceActivity > 0 {
		extra = append(extra, fmt.Sprintf("%d ping(s) since activity", v.PingsSinceActivity))
	}
	if v.Sendable.OK {
		extra = append(extra, "sendable")
	} else {
		extra = append(extra, "not sendable: "+v.Sendable.Reason)
	}
	fmt.Println("  " + strings.Join(extra, "; "))
}
