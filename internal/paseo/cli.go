package paseo

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// ContinueText is the message sent to a resumed agent. Only a turn that
// failed on the limit is resumed, so the run was interrupted: there is no
// "turn yourself off if finished" clause, which agents read as "off once
// this batch is done".
const ContinueText = "Continue."

// Paseo is the access to the Paseo daemon the engine needs.
type Paseo interface {
	// Inspect is `paseo inspect <id> --home <home> --json`.
	Inspect(ctx context.Context, agentID, home string) (*Object, error)
	// Send is `paseo send <id> <text> --behavior <behavior> --home <home>
	// --no-wait --json`. Any error means the outcome is uncertain.
	Send(ctx context.Context, agentID, home, text, behavior string) error
}

// CLI is the real Paseo access through the paseo command.
type CLI struct {
	Runner  procx.Runner  // nil means procx.Exec
	Binary  string        // "" means "paseo"
	Timeout time.Duration // per command; 0 means 60 s, as the old tool
	// Env is the base environment; nil means os.Environ(). The agent
	// marker variables are always removed.
	Env []string
}

// Errors keep the command's output private: diagnostics of the native tools
// can contain credentials or names.
var (
	ErrCommandUnavailable = refuse("External command unavailable or timed out")
	ErrCommandFailed      = refuse("External command failed; its private output was withheld")
)

func (c CLI) run(ctx context.Context, argv []string) ([]byte, error) {
	runner := c.Runner
	if runner == nil {
		runner = procx.Exec{}
	}
	bin := c.Binary
	if bin == "" {
		bin = "paseo"
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	env = procx.EnvWithout(env, procx.AgentMarkers, nil)
	res, err := runner.Run(ctx, procx.Cmd{Argv: append([]string{bin}, argv...), Env: env, Timeout: timeout})
	if err != nil {
		if res.ExitCode > 0 && !errors.Is(err, procx.ErrTimeout) {
			return nil, ErrCommandFailed
		}
		return nil, ErrCommandUnavailable
	}
	if res.ExitCode != 0 {
		return nil, ErrCommandFailed
	}
	return res.Stdout, nil
}

// Inspect implements Paseo.
func (c CLI) Inspect(ctx context.Context, agentID, home string) (*Object, error) {
	out, err := c.run(ctx, []string{"inspect", agentID, "--home", home, "--json"})
	if err != nil {
		return nil, err
	}
	o, err := DecodeObject(out)
	if err != nil {
		return nil, refuse("paseo inspect returned unexpected JSON")
	}
	return o, nil
}

// Send implements Paseo.
func (c CLI) Send(ctx context.Context, agentID, home, text, behavior string) error {
	_, err := c.run(ctx, SendArgv(agentID, home, text, behavior))
	return err
}

// SendArgv is the paseo argv (without the binary) of one send.
func SendArgv(agentID, home, text, behavior string) []string {
	return []string{"send", agentID, text, "--behavior", behavior, "--home", home, "--no-wait", "--json"}
}

// SendBehavior is how `paseo send` treats an agent mid-turn: the configured
// value, else PASEO_HANDOFF_SEND_BEHAVIOR, else PASEO_SEND_BEHAVIOR, else
// "steer" (steer joins the turn, interrupt aborts it).
func SendBehavior(configured string, getenv func(string) string) string {
	if configured != "" {
		return configured
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, name := range []string{"PASEO_HANDOFF_SEND_BEHAVIOR", "PASEO_SEND_BEHAVIOR"} {
		if v := getenv(name); v != "" {
			return v
		}
	}
	return "steer"
}
