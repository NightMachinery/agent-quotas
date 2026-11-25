package agy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/NightMachinery/agent-quotas/internal/model"
	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// usageRow is one row of `agy -p /usage`, normalized as h-agy-status-rows
// does. Remaining is what is LEFT, the opposite of a usage percent.
type usageRow struct {
	Group     string
	Label     string
	Remaining float64
	ResetsAt  int64
	ISO       string // upstream's own reset string
	BucketID  string
}

var (
	rePercent  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	reNonAlnum = regexp.MustCompile(`[^a-z0-9]`)
	reDashes   = regexp.MustCompile(`-{2,}`)
)

func slug(s string) string {
	s = reNonAlnum.ReplaceAllString(s, "-")
	s = reDashes.ReplaceAllString(s, "-")
	s = strings.TrimPrefix(s, "-")
	return strings.TrimSuffix(s, "-")
}

// BucketID is the cache bucket id of one /usage row: gemini-5h,
// gemini-weekly, 3p-5h, 3p-weekly for the family-wide pools, the group's
// slug plus the window for a model pool (gemini-4-argon-weekly), and a
// stable id derived from both strings for a row it does not recognize.
func BucketID(group, label string) string {
	g, l := strings.ToLower(group), strings.ToLower(label)
	var family, window string
	switch g {
	case "gemini models", "gemini":
		family = "gemini"
	case "claude and gpt models", "claude and gpt", "claude/gpt models":
		family = "3p"
	default:
		family = slug(g)
	}
	switch {
	case strings.Contains(l, "weekly"):
		window = "weekly"
	case strings.Contains(l, "5h") || strings.Contains(l, "hour"):
		window = "5h"
	}
	if family != "" && window != "" {
		return family + "-" + window
	}
	return slug(g + "-" + l)
}

// bucketDisplayNames is h-agy-status-bucket-names: the group and label the
// slow path would print for a cache bucket that does not carry its own.
func bucketDisplayNames(id string) (group, label string) {
	switch {
	case id == "gemini-5h" || id == "gemini-weekly":
		group = "Gemini Models"
	case id == "3p-5h" || id == "3p-weekly":
		group = "Claude and GPT models"
	case strings.HasSuffix(id, "-5h") || strings.HasSuffix(id, "-weekly"):
		group = zshCaps(strings.ReplaceAll(id[:strings.LastIndex(id, "-")], "-", " "))
	default:
		group = id
	}
	switch {
	case strings.HasSuffix(id, "-5h"):
		label = "5h Limit Remaining"
	case strings.HasSuffix(id, "-weekly"):
		label = "Weekly Limit Remaining"
	default:
		label = "Limit Remaining"
	}
	return group, label
}

// zshCaps is zsh's ${(C)...}: the first character of each word upper case,
// the rest lower case, where a word is a run of letters and digits.
func zshCaps(s string) string {
	var b strings.Builder
	prev := false
	for _, r := range s {
		alnum := unicode.IsLetter(r) || unicode.IsDigit(r)
		if alnum && !prev {
			b.WriteRune(unicode.ToUpper(r))
		} else if alnum {
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
		prev = alnum
	}
	return b.String()
}

// parseUsage reads `agy -p /usage`: tab-separated group, label, percent
// left and ISO reset per row. Banner and upsell lines are skipped, as is a
// row whose percent is not a number; a reset time that does not parse fails
// the whole read, as gdate did.
func parseUsage(out string) ([]usageRow, error) {
	var rows []usageRow
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		pct := strings.TrimSuffix(f[2], "%")
		if !rePercent.MatchString(pct) {
			continue
		}
		remaining, err := strconv.ParseFloat(pct, 64)
		if err != nil {
			continue
		}
		t, err := parseISO(f[3])
		if err != nil {
			return nil, fmt.Errorf("unreadable reset time %q: %w", f[3], err)
		}
		rows = append(rows, usageRow{
			Group: f[0], Label: f[1], Remaining: remaining,
			ResetsAt: t.Unix(), ISO: f[3], BucketID: BucketID(f[0], f[1]),
		})
	}
	return rows, nil
}

