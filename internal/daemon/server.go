package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

// ErrorBody is the JSON body of every error answer.
type ErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// Server is the unix-socket API in front of a Daemon.
type Server struct {
	d   *Daemon
	mux *http.ServeMux
}

// NewServer builds the API; more routes can be added with Handle.
func NewServer(d *Daemon) *Server {
	s := &Server{d: d, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /v1/health", s.health)
	s.mux.HandleFunc("GET /v1/accounts", s.accounts)
	s.mux.HandleFunc("GET /v1/accounts/{id}", s.account)
	s.mux.HandleFunc("POST /v1/refresh", s.refresh)
	s.mux.HandleFunc("GET /v1/transitions", s.transitions)
	return s
}

// Handle adds a route (Go 1.22 pattern syntax).
func (s *Server) Handle(pattern string, h http.HandlerFunc) { s.mux.HandleFunc(pattern, h) }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// WriteJSON writes v with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// WriteError writes an error body; the status follows from err.
func WriteError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, model.ErrUnknownAccount):
		status, code = http.StatusNotFound, "unknown_account"
	case errors.Is(err, ErrBadRequest):
		status, code = http.StatusBadRequest, "bad_request"
	case errors.Is(err, ErrBackedOff):
		status, code = http.StatusServiceUnavailable, "backed_off"
	}
	WriteJSON(w, status, ErrorBody{Error: err.Error(), Code: code})
}

// ErrBadRequest marks a malformed request.
var ErrBadRequest = errors.New("bad request")

// ReadJSON decodes a request body into v (at most 1 MiB).
func ReadJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	return nil
}

func truthy(v string) bool {
	switch v {
	case "1", "true", "y", "yes":
		return true
	}
	return false
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, s.d.Health())
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	selectors := append(q["id"], q["provider"]...)
	ids, err := s.d.Resolve(selectors)
	if err != nil {
		WriteError(w, err)
		return
	}
	snaps, err := s.d.GetMany(r.Context(), ids, truthy(q.Get("refresh")))
	if err != nil {
		WriteError(w, err)
		return
	}
	if snaps == nil {
		snaps = []model.Snapshot{}
	}
	WriteJSON(w, http.StatusOK, model.Document{SchemaVersion: model.SchemaVersion,
		GeneratedAt: model.Unix(s.d.now()), Accounts: snaps})
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap, err := s.d.Get(r.Context(), id, truthy(r.URL.Query().Get("refresh")))
	if err != nil && !errors.Is(err, ErrBackedOff) {
		WriteError(w, err)
		return
	}
	if err != nil && snap.Error == nil {
		snap.Error = model.S(err.Error())
	}
	WriteJSON(w, http.StatusOK, model.OneDocument{SchemaVersion: model.SchemaVersion,
		GeneratedAt: model.Unix(s.d.now()), Account: snap})
}

// RefreshRequest is the body of POST /v1/refresh.
type RefreshRequest struct {
	IDs []string `json:"ids"`
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	if err := ReadJSON(r, &req); err != nil {
		WriteError(w, err)
		return
	}
	ids, err := s.d.Resolve(req.IDs)
	if err != nil {
		WriteError(w, err)
		return
	}
	snaps, err := s.d.GetMany(r.Context(), ids, true)
	if err != nil {
		WriteError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, model.Document{SchemaVersion: model.SchemaVersion,
		GeneratedAt: model.Unix(s.d.now()), Accounts: snaps})
}

func (s *Server) transitions(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseFloat(r.URL.Query().Get("since"), 64)
	ts, err := s.d.Transitions(since)
	if err != nil {
		WriteError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"schema_version": model.SchemaVersion, "transitions": ts})
}

// EnsurePrivateDir creates dir (0700) and refuses one that is not a
// directory owned by this user or that others can access.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s: not a directory owned by this user", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s: group or others have access (mode %o); run chmod 700", dir, info.Mode().Perm())
	}
	return nil
}

// Listen opens the unix socket at path with mode 0600. A stale socket file
// (nothing answers on it) is removed; a live one means another daemon runs.
func Listen(path string) (net.Listener, error) {
	if err := EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("another agent-quotas daemon is answering on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	old := syscall.Umask(0o177)
	l, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Serve runs the API on l until ctx ends.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
