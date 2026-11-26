package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/agy"
	"github.com/NightMachinery/agent-quotas/internal/config"
	"github.com/NightMachinery/agent-quotas/internal/daemon"
	"github.com/NightMachinery/agent-quotas/internal/model"
)

// serve runs the daemon: store, pollers, API and the auto-continue engines.
func serve(ctx context.Context, cfg *config.Config, socket string) error {
	if err := daemon.EnsurePrivateDir(config.StateDir()); err != nil {
		return err
	}
	logger := log.New(os.Stderr, "agent-quotas: ", log.LstdFlags)
	readers := buildReaders(cfg)
	for _, r := range readers {
		if a, ok := r.(*agy.Reader); ok {
			a.StaleRefresh = true // idle accounts' readings are re-read hourly
		}
	}
	d := daemon.New(cfg, readers, daemon.Options{Logger: logger})
	srv := daemon.NewServer(d)
	engines, err := attachEngines(ctx, cfg, d, srv, logger)
	if err != nil {
		return err
	}
	l, err := daemon.Listen(socket)
	if err != nil {
		return err
	}
	logger.Printf("version %s listening on %s", version, socket)
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ctx, l) }()
	go func() { errc <- d.Run(ctx) }()
	if engines != nil {
		go engines(ctx)
	}
	err = <-errc
	if ctx.Err() != nil {
		logger.Printf("stopping: %v", ctx.Err())
		return nil
	}
	return err
}

// directDocument reads the selected accounts in this process, without the
// daemon: no cache, no coalescing.
func directDocument(ctx context.Context, cfg *config.Config, selectors []string, force bool) (model.Document, error) {
	return readWith(ctx, buildReaders(cfg), selectors, force)
}

// readWith reads the selected accounts of readers in this process.
func readWith(ctx context.Context, readers []model.Reader, selectors []string, force bool) (model.Document, error) {
	var refs []model.AccountRef
	byProvider := map[string]model.Reader{}
	for _, r := range readers {
		byProvider[r.Provider()] = r
		got, err := r.Accounts(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-quotas: %s: %v\n", r.Provider(), err)
			continue
		}
		refs = append(refs, got...)
	}
	var chosen []model.AccountRef
	for _, ref := range refs {
		if len(selectors) == 0 {
			chosen = append(chosen, ref)
			continue
		}
		for _, sel := range selectors {
			if sel == ref.ID || sel == ref.Provider {
				chosen = append(chosen, ref)
				break
			}
		}
	}
	if len(chosen) == 0 && len(selectors) > 0 {
		return model.Document{}, fmt.Errorf("%w: %v", model.ErrUnknownAccount, selectors)
	}
	out := make([]model.Snapshot, len(chosen))
	var wg sync.WaitGroup
	for i, ref := range chosen {
		wg.Add(1)
		go func(i int, ref model.AccountRef) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			snap, err := byProvider[ref.Provider].Read(rctx, ref, model.ReadOptions{Force: force})
			snap.ID, snap.Provider, snap.Name = ref.ID, ref.Provider, ref.Name
			if snap.Label == "" {
				snap.Label = ref.Name
			}
			if snap.Verdict == "" {
				snap.Verdict = model.Unknown
			}
			if err != nil && snap.Error == nil {
				snap.Error = model.S(err.Error())
			}
			out[i] = snap
		}(i, ref)
	}
	wg.Wait()
	return model.Document{SchemaVersion: model.SchemaVersion, GeneratedAt: model.Unix(time.Now()), Accounts: out}, nil
}
