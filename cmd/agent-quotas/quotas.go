package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/model"
)

// snapshotFunc returns an account's snapshot, refreshed first when the
// cached reading is older than maxAge (the daemon), or simply the cached one
// (the CLI, through the socket).
type snapshotFunc func(ctx context.Context, id string, maxAge time.Duration) (model.Snapshot, error)

// quotaAdapter answers "is this seat usable" for both auto-continue engines
// from snapshots, with each provider's own rule.
type quotaAdapter struct {
	cfg  *config.Config
	snap snapshotFunc
	// maxAge is how old a reading may be before an engine acts on it.
	maxAge time.Duration
}

func (q quotaAdapter) get(ctx context.Context, id string) (model.Snapshot, error) {
	s, err := q.snap(ctx, id, q.maxAge)
	if err != nil {
		return s, err
	}
	if s.Error != nil && s.Stale {
		// A stale reading is not proof of quota: postpone.
		return s, fmt.Errorf("%s: reading is stale: %s", id, *s.Error)
	}
	return s, nil
}

// Claude implements paseo.Quotas.
func (q quotaAdapter) Claude(ctx context.Context, profile, family string) (bool, error) {
	s, err := q.get(ctx, model.ID(model.Claude, profile))
	if err != nil {
		return false, err
	}
	return claudeUsableFor(s, family)
}

// Codex implements paseo.Quotas: the active auth of that CODEX_HOME.
func (q quotaAdapter) Codex(ctx context.Context, codexHome string) (bool, error) {
	id, err := q.codexAccount(codexHome)
	if err != nil {
		return false, err
	}
	s, err := q.get(ctx, id)
	if err != nil {
		return false, err
	}
	return codexUsableFor(s)
}

// Agy implements paseo.Quotas.
func (q quotaAdapter) Agy(ctx context.Context, account, modelID string) (bool, error) {
	s, err := q.get(ctx, model.ID(model.Agy, account))
	if err != nil {
		return false, err
	}
	return agyUsableFor(s, modelID)
}

// codexAccount maps a CODEX_HOME to the account id of its active auth.
func (q quotaAdapter) codexAccount(codexHome string) (string, error) {
	want := canonical(codexHome)
	for _, h := range q.cfg.Codex.Homes {
		if canonical(config.Expand(h.CodexHome)) != want {
			continue
		}
		alias, err := codexActiveAlias(config.Expand(h.CodexHome))
		if err != nil {
			return "", err
		}
		if h.Name == "default" {
			return model.ID(model.Codex, alias), nil
		}
		return model.ID(model.Codex, h.Name+"/"+alias), nil
	}
	return "", fmt.Errorf("CODEX_HOME %s is not configured in agent-quotas", codexHome)
}

func canonical(p string) string {
	p = config.Expand(p)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return filepath.Clean(p)
}

// daemonSnapshots is the CLI's snapshotFunc: the daemon's cached reading.
func daemonSnapshots(ctx context.Context, id string, _ time.Duration) (model.Snapshot, error) {
	doc, err := newClient(2*time.Minute).Accounts(ctx, []string{id}, false)
	if err != nil {
		return model.Snapshot{}, err
	}
	if len(doc.Accounts) != 1 {
		return model.Snapshot{}, fmt.Errorf("%w: %s", model.ErrUnknownAccount, id)
	}
	return doc.Accounts[0], nil
}

func warnStderr(text string) { fmt.Fprintln(os.Stderr, text) }
