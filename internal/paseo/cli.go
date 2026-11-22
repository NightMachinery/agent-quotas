package paseo

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/procx"
)

// ContinueText resumes a main agent whose last work turn failed on quota.
const ContinueText = "Continue."

// ParentLabel is the label Paseo puts on an agent another agent created.
const ParentLabel = "paseo.parent-agent-id"

// ParentOf is the agent that created the one whose stored record this is
// (its parentAgentId, else the parent label); "" for a main agent.
func ParentOf(record *Object) string {
	if record == nil {
		return ""
	}
	if s, ok := record.String("parentAgentId"); ok && s != "" {
		return s
	}
	if labels, ok := record.Object("labels"); ok {
		s, _ := labels.String(ParentLabel)
		return s
	}
	return ""
}

// ParentState is whether a subagent's parent can still be asked.
type ParentState int

const (
	ParentLive     ParentState = iota // in this Paseo home, not archived
	ParentArchived                    // a prompt would unarchive and wake it
	ParentMissing                     // deleted, or another daemon's agent
)

// ResumeText resumes an agent whose last work turn failed on quota. A
// subagent asks its parent before going on: while it was stopped, the parent
// may have handed its task to a replacement, done it itself or changed
// plans, and a child that simply continued would duplicate or undo that.
// A subagent whose parent is archived or gone is told not to message it
// (Paseo unarchives an agent a prompt is sent to) and to stop unless the
// work is clearly still wanted.
func ResumeText(parent string, state ParentState) string {
	if parent == "" {
		return ContinueText
	}
	const resumed = "Continue: your usage limit has reset, and agent-quotas resumed the turn it stopped. "
	switch state {
	case ParentArchived, ParentMissing:
		why := "has been archived since. Do not message it: a message would unarchive and wake it"
		if state == ParentMissing {
			why = "is not in this Paseo any more (deleted, or run by another daemon), so it cannot be asked"
		}
		return resumed + "Your parent agent (" + parent + ") " + why + ". Nobody may be waiting for " +
			"this work. First check whether it was finished or taken over elsewhere (recent commits, " +
			"the files you were changing). If it was, or you cannot tell whether it is still wanted, " +
			"stop without further changes. Either way, tell the user briefly what you found and what you did."
	}
	return resumed + "Your parent agent (" + parent + ") may have moved on while you were stopped: given your task " +
		"to another agent, done it itself, or changed its plan. Before doing more, re-coordinate: " +
		"message it (paseo send " + parent + ", or the send_agent_prompt tool) with where you stopped " +
		"and what you would do next, ask whether to go on, then end your turn and wait for its answer. " +
		"If it no longer needs this work, stop without further changes. If your work was already " +
		"complete, tell it so instead."
}

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

// Check failures can share a retry policy across callers. Send errors still
// follow each caller's existing uncertain-send handling.
type CheckRetry struct {
	delay time.Duration
	At    time.Time
}

// Failed schedules the next check and reports whether this starts a streak.
func (r *CheckRetry) Failed(now time.Time, interval time.Duration) bool {
	first := r.delay == 0
	if first {
		r.delay = min(20*time.Second, interval)
	} else {
		r.delay = min(2*r.delay, interval)
	}
	r.At = now.Add(r.delay)
	return first
}

type homeCommands struct {
	slots chan struct{}
	users int
}

var commandHomes = struct {
	sync.Mutex
	homes map[string]*homeCommands
}{homes: make(map[string]*homeCommands)}

// All CLI callers in this process share the same limits, including the
// keeper, HTTP handlers, and the auto-continue engine.
func commandSlot(ctx context.Context, home string) (func(), error) {
	home = realpath(home)
	commandHomes.Lock()
	gate := commandHomes.homes[home]
	if gate == nil {
		gate = &homeCommands{slots: make(chan struct{}, 2)}
		commandHomes.homes[home] = gate
	}
	gate.users++
	commandHomes.Unlock()

	done := func() {
		commandHomes.Lock()
		gate.users--
		if gate.users == 0 {
			delete(commandHomes.homes, home)
		}
		commandHomes.Unlock()
	}
	select {
	case gate.slots <- struct{}{}:
		return func() { <-gate.slots; done() }, nil
	case <-ctx.Done():
		done()
		return nil, ErrCommandUnavailable
	}
}

func (c CLI) run(ctx context.Context, home string, argv []string) ([]byte, error) {
	release, err := commandSlot(ctx, home)
	if err != nil {
		return nil, err
	}
	defer release()
	if ctx.Err() != nil {
		return nil, ErrCommandUnavailable
	}
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
	out, err := c.run(ctx, home, []string{"inspect", agentID, "--home", home, "--json"})
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
	_, err := c.run(ctx, home, SendArgv(agentID, home, text, behavior))
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

// Logs is `paseo logs <id> --json --since <RFC 3339> --home <home>`: the
// timeline items since that time.
func (c CLI) Logs(ctx context.Context, agentID, home string, since time.Time) ([]*Object, error) {
	out, err := c.run(ctx, home, []string{"logs", agentID, "--json", "--since", since.UTC().Format(time.RFC3339Nano), "--home", home})
	if err != nil {
		return nil, err
	}
	o, err := DecodeObject(out)
	if err != nil {
		return nil, refuse("paseo logs returned unexpected JSON")
	}
	items, _ := o.Value("items").([]any)
	var res []*Object
	for _, it := range items {
		if obj, ok := it.(*Object); ok {
			res = append(res, obj)
		}
	}
	return res, nil
}

// List is `paseo ls -g --json --home <home>`: the non-archived agents of
// every directory.
func (c CLI) List(ctx context.Context, home string) ([]*Object, error) {
	out, err := c.run(ctx, home, []string{"ls", "-g", "--json", "--home", home})
	if err != nil {
		return nil, err
	}
	v, err := Decode(out)
	if err != nil {
		return nil, refuse("paseo ls returned unexpected JSON")
	}
	list, ok := v.([]any)
	if !ok {
		return nil, refuse("paseo ls returned unexpected JSON")
	}
	var res []*Object
	for _, it := range list {
		if obj, ok := it.(*Object); ok {
			res = append(res, obj)
		}
	}
	return res, nil
}