func parseISO(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02 15:04:05Z07:00", "2006-01-02T15:04:05.999999999Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("not an ISO-8601 time")
}

// slowError is a failed slow-path run, with the exit status the old report
// put into its error text.
type slowError struct {
	exit int
	err  error
}

func (e *slowError) Error() string { return e.err.Error() }
func (e *slowError) Unwrap() error { return e.err }

// runUsage runs `agy -p /usage` for one account and reads its rows.
func (r *Reader) runUsage(ctx context.Context, a Account) ([]usageRow, error) {
	out, err := r.runPrint(ctx, a, "/usage")
	if err != nil {
		return nil, err
	}
	rows, err := parseUsage(out)
	if err != nil {
		return nil, &slowError{1, err}
	}
	if len(rows) == 0 {
		return nil, &slowError{1, errors.New("no quota rows in `agy -p /usage'")}
	}
	return rows, nil
}

// runPrint runs one of agy's print-mode slash commands (/usage, /credits)
// for one account, as h-agy-status-run did: file-auth environment, the
// account's --gemini_dir and --app_data_dir, plan mode, no stdin, a fresh
// empty working directory (agy reads the project context of its cwd) and a
// timeout. It returns stdout without trailing newlines. A slash command
// starts no agent turn, so it spends no quota.
func (r *Reader) runPrint(ctx context.Context, a Account, slash string) (string, error) {
	tmp, err := os.MkdirTemp("", "agy-status-")
	if err != nil {
		return "", &slowError{1, err}
	}
	defer os.RemoveAll(tmp)
	timeout := time.Duration(r.cfg.Agy.TimeoutS) * time.Second
	res, err := r.run.Run(ctx, procx.Cmd{
		Argv:    a.PrintArgv(r.cfg.Agy.Binary, slash),
		Env:     r.usageEnv(a),
		Dir:     tmp,
		Timeout: timeout,
	})
	switch {
	case errors.Is(err, procx.ErrTimeout):
		return "", &slowError{124, &model.UpstreamError{Err: fmt.Errorf("`agy -p %s' timed out after %s", slash, timeout)}}
	case err != nil && res.ExitCode > 0:
		return "", &slowError{res.ExitCode, &model.UpstreamError{Err: fmt.Errorf("`agy -p %s' failed (%d)", slash, res.ExitCode)}}
	case err != nil:
		return "", &slowError{1, fmt.Errorf("`agy -p %s': %w", slash, err)}
	}
	return strings.TrimRight(string(res.Stdout), "\n"), nil
}

// PrintArgv is the command line of a print-mode slash command.
func (a Account) PrintArgv(binary, slash string) []string {
	return []string{binary, "--gemini_dir=" + a.GeminiDir, "--app_data_dir=antigravity-cli",
		"--mode", "plan", "--print", slash}
}

func (r *Reader) usageEnv(a Account) []string {
	env := os.Environ()
	for k, v := range r.cfg.Env {
		env = procx.EnvSet(env, k, v)
	}
	env = procx.EnvWithout(env, procx.AgentMarkers, nil)
	ssh := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "SSH_CONNECTION="); ok {
			ssh = v
		}
	}
	if ssh == "" {
		// The verified trigger for agy's file credential storage, scoped
		// to this child.
		ssh = "agy-file-auth"
	}
	env = procx.EnvSet(env, "ANTIGRAVITY_GEMINI_DIR", a.GeminiDir)
	env = procx.EnvSet(env, "AGY_ACCOUNT_ID", a.Name)
	env = procx.EnvSet(env, "AGY_ACCOUNT_LABEL", a.Label)
	env = procx.EnvSet(env, "agy_status_cache_file", a.CacheFile)
	return procx.EnvSet(env, "SSH_CONNECTION", ssh)
}
