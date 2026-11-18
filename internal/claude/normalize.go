package claude

import (
	"errors"
	"fmt"
	"strings"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

// accountWide are the window keys that limit every model of the account:
// the limits-array kinds and their legacy names.
var accountWide = map[string]bool{"session": true, "five_hour": true, "weekly_all": true, "seven_day": true}

func windowPeriod(key, label string) (string, *float64) {
	switch {
	case key == "session" || key == "five_hour":
		return "5h", model.F(300)
	case strings.HasPrefix(key, "weekly") || strings.HasPrefix(key, "seven_day"):
		return "weekly", model.F(7 * 24 * 60)
	}
	return label, nil
}

// windowFamily: "Claude" for account-wide windows; the scoped model's name
// otherwise (a limit's scope.model.display_name, or the label without its
// "7d " prefix, as the agent-status report does).
func windowFamily(w usageWindow) (family, scope string) {
	if accountWide[w.key] {
		return "Claude", model.ScopeAccount
	}
	if w.scopeModel != "" {
		return w.scopeModel, model.ScopeModel
	}
	if w.key == "weekly_scoped" || strings.HasPrefix(w.key, "seven_day_") {
		f := strings.TrimSpace(strings.TrimPrefix(w.label, "7d "))
		if f == "" {
			f = w.key
		}
		return f, model.ScopeModel
	}
	return "Claude", model.ScopeAccount
}

// normalize turns a fetch result into the snapshot's windows, verdict and
// notes.
//
// Verdict: blocked when an account-wide window is at 100% or more and its
// reset is still ahead. A window whose reset has passed ("rolled over",
// only seen in cached data) is not blocking: its reading predates the
// reset. No account-wide window, or one without a number, gives unknown.
func (r *Reader) normalize(name string, tok *tokenInfo, res *fetchResult, warnings []string, now float64) model.Snapshot {
	s := r.baseSnapshot(name)
	if tok != nil {
		s.Plan = tok.subscription
	}
	s.Source = res.source
	if res.fetchedAt > 0 {
		s.FetchedAt = model.F(res.fetchedAt)
	}
	if res.source != "api" {
		s.Stale = true
		if res.staleReason != nil {
			s.StaleReason = res.staleReason
		} else {
			s.StaleReason = model.S("local cache")
		}
	}

	var blockedUntil *float64
	sawAccount, unknown, blocked := false, false, false
	for _, w := range extractWindows(res.payload, r.Location) {
		family, scope := windowFamily(w)
		period, minutes := windowPeriod(w.key, w.label)
		rolled := w.resetsAt != nil && *w.resetsAt < now
		mw := model.Window{
			Key:           w.key,
			Family:        family,
			Window:        period,
			Label:         w.label,
			UsedPercent:   w.pct,
			ResetsAt:      w.resetsAt,
			WindowMinutes: minutes,
			Scope:         scope,
			Blocking:      w.pct != nil && *w.pct >= 100 && !rolled,
		}
		s.Windows = append(s.Windows, mw)
		if rolled && w.pct != nil {
			s.Notes = append(s.Notes, model.Note{Severity: model.Info,
				Text: w.label + ": reading predates its reset (rolled over)"})
		}
		if !accountWide[w.key] {
			if mw.Blocking {
				s.Notes = append(s.Notes, model.Note{Severity: model.Warning,
					Text: fmt.Sprintf("%s at %s%%: blocks %s only", w.label, pyG(pyRound1(*w.pct)), family)})
			}
			continue
		}
		sawAccount = true
		switch {
		case w.pct == nil:
			unknown = true
		case mw.Blocking:
			blocked = true
			s.BlockedReasons = append(s.BlockedReasons, fmt.Sprintf("%s at %s%%", period, pyG(pyRound1(*w.pct))))
			if w.resetsAt != nil && (blockedUntil == nil || *w.resetsAt > *blockedUntil) {
				blockedUntil = model.F(*w.resetsAt)
			}
		}
	}
	switch {
	case blocked:
		s.Verdict = model.Blocked
		s.BlockedUntil = blockedUntil
	case !sawAccount || unknown:
		s.Verdict = model.Unknown
	default:
		s.Verdict = model.Usable
	}

	for _, w := range warnings {
		s.Notes = append(s.Notes, model.Note{Severity: model.Warning, Text: w})
	}
	if e := tok.expired(now); e != nil && *e {
		s.Notes = append(s.Notes, model.Note{Severity: model.Warning, Text: "OAuth token expired; " + loginHint})
	}
	if eu := res.payload.get("extra_usage"); extraUsageActive(eu) {
		sev := model.Info
		if eu.get("spend_limit_reached").truthy() {
			sev = model.Warning
		}
		s.Notes = append(s.Notes, model.Note{Severity: sev, Text: "Extra usage: " + extraUsageDetail(eu)})
	}
	if c := extractCloudCredit(res.payload, now, r.Location); c != nil && c.Available {
		text := fmt.Sprintf("Cloud credit: $%s of $%s left", pyMoney(c.RemainingDollars), pyMoney(c.LimitDollars))
		if c.ExpiresAt != nil {
			text += ", expires " + formatTimestamp(*c.ExpiresAt, r.Location)
		}
		if c.LockedReason != nil {
			text += " (locked: " + *c.LockedReason + ")"
		}
		s.Notes = append(s.Notes, model.Note{Severity: model.Info, Text: text})
	}
	if len(s.Windows) == 0 {
		s.Notes = append(s.Notes, model.Note{Severity: model.Warning, Text: "No usage windows reported"})
	}
	return s
}

// ModelFamily is the Paseo watcher's model_family: "claude-opus-4-1" gives
// "opus", "sonnet[1m]" gives "sonnet[1m]", "" gives "".
func ModelFamily(m string) string {
	m = strings.TrimPrefix(m, "claude-")
	first, _, _ := strings.Cut(m, "-")
	return strings.ToLower(first)
}

// ErrQuotaUnavailable is wrapped by every UsableFor error.
var ErrQuotaUnavailable = errors.New("claude quota unavailable")

// UsableFor is the old Paseo watcher's quota_usable for a Claude snapshot:
// usable when every applicable window is below 100%. The applicable windows
// are the account-wide ones plus, for a family, the model-scoped weekly
// windows (key weekly_scoped or seven_day_<family>) whose label names it.
// A snapshot without data, an applicable window without a number, or no
// applicable window at all is an error, never usable. Like the original,
// it does not discount rolled-over windows; callers that care read a fresh
// snapshot first.
func UsableFor(s model.Snapshot, family string) (bool, error) {
	if s.Source == "" || s.Verdict == model.SignedOut {
		msg := "no data"
		if s.Error != nil {
			msg = *s.Error
		}
		return false, fmt.Errorf("%w: Claude quota check failed: %s", ErrQuotaUnavailable, msg)
	}
	family = strings.ToLower(family)
	var pcts []float64
	for _, w := range s.Windows {
		applies := accountWide[w.Key] || (family != "" &&
			(w.Key == "weekly_scoped" || w.Key == "seven_day_"+family) &&
			strings.Contains(strings.ToLower(w.Label), family))
		if !applies {
			continue
		}
		if w.UsedPercent == nil {
			return false, fmt.Errorf("%w: Claude quota utilization unavailable", ErrQuotaUnavailable)
		}
		pcts = append(pcts, *w.UsedPercent)
	}
	if len(pcts) == 0 {
		return false, fmt.Errorf("%w: no applicable Claude quota window", ErrQuotaUnavailable)
	}
	for _, p := range pcts {
		if p >= 100 {
			return false, nil
		}
	}
	return true, nil
}
