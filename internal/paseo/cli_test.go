package paseo

import (
	"context"
	"errors"
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
