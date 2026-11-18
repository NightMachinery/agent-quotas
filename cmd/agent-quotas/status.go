package main

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

// shortDuration renders d with at most two units ("1h 25m", "4d 7h", "12m").
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(math.Round(d.Seconds()))
	days, s := s/86400, s%86400
	hours, s := s/3600, s%3600
	mins := s / 60
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && mins > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	case mins > 0:
		return fmt.Sprintf("%dm", mins)
	}
	return fmt.Sprintf("%ds", s%60)
}

// writeStatus prints the compact account listing of `agent-quotas status`.
func writeStatus(w io.Writer, doc model.Document, now time.Time) {
	idWidth := 0
	for _, s := range doc.Accounts {
		if len(s.ID) > idWidth {
			idWidth = len(s.ID)
		}
	}
	for _, s := range doc.Accounts {
		head := fmt.Sprintf("%-*s  %-10s", idWidth, s.ID, s.Verdict)
		var meta []string
		if s.Label != "" && s.Label != s.Name {
			meta = append(meta, s.Label)
		}
		if s.Plan != nil && *s.Plan != "" {
			meta = append(meta, *s.Plan)
		}
		if s.Active != nil && *s.Active {
			meta = append(meta, "active")
		}
		src := s.Source
		if s.FetchedAt != nil {
			src += " " + shortDuration(now.Sub(model.Time(*s.FetchedAt))) + " ago"
		}
		if s.Stale {
			src += " (stale)"
		}
		if strings.TrimSpace(src) != "" {
			meta = append(meta, src)
		}
		if s.Verdict == model.Blocked && s.BlockedUntil != nil {
			meta = append(meta, "until "+model.Time(*s.BlockedUntil).Local().Format("Mon 15:04")+
				" (in "+shortDuration(model.Time(*s.BlockedUntil).Sub(now))+")")
		}
		fmt.Fprintln(w, strings.TrimRight(head+"  "+strings.Join(meta, "  "), " "))
		for _, win := range s.Windows {
			used := "   ?"
			if win.UsedPercent != nil {
				used = fmt.Sprintf("%3.0f%%", *win.UsedPercent)
			}
			reset := ""
			if win.ResetsAt != nil {
				reset = "resets in " + shortDuration(model.Time(*win.ResetsAt).Sub(now))
			}
			mark := " "
			if win.Blocking {
				mark = "!"
			}
			fmt.Fprintf(w, "  %s %-16s %-7s %s  %s\n", mark, clip(win.Family, 16), clip(win.Window, 7), used, reset)
		}
		if s.Error != nil {
			fmt.Fprintf(w, "  error: %s\n", *s.Error)
		}
		for _, n := range s.Notes {
			if n.Severity != model.Info {
				fmt.Fprintf(w, "  %s: %s\n", n.Severity, n.Text)
			}
		}
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
