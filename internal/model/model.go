// Package model holds the types every other package shares: the snapshot of
// one account's quota, its windows, the verdict, and the reader interface the
// daemon polls. docs/design.md is the contract these types follow.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion is the version of every API document. Additive changes keep
// it; renaming or removing a field bumps it.
const SchemaVersion = 1

// Provider names.
const (
	Claude = "claude"
	Codex  = "codex"
	Agy    = "agy"
)

// Verdict values.
const (
	Usable    = "usable"
	Blocked   = "blocked"
	Unknown   = "unknown"
	SignedOut = "signed_out"
)

// Note severities.
const (
	Info    = "info"
	Warning = "warning"
	Alarm   = "alarm"
)

// Window scopes.
const (
	ScopeAccount = "account"
	ScopeModel   = "model"
	ScopeReserve = "reserve"
)

// Note is one line of extra information about an account.
type Note struct {
	Severity string `json:"severity"`
	Text     string `json:"text"`
}

// Window is one usage window of an account.
type Window struct {
	Key           string   `json:"key"`
	Family        string   `json:"family"`
	Window        string   `json:"window"`
	Label         string   `json:"label"`
	UsedPercent   *float64 `json:"used_percent"`
	ResetsAt      *float64 `json:"resets_at"`
	WindowMinutes *float64 `json:"window_minutes"`
	Scope         string   `json:"scope"`
	Blocking      bool     `json:"blocking"`
}

// PollInfo is the poller's bookkeeping for one account.
type PollInfo struct {
	LastAttemptAt       *float64 `json:"last_attempt_at"`
	LastSuccessAt       *float64 `json:"last_success_at"`
	NextPollAt          *float64 `json:"next_poll_at"`
	BackoffUntil        *float64 `json:"backoff_until"`
	ConsecutiveFailures int      `json:"consecutive_failures"`
	LastHTTPStatus      int      `json:"last_http_status,omitempty"`
	UpstreamCalls       int      `json:"upstream_calls"`
}

// Snapshot is the latest reading of one account.
type Snapshot struct {
	ID             string          `json:"id"`
	Provider       string          `json:"provider"`
	Name           string          `json:"name"`
	Label          string          `json:"label"`
	Verdict        string          `json:"verdict"`
	BlockedReasons []string        `json:"blocked_reasons"`
	BlockedUntil   *float64        `json:"blocked_until"`
	Plan           *string         `json:"plan"`
	Email          *string         `json:"email"`
	Active         *bool           `json:"active"`
	Windows        []Window        `json:"windows"`
	Notes          []Note          `json:"notes"`
	FetchedAt      *float64        `json:"fetched_at"`
	Source         string          `json:"source"`
	Stale          bool            `json:"stale"`
	StaleReason    *string         `json:"stale_reason"`
	Error          *string         `json:"error"`
	Native         json.RawMessage `json:"native,omitempty"`
	Poll           PollInfo        `json:"poll"`
}

// Document is the answer to a read of several accounts.
type Document struct {
	SchemaVersion int        `json:"schema_version"`
	GeneratedAt   float64    `json:"generated_at"`
	Accounts      []Snapshot `json:"accounts"`
}

// OneDocument is the answer to a read of one account.
type OneDocument struct {
	SchemaVersion int      `json:"schema_version"`
	GeneratedAt   float64  `json:"generated_at"`
	Account       Snapshot `json:"account"`
}

// Transition records a change of an account's verdict.
type Transition struct {
	At       float64  `json:"at"`
	ID       string   `json:"id"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Reasons  []string `json:"reasons,omitempty"`
	ResetsAt *float64 `json:"blocked_until,omitempty"`
}

// AccountRef names one account a reader serves.
type AccountRef struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Name     string `json:"name"`
	// Extra carries reader-specific locator data (config dir, auth file,
	// gemini dir); readers own its meaning.
	Extra map[string]string `json:"extra,omitempty"`
}

// ReadOptions tune one read.
type ReadOptions struct {
	// Force asks for an upstream read (Antigravity: the slow path).
	Force bool
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Clock returns the read's clock.
func (o ReadOptions) Clock() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Reader is one provider's quota reader. The daemon owns scheduling,
// caching, coalescing and back-off; a reader only reads.
type Reader interface {
	Provider() string
	// Accounts lists the accounts this reader serves. It may do cheap local
	// discovery (auth snapshot files) but never contacts upstream.
	Accounts(ctx context.Context) ([]AccountRef, error)
	// Read reads one account. A Snapshot with Error set and usable data
	// (from a cache) is a partial success; a returned error means nothing
	// usable. An *UpstreamError lets the daemon back off correctly.
	Read(ctx context.Context, ref AccountRef, opts ReadOptions) (Snapshot, error)
}

// UpstreamError is a failed upstream call the poller should back off from.
type UpstreamError struct {
	Status     int           // HTTP status, 0 for transport errors
	RetryAfter time.Duration // from a Retry-After header, 0 when absent
	Err        error
}

func (e *UpstreamError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("upstream HTTP %d: %v", e.Status, e.Err)
	}
	return fmt.Sprintf("upstream: %v", e.Err)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// RateLimited reports whether err is an upstream 429.
func RateLimited(err error) bool {
	var up *UpstreamError
	return errors.As(err, &up) && up.Status == 429
}

// ErrUnknownAccount is returned for an account id no reader serves.
var ErrUnknownAccount = errors.New("unknown account")

// ID builds an account id.
func ID(provider, name string) string { return provider + ":" + name }

// SplitID splits an account id into provider and name.
func SplitID(id string) (provider, name string, ok bool) {
	provider, name, ok = strings.Cut(id, ":")
	return provider, name, ok && provider != "" && name != ""
}

// F returns a pointer to v, for the optional numeric fields.
func F(v float64) *float64 { return &v }

// S returns a pointer to v, for the optional string fields.
func S(v string) *string { return &v }

// B returns a pointer to v.
func B(v bool) *bool { return &v }

// Unix converts a time to the epoch seconds the schema uses.
func Unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// Time converts epoch seconds back.
func Time(epoch float64) time.Time {
	sec := int64(epoch)
	return time.Unix(sec, int64((epoch-float64(sec))*1e9))
}

// Blocking returns the windows marked blocking.
func (s Snapshot) Blocking() []Window {
	var out []Window
	for _, w := range s.Windows {
		if w.Blocking {
			out = append(out, w)
		}
	}
	return out
}
