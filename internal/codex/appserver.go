package codex

// The codex app-server session of codex_status.py: initialize, initialized,
// account/read (best effort, short deadline), account/rateLimits/read.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Conn is one running app-server: newline-delimited JSON-RPC on stdin and
// stdout.
type Conn interface {
	// Send writes one line (the newline is added).
	Send(line []byte) error
	// Lines delivers stdout lines; it is closed when stdout ends.
	Lines() <-chan []byte
	// Stop terminates the server and returns its stderr.
	Stop() string
}

// Launcher starts app-servers; tests substitute a fake.
type Launcher interface {
	Launch(ctx context.Context, argv, env []string) (Conn, error)
}

// ErrCodexNotFound is returned when the codex binary cannot be found.
var ErrCodexNotFound = errors.New("codex-status: codex not found in PATH")

// statusError is codex_status.CodexStatusError: a message and the server's
// stderr.
type statusError struct {
	msg     string
	stderr  string
	timeout bool
}

func (e *statusError) Error() string { return e.msg }

type sessionResult struct {
	Init    *obj
	Account *obj
	Rate    *obj
}

// sessionTimeouts are the deadlines of one session.
func sessionDeadlines(start time.Time, timeout time.Duration) (overall, account time.Time) {
	if timeout < time.Second {
		timeout = time.Second
	}
	overall = start.Add(timeout)
	acc := timeout / 3
	if acc > 5*time.Second {
		acc = 5 * time.Second
	}
	if acc < time.Second {
		acc = time.Second
	}
	account = start.Add(acc)
	if account.After(overall) {
		account = overall
	}
	return overall, account
}

func sendJSON(c Conn, v *obj) error {
	return c.Send(dumpTight(v))
}

// idString is Python's str(msg.get("id")).
func idString(v any) string { return pyStr(v) }

// recvResponse waits for the response with the wanted id, skipping other
// lines, until the deadline or the end of stdout.
func recvResponse(c Conn, wanted string, deadline time.Time) *obj {
	lines := c.Lines()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		timer := time.NewTimer(remaining)
		select {
		case line, ok := <-lines:
			timer.Stop()
			if !ok {
				return nil
			}
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			v, err := parseJSON(line)
			if err != nil {
				continue
			}
			msg, isObj := v.(*obj)
			if !isObj {
				continue
			}
			if idString(msg.V("id")) == wanted {
				return msg
			}
		case <-timer.C:
			return nil
		}
	}
}

func rpcRequest(c Conn, method, id string, deadline time.Time, params any, includeParams bool) (*obj, error) {
	req := newObj().Set("method", method).Set("id", id)
	if includeParams {
		req.Set("params", params)
	}
	if err := sendJSON(c, req); err != nil {
		return nil, &statusError{msg: "codex-status: " + method + " failed: " + err.Error()}
	}
	resp := recvResponse(c, id, deadline)
	if resp == nil {
		return nil, &statusError{msg: "codex-status: " + method + " timed out", timeout: true}
	}
	if resp.Has("error") {
		return nil, &statusError{msg: "codex-status: " + method + " failed: " + dumpCompact(resp.V("error"))}
	}
	res, _ := resp.V("result").(*obj)
	if res == nil {
		res = newObj()
	}
	return res, nil
}

// runSession runs one app-server session, as run_codex_session does.
func runSession(ctx context.Context, l Launcher, argv, env []string, timeout time.Duration) (sessionResult, error) {
	overall, accountDeadline := sessionDeadlines(time.Now(), timeout)
	conn, err := l.Launch(ctx, argv, env)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return sessionResult{}, &statusError{msg: ErrCodexNotFound.Error()}
		}
		return sessionResult{}, &statusError{msg: "codex-status: " + err.Error()}
	}
	res, err := func() (sessionResult, error) {
		init := newObj().Set("method", "initialize").Set("id", "1").Set("params", newObj().
			Set("clientInfo", newObj().Set("name", "codex-status").Set("version", "1")).
			Set("capabilities", newObj().Set("experimentalApi", true)))
		if err := sendJSON(conn, init); err != nil {
			return sessionResult{}, &statusError{msg: "codex-status: initialize failed: " + err.Error()}
		}
		resp := recvResponse(conn, "1", overall)
		if resp == nil {
			return sessionResult{}, &statusError{msg: "codex-status: initialize timed out", timeout: true}
		}
		if resp.Has("error") {
			return sessionResult{}, &statusError{msg: "codex-status: initialize failed: " + dumpCompact(resp.V("error"))}
		}
		initResult, _ := resp.V("result").(*obj)
		if initResult == nil {
			initResult = newObj()
		}
		if err := sendJSON(conn, newObj().Set("method", "initialized")); err != nil {
			return sessionResult{}, &statusError{msg: "codex-status: initialized failed: " + err.Error()}
		}
		account, err := rpcRequest(conn, "account/read", "2", accountDeadline, newObj().Set("refreshToken", false), true)
		if err != nil {
			account = newObj() // best effort, as try_rpc_request
		}
		rate, err := rpcRequest(conn, "account/rateLimits/read", "3", overall, nil, true)
		if err != nil {
			return sessionResult{}, err
		}
		return sessionResult{Init: initResult, Account: account, Rate: rate}, nil
	}()
	stderr := pyStrip(conn.Stop())
	if err != nil {
		var se *statusError
		if errors.As(err, &se) {
			se.stderr = stderr
		}
		return sessionResult{}, err
	}
	return res, nil
}

// lookPathIn resolves name against the PATH in env (falling back to the
// process's own PATH), so a configured PATH is honoured.
func lookPathIn(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	path := os.Getenv("PATH")
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// ExecLauncher starts the real codex app-server in its own process group.
type ExecLauncher struct{}

// Launch starts argv with env.
func (ExecLauncher) Launch(ctx context.Context, argv, env []string) (Conn, error) {
	bin, err := lookPathIn(argv[0], env)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &execConn{cmd: cmd, stdin: stdin, lines: make(chan []byte, 64), done: make(chan struct{}), exited: make(chan struct{})}
	cmd.Stderr = &c.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go c.readLoop(stdout)
	go func() {
		c.waitErr = cmd.Wait()
		close(c.exited)
	}()
	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-c.done:
		}
	}()
	return c, nil
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type execConn struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stderr   lockedBuffer
	lines    chan []byte
	done     chan struct{}
	exited   chan struct{}
	waitErr  error
	stopOnce sync.Once
	out      string
}

func (c *execConn) readLoop(r io.Reader) {
	defer close(c.lines)
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			select {
			case c.lines <- line:
			case <-c.done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *execConn) Send(line []byte) error {
	_, err := c.stdin.Write(append(append([]byte(nil), line...), '\n'))
	return err
}

func (c *execConn) Lines() <-chan []byte { return c.lines }

// Stop sends SIGTERM to the process group, waits 1.5 s, then SIGKILL and
// another second, as the Python's terminate/communicate/kill sequence.
func (c *execConn) Stop() string {
	c.stopOnce.Do(func() {
		close(c.done)
		_ = c.stdin.Close()
		pid := c.cmd.Process.Pid
		if pid > 1 && pid != os.Getpid() {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
		select {
		case <-c.exited:
		case <-time.After(1500 * time.Millisecond):
			if pid > 1 && pid != os.Getpid() {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
			select {
			case <-c.exited:
			case <-time.After(time.Second):
			}
		}
		c.out = c.stderr.String()
	})
	return c.out
}
