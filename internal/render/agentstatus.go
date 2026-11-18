package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// Envelope is one provider's answer, as the old collector
// (h-agent-status-collect) wrapped it: the provider's legacy --json output,
// its exit status, and for Antigravity the cache file of each account.
type Envelope struct {
	Agent      string            `json:"agent"`
	Exit       int               `json:"exit"`
	Report     json.RawMessage   `json:"report"`
	CacheFiles map[string]string `json:"cache_files"`
}

// Titles are the section titles per provider.
var Titles = map[string]string{"claude": "Claude Code", "codex": "Codex", "agy": "Antigravity"}

func baseAccount(name, tier, email, errValue any, source string) *Object {
	a := NewObject()
	a.Set("account", name)
	a.Set("label", name)
	a.Set("tier", tier)
	a.Set("email", email)
	a.Set("error", errValue)
	a.Set("buckets", []any{})
	state, src := "ready", source
	if truthy(errValue) {
		state, src = "unavailable", ""
	}
	a.Set("_display_state", state)
	a.Set("_display_source", src)
	a.Set("_display_notes", []any{})
	return a
}

func addNote(account *Object, severity, text string) {
	account.Set("_display_notes", append(asList(account.Get("_display_notes")), []any{severity, text}))
}

func addWindow(account *Object, family, window string, used, reset any) {
	n, ok := asNum(used)
	if !ok || !finite(n.F) || n.F < 0 || n.F > 100 {
		addNote(account, "warning", family+" "+window+": usage unavailable")
		return
	}
	b := NewObject()
	b.Set("bucket_id", family+"-"+window)
	b.Set("group", family)
	b.Set("used_percent", used)
	b.Set("resets_at", reset)
	b.Set("_display_family", family)
	b.Set("_display_window", window)
	account.Set("buckets", append(asList(account.Get("buckets")), b))
}

func strOf(v any) string {
	s, ok := v.(string)
	if !ok {
		bad("expected a string, got %s", pyTypeName(v))
	}
	return s
}

func getOr(o *Object, key string, dflt any) any {
	if o.Has(key) {
		return o.Get(key)
	}
	return dflt
}

func needObject(v any) *Object {
	o := asObject(v)
	if o == nil {
		bad("'%s' object has no attribute 'get'", pyTypeName(v))
	}
	return o
}

func iterList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case *Object:
		keys := make([]any, 0, t.Len())
		for _, k := range t.Keys() {
			keys = append(keys, k)
		}
		return keys
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out
	}
	bad("'%s' object is not iterable", pyTypeName(v))
	return nil
}

func cloudCreditNote(credit *Object, now float64, loc *time.Location) string {
	remaining, ok := asNumLoose(credit.Get("remaining_dollars"))
	if !ok {
		bad("bad remaining_dollars")
	}
	note := "Cloud credit: $" + fmtMoney(remaining.F)
	if limit, ok := asNumLoose(credit.Get("limit_dollars")); ok {
		note += " of $" + fmtMoney(limit.F)
	}
	note += " left"
	if exp, ok := asNumLoose(credit.Get("expires_at")); ok {
		note += ", expires in " + Duration(exp.F-now) + " (" + fmtDayMonth(epochTime(exp.F).In(loc)) + ")"
	}
	if truthy(credit.Get("locked_reason")) {
		note += " (locked: " + pyStr(credit.Get("locked_reason")) + ")"
	}
	return note
}

