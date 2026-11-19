package paseo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/procx"
)

type fakeRunner struct {
	cmds []procx.Cmd
	res  procx.Result
	err  error
}

func (f *fakeRunner) Run(_ context.Context, c procx.Cmd) (procx.Result, error) {
	f.cmds = append(f.cmds, c)
	return f.res, f.err
}

func TestCLISendArgvAndCleanEnv(t *testing.T) {
	r := &fakeRunner{}
	env := []string{"PATH=/usr/bin", "PASEO_AGENT_ID=00000000-0000-4000-8000-00000000000f",
		"PASEO_HOST=example.test", "CLAUDE_CONFIG_DIR=/tmp/x", "CODEX_HOME=/tmp/y", "KEEP=1"}
	c := CLI{Runner: r, Binary: "paseo-test", Env: env}
	if err := c.Send(context.Background(), "agent-id", "/tmp/example-home", ContinueText, "steer"); err != nil {
		t.Fatal(err)
	}
	cmd := r.cmds[0]
	want := []string{"paseo-test", "send", "agent-id", "Continue.", "--behavior", "steer", "--home", "/tmp/example-home", "--no-wait", "--json"}
	if !reflect.DeepEqual(cmd.Argv, want) {
		t.Fatal(cmd.Argv)
	}
	if !reflect.DeepEqual(cmd.Env, []string{"PATH=/usr/bin", "KEEP=1"}) {
		t.Fatal(cmd.Env)
	}
	if cmd.Timeout != 60*time.Second {
		t.Fatal(cmd.Timeout)
	}
}

func TestCLIInspect(t *testing.T) {
	r := &fakeRunner{res: procx.Result{Stdout: []byte(`{"Id": "agent-id", "Status": "idle", "PendingPermissions": []}`)}}
	c := CLI{Runner: r, Env: []string{}}
	snap, err := c.Inspect(context.Background(), "agent-id", "/tmp/example-home")
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := snap.String("Status"); s != "idle" {
		t.Fatal(s)
	}
	if want := []string{"paseo", "inspect", "agent-id", "--home", "/tmp/example-home", "--json"}; !reflect.DeepEqual(r.cmds[0].Argv, want) {
		t.Fatal(r.cmds[0].Argv)
	}
	r.res.Stdout = []byte(`[1]`)
	if _, err := c.Inspect(context.Background(), "agent-id", "/tmp/example-home"); err == nil {
		t.Fatal("non-object accepted")
	}
}

func TestCLIErrorsWithholdOutput(t *testing.T) {
	r := &fakeRunner{res: procx.Result{ExitCode: 2, Stderr: []byte("secret detail")}, err: errors.New("paseo: exit status 2: secret detail")}
	c := CLI{Runner: r, Env: []string{}}
	err := c.Send(context.Background(), "a", "/h", ContinueText, "steer")
	if err != ErrCommandFailed || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
	r.res, r.err = procx.Result{ExitCode: -1}, procx.ErrTimeout
	if err := c.Send(context.Background(), "a", "/h", ContinueText, "steer"); err != ErrCommandUnavailable {
		t.Fatal(err)
	}
	r.res, r.err = procx.Result{}, errors.New("exec: not found")
	if err := c.Send(context.Background(), "a", "/h", ContinueText, "steer"); err != ErrCommandUnavailable {
		t.Fatal(err)
	}
}

type runnerFunc func(context.Context, procx.Cmd) (procx.Result, error)

func (run runnerFunc) Run(ctx context.Context, cmd procx.Cmd) (procx.Result, error) {
	return run(ctx, cmd)
}

func TestCLICommandsShareTwoSlotsPerHome(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	release := make(chan struct{})
	defer cancel()
	defer close(release)
	starts := make(chan string, 12)
	fake := runnerFunc(func(ctx context.Context, c procx.Cmd) (procx.Result, error) {
		starts <- c.Argv[1]
		select {
		case <-release:
		case <-ctx.Done():
			return procx.Result{}, ctx.Err()
		}
		return procx.Result{Stdout: []byte(`[]`)}, nil
	})
	home := t.TempDir()
	otherHome := t.TempDir()
	a, b := CLI{Runner: fake}, CLI{Runner: fake}
	call := func(ctx context.Context, gotHome string, method string) error {
		switch method {
		case "list":
			_, err := a.List(ctx, gotHome)
			return err
		case "inspect":
			_, err := b.Inspect(ctx, "synthetic", gotHome)
			return err
		default:
			return a.Send(ctx, "synthetic", gotHome, "sentinel", "steer")
		}
	}
	waitStart := func(want string) {
		t.Helper()
		select {
		case got := <-starts:
			if got != want {
				t.Fatalf("started %s, want %s", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("%s never started", want)
		}
	}
	go func() { _ = call(ctx, home, "list") }()
	waitStart("ls")
	go func() { _ = call(ctx, home, "inspect") }()
	waitStart("inspect")
	queued, stop := context.WithTimeout(ctx, 40*time.Millisecond)
	defer stop()
	if err := call(queued, home, "send"); !errors.Is(err, ErrCommandUnavailable) {
		t.Fatalf("third command returned %v", err)
	}
	go func() { _ = call(ctx, otherHome, "send") }()
	waitStart("send")
}

func TestCLIHealthyAgentCompletesWhileAnotherTimesOut(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	fake := filepath.Join(dir, "paseo")
	const script = `#!/bin/sh
if [ "$2" = slow ]; then
    : > "$AQ_TEST_PASEO_STARTED"
    sleep 5
else
    printf '%s\n' '{"Id":"healthy","Status":"idle"}'
fi
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	c := CLI{Binary: fake, Timeout: 500 * time.Millisecond, Env: append(os.Environ(), "AQ_TEST_PASEO_STARTED="+started)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	slow := make(chan error, 1)
	go func() {
		_, err := c.Inspect(ctx, "slow", dir)
		slow <- err
	}()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("slow command never started")
		case <-time.After(5 * time.Millisecond):
		}
	}
	snapshot, err := c.Inspect(ctx, "healthy", dir)
	if id, _ := snapshot.String("Id"); err != nil || id != "healthy" {
		t.Fatalf("healthy command: id %q, error %v", id, err)
	}
	if err := <-slow; !errors.Is(err, ErrCommandUnavailable) {
		t.Fatalf("slow command returned %v", err)
	}
}
