// Package procx runs external commands with a timeout, in their own process
// group, so a command that hangs is killed together with its children.
package procx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Cmd describes one command.
type Cmd struct {
	Argv    []string
	Env     []string // full environment; nil means os.Environ()
	Dir     string
	Stdin   []byte
	Timeout time.Duration
}

// Result is what a command produced.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// ErrTimeout is returned when a command ran past its timeout.
var ErrTimeout = errors.New("command timed out")

// Runner runs commands; tests substitute fakes.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// Exec is the real Runner.
type Exec struct{}

// Run runs c. A non-zero exit is an *exec.ExitError wrapped with stderr; the
// Result is still filled in.
func (Exec) Run(ctx context.Context, c Cmd) (Result, error) {
	if len(c.Argv) == 0 {
		return Result{}, errors.New("empty command")
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Env = c.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Dir = c.Dir
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		pid := cmd.Process.Pid
		if pid > 1 && pid != os.Getpid() {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				<-done
			}
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = ErrTimeout
		} else {
			err = ctx.Err()
		}
	}
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil && !errors.Is(err, ErrTimeout) && !errors.Is(err, context.Canceled) {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		if msg != "" {
			err = fmt.Errorf("%s: %w: %s", c.Argv[0], err, msg)
		} else {
			err = fmt.Errorf("%s: %w", c.Argv[0], err)
		}
	}
	return res, err
}

// EnvWithout returns env without the named variables, and without any
// variable whose name starts with one of the prefixes.
func EnvWithout(env []string, names []string, prefixes []string) []string {
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if drop[name] {
			continue
		}
		skip := false
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return out
}

// EnvSet returns env with name set to value (replacing any previous value).
func EnvSet(env []string, name, value string) []string {
	out := EnvWithout(env, []string{name}, nil)
	return append(out, name+"="+value)
}

// AgentMarkers are the variables that name an agent session. Commands the
// daemon runs on behalf of a session must not inherit another session's.
var AgentMarkers = []string{
	"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID",
	"AI_AGENT", "AGENT_SESSION_REUSE_PANE", "AGENT_SESSION_STATE",
	"PASEO_AGENT_ID", "PASEO_HOST", "CLAUDE_CONFIG_DIR", "CODEX_HOME",
}