func claudeAccounts(report any, now float64, loc *time.Location) []*Object {
	rows := []any{report}
	if l, ok := report.([]any); ok {
		rows = l
	}
	var accounts []*Object
	for _, r := range rows {
		row := needObject(r)
		cache := asObject(or(row.Get("cache"), NewObject()))
		if cache == nil {
			bad("cache is not an object")
		}
		source := "live"
		if truthy(cache.Get("from_cache")) {
			if capture, ok := asNumLoose(cache.Get("fetched_at")); ok {
				source = "cache " + Duration(math.Max(0, now-capture.F))
			} else {
				source = "cache age unknown"
			}
		}
		if truthy(cache.Get("stale_reason")) {
			source += " (stale)"
		}
		account := baseAccount(or(row.Get("profile"), "default"), row.Get("plan"), nil, row.Get("error"), source)
		for _, w := range iterList(getOr(row, "windows", []any{})) {
			window := needObject(w)
			key := strOf(getOr(window, "key", ""))
			label := strOf(getOr(window, "label", ""))
			var family string
			if key == "session" || key == "weekly_all" {
				family = "Claude"
			} else {
				family = pyStrip(strings.TrimPrefix(label, "7d "))
				if family == "" {
					family = key
				}
			}
			period := label
			if key == "session" {
				period = "5h"
			} else if strings.HasPrefix(key, "weekly") {
				period = "weekly"
			}
			addWindow(account, family, period, window.Get("utilization_percent"), window.Get("resets_at"))
		}
		for _, note := range iterList(or(row.Get("warnings"), []any{})) {
			account.Set("_display_notes", append(asList(account.Get("_display_notes")), []any{"warning", note}))
		}
		if truthy(row.Get("token_expired")) {
			addNote(account, "warning", "OAuth token expired; run the profile's login command")
		}
		if truthy(cache.Get("stale_reason")) {
			account.Set("_display_notes", append(asList(account.Get("_display_notes")), []any{"warning", cache.Get("stale_reason")}))
		}
		if truthy(row.Get("relogin")) {
			addNote(account, "dim", "Relogin: "+pyStr(row.Get("relogin")))
		}
		if extra := asObject(row.Get("extra_usage")); extra != nil &&
			(truthy(extra.Get("is_enabled")) || truthy(extra.Get("used_credits")) || truthy(extra.Get("spend_limit_reached"))) {
			var parts []string
			for _, k := range extra.Keys() {
				if v := extra.Get(k); v != nil {
					parts = append(parts, strings.ReplaceAll(k, "_", " ")+": "+pyStr(v))
				}
			}
			sev := "dim"
			if truthy(extra.Get("spend_limit_reached")) {
				sev = "warning"
			}
			addNote(account, sev, "Extra usage: "+strings.Join(parts, ", "))
		}
		if credit := asObject(row.Get("cloud_credit")); credit != nil && truthy(credit.Get("available")) {
			remaining, ok := asNumLoose(or(credit.Get("remaining_dollars"), Int(0)))
			if !ok {
				bad("'>' not supported")
			}
			if remaining.F > 0 {
				addNote(account, "dim", cloudCreditNote(credit, now, loc))
			}
		}
		if len(asList(account.Get("buckets"))) == 0 && !truthy(account.Get("error")) {
			addNote(account, "warning", "No usage windows reported")
		}
		accounts = append(accounts, account)
	}
	return accounts
}

func codexWindow(window *Object) string {
	minutes := window.Get("windowDurationMins")
	if n, ok := asNumLoose(minutes); ok {
		switch {
		case n.F == 10080:
			return "weekly"
		case n.F == 300:
			return "5h"
		case n.F > 0:
			return Duration(n.F * 60)
		}
	}
	return pyStr(or(or(window.Get("label"), window.Get("key")), "quota"))
}

// codexLimit is get_codex_limit() of libs/codex_rate_limits.py.
func codexLimit(status any) *Object {
	data := asObject(status)
	if d := asObject(asObject(data.Get("rateLimitsByLimitId")).Get("codex")); d != nil {
		return d
	}
	if top := asObject(data.Get("rateLimits")); top != nil {
		return top
	}
	entries, ok := data.Get("authFiles").([]any)
	if !ok {
		return nil
	}
	var okEntries []*Object
	for _, e := range entries {
		if eo := asObject(e); eo != nil && truthy(eo.Get("ok")) {
			okEntries = append(okEntries, eo)
		}
	}
	if len(okEntries) == 0 {
		return nil
	}
	for _, e := range okEntries {
		if truthy(e.Get("active")) {
			return codexLimit(e)
		}
	}
	return codexLimit(okEntries[0])
}

func codexAccounts(report any, now float64) []*Object {
	rep := needObject(report)
	var accounts []*Object
	for _, r := range iterList(getOr(rep, "authFiles", []any{report})) {
		row := needObject(r)
		identity := needObject(or(row.Get("identity"), NewObject()))
		quota := needObject(or(row.Get("quota"), NewObject()))
		account := baseAccount(or(row.Get("alias"), "default"), identity.Get("planType"),
			identity.Get("planOwnerEmail"), row.Get("error"), "live")
		if truthy(identity.Get("workspaceName")) {
			account.Set("label", identity.Get("workspaceName"))
		}
		if v, ok := row.Get("ok").(bool); ok && !v && !truthy(account.Get("error")) {
			account.Set("error", "Status lookup failed")
			account.Set("_display_state", "unavailable")
			account.Set("_display_source", "")
		}
		for _, w := range iterList(getOr(quota, "windows", []any{})) {
			window := needObject(w)
			addWindow(account, "Codex", codexWindow(window), window.Get("usedPercent"), window.Get("resetsAt"))
		}
		reserve := needObject(or(quota.Get("lunaReserve"), NewObject()))
		limits := iterList(or(quota.Get("limits"), []any{}))
		if reserve.Len() > 0 {
			merged := []any{reserve}
			for _, l := range limits {
				if !jsonEqual(needObject(l).Get("limitId"), reserve.Get("limitId")) {
					merged = append(merged, l)
				}
			}
			limits = merged
			note := "Reserve: spent"
			if truthy(reserve.Get("available")) {
				note = "Reserve: available"
			}
			if truthy(reserve.Get("model")) {
				note += " via " + strOf(reserve.Get("model"))
			}
			sev := "warning"
			if truthy(reserve.Get("available")) {
				sev = "dim"
			}
			addNote(account, sev, note)
		}
		for _, l := range limits {
			limit := needObject(l)
			family := pyStr(or(or(or(limit.Get("label"), limit.Get("limitName")), limit.Get("limitId")), "Model"))
			for _, w := range iterList(getOr(limit, "windows", []any{})) {
				window := needObject(w)
				addWindow(account, family, codexWindow(window), window.Get("usedPercent"), window.Get("resetsAt"))
			}
		}
		if truthy(quota.Get("blocked")) {
			var reasons []string
			for _, x := range iterList(or(quota.Get("reasons"), []any{"quota exhausted"})) {
				reasons = append(reasons, strOf(x))
			}
			addNote(account, "alarm", "Allowance blocked: "+strings.Join(reasons, "; "))
		}
		credits := needObject(or(quota.Get("resetCredits"), NewObject()))
		if truthy(getOr(credits, "availableCount", Int(0))) {
			note := "Reset credits: " + pyStr(credits.Get("availableCount")) + " available"
			var titles []string
			seen := map[string]bool{}
			var expiries []float64
			for _, g := range iterList(getOr(credits, "credits", []any{})) {
				grant := needObject(g)
				if s, _ := grant.Get("status").(string); s != "available" {
					continue
				}
				if truthy(grant.Get("title")) {
					t := pyStr(grant.Get("title"))
					if !seen[t] {
						seen[t] = true
						titles = append(titles, t)
					}
				}
				if e, ok := asNumLoose(grant.Get("expiresAt")); ok {
					expiries = append(expiries, e.F)
				}
			}
			details := titles
			if len(expiries) > 0 {
				first := expiries[0]
				for _, e := range expiries[1:] {
					first = math.Min(first, e)
				}
				details = append(details, "expires in "+Duration(math.Max(0, first-now)))
			}
			if len(details) > 0 {
				note += " (" + strings.Join(details, ", ") + ")"
			}
			addNote(account, "dim", note)
		}
		balance := asObject(or(asObject(codexLimit(row)).Get("credits"), NewObject()))
		if balance != nil && (truthy(balance.Get("unlimited")) || truthy(balance.Get("hasCredits"))) {
			note := "Credits: available"
			if truthy(balance.Get("unlimited")) {
				note = "Credits: unlimited"
			}
			if balance.Get("balance") != nil {
				note += " (balance: " + pyStr(balance.Get("balance")) + ")"
			}
			addNote(account, "dim", note)
		}
		if len(asList(account.Get("buckets"))) == 0 && !truthy(account.Get("error")) {
			addNote(account, "warning", "No usage windows reported")
		}
		accounts = append(accounts, account)
	}
	average := needObject(or(rep.Get("averageUsage"), NewObject()))
	if len(accounts) > 1 && average.Len() > 0 {
		last := accounts[len(accounts)-1]
		usable, ok1 := asNumLoose(getOr(average, "usableCount", Int(0)))
		blocked, ok2 := asNumLoose(getOr(average, "blockedCount", Int(0)))
		if !ok1 || !ok2 {
			bad("bad counts")
		}
		total := Num{F: usable.F + blocked.F, IsInt: usable.IsInt && blocked.IsInt}
		addNote(last, "dim", "All accounts: "+usable.String()+"/"+total.String()+" usable")
		var cells []string
		for _, w := range iterList(or(average.Get("windows"), []any{})) {
			window := needObject(w)
			if n, ok := asNumLoose(window.Get("usedPercent")); ok {
				cells = append(cells, codexWindow(window)+" "+fmtG(n)+"%")
			}
		}
		if len(cells) > 0 {
			addNote(last, "dim", "Average usage: "+strings.Join(cells, ", "))
		}
		if reset, ok := asNumLoose(average.Get("firstTimeToReset")); ok {
			alias := pyStr(getOr(average, "firstTimeToResetAlias", "account"))
			addNote(last, "dim", "First reset: "+alias+" in "+Duration(math.Max(0, reset.F-now)))
		}
	}
	return accounts
}

func jsonEqual(a, b any) bool {
	var x, y bytes.Buffer
	pyJSON(&x, a)
	pyJSON(&y, b)
	return x.String() == y.String()
}

func sectionAccounts(env Envelope, now float64, loc *time.Location) (accounts []*Object, err error) {
	defer func() {
		if r := recover(); r != nil {
			b, ok := r.(badReport)
			if !ok {
				panic(r)
			}
			accounts, err = nil, b
		}
	}()
	if _, ok := Titles[env.Agent]; !ok {
		return nil, badReport{"Unknown provider"}
	}
	var report any
	if len(env.Report) > 0 {
		report, _ = Decode(env.Report)
	}
	if (asObject(report) == nil && asList(report) == nil) || !truthy(report) {
		return nil, badReport{fmt.Sprintf("Status lookup failed (exit %d)", env.Exit)}
	}
	switch env.Agent {
	case "claude":
		return claudeAccounts(report, now, loc), nil
	case "codex":
		return codexAccounts(report, now), nil
	}
	for _, r := range iterList(report) {
		row := asObject(r)
		if row == nil {
			bad("dictionary update sequence element #0 has length 1; 2 is required")
		}
		row = row.Copy()
		path := ""
		if name, ok := row.Get("account").(string); ok {
			path = env.CacheFiles[name]
		}
		row.Set("_display_cache_file", path)
		accounts = append(accounts, row)
	}
	return accounts, nil
}

func sectionModel(env Envelope, now float64, loc *time.Location) *Object {
	m := NewObject()
	m.Set("agent", Clean(env.Agent))
	title, ok := Titles[env.Agent]
	if !ok {
		title = Clean(env.Agent)
	}
	m.Set("title", title)
	m.Set("error", nil)
	accounts := []any{}
	list, err := sectionAccounts(env, now, loc)
	if err == nil {
		_, err = safeRender(func() string {
			for _, a := range list {
				accounts = append(accounts, accountModel(a, now).object())
			}
			return ""
		})
	}
	if err != nil {
		msg := cleanString(err.Error())
		if msg == "" {
			msg = "Invalid status response"
		}
		m.Set("error", msg)
		accounts = []any{}
	}
	m.Set("accounts", accounts)
	return m
}

// AgentStatusJSON is agent-status --json: the document the web panel reads,
// {generated_at, providers: [{agent, title, error, accounts}]}, in the
// formatting of Python's json.dump (", " and ": " separators, non-ASCII
// kept), ending in a newline.
func AgentStatusJSON(envs []Envelope, now time.Time) ([]byte, error) {
	return agentStatusJSON(envs, now, time.Local)
}

func agentStatusJSON(envs []Envelope, now time.Time, loc *time.Location) ([]byte, error) {
	epoch := epochOf(now)
	providers := []any{}
	for _, env := range envs {
		providers = append(providers, sectionModel(env, epoch, loc))
	}
	doc := NewObject()
	doc.Set("generated_at", Float(epoch))
	doc.Set("providers", providers)
	var b bytes.Buffer
	pyJSON(&b, doc)
	b.WriteByte('\n')
	return b.Bytes(), nil
}

func renderSection(env Envelope, columns int, color bool, absolute string, now float64, loc *time.Location) string {
	title, ok := Titles[env.Agent]
	if !ok {
		title = Clean(env.Agent)
	}
	head := Clip(title, columns)
	rule := strings.Repeat("─", max(0, columns-Width(head)-2))
	heading := head + "  " + rule
	if color {
		heading = "\033[1;34m" + head + "\033[0m  \033[2m" + rule + "\033[0m"
	}
	accounts, err := sectionAccounts(env, now, loc)
	var body string
	if err == nil {
		body, err = safeRender(func() string { return renderAccounts(accounts, columns, color, absolute, now, loc) })
	}
	if err != nil {
		msg := err.Error()
		if msg == "" {
			msg = "Invalid status response"
		}
		message := "  ! " + Clip(msg, columns-4)
		if color {
			message = "\033[31m" + message + "\033[0m"
		}
		body = message + "\n"
	}
	return rstrip(heading) + "\n\n" + rstrip(body) + "\n\n"
}

// RenderAgentStatus writes the combined terminal report, one section per
// envelope in order.
func RenderAgentStatus(w io.Writer, envs []Envelope, opts Options) error {
	columns := max(40, opts.Columns)
	for _, env := range envs {
		if _, err := io.WriteString(w, renderSection(env, columns, opts.Color, opts.absolute(), opts.now(), opts.loc())); err != nil {
			return err
		}
	}
	return nil
}

// ParseEnvelopes reads the collector's stream: one JSON envelope per line,
// blank lines skipped, and a line that is not an object becoming the
// "Status" section that says Unknown provider, as the Python renderer did.
func ParseEnvelopes(data []byte) []Envelope {
	var out []Envelope
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var env Envelope
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &probe); err != nil || probe == nil {
			out = append(out, Envelope{Agent: "Status"})
			continue
		}
		_ = json.Unmarshal([]byte(line), &env)
		if _, ok := probe["agent"]; !ok {
			env.Agent = "unknown"
		}
		out = append(out, env)
	}
	return out
}
