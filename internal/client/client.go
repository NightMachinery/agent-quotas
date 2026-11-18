// Package client talks to the agent-quotas daemon over its unix socket.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

// ErrDaemonDown means nothing answers on the socket.
var ErrDaemonDown = errors.New("daemon not running")

// APIError is an error answer from the daemon.
type APIError struct {
	Status int
	Msg    string
	Code   string
}

func (e *APIError) Error() string { return e.Msg }

// Client is a connection to one daemon socket.
type Client struct {
	Socket string
	hc     *http.Client
}

// New returns a client for the socket at path.
func New(path string, timeout time.Duration) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	return &Client{Socket: path, hc: &http.Client{Transport: tr, Timeout: timeout}}
}

// Do sends one request; body and out may be nil.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent-quotas"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("%w (socket %s)", ErrDaemonDown, c.Socket)
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &APIError{Status: resp.StatusCode, Msg: e.Error, Code: e.Code}
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], data...)
		return nil
	}
	return json.Unmarshal(data, out)
}

// Health returns the daemon's health document.
func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	return out, c.Do(ctx, http.MethodGet, "/v1/health", nil, &out)
}

// Accounts reads the selected accounts (ids or provider names; none = all).
func (c *Client) Accounts(ctx context.Context, selectors []string, refresh bool) (model.Document, error) {
	q := url.Values{}
	for _, s := range selectors {
		q.Add("id", s)
	}
	if refresh {
		q.Set("refresh", "1")
	}
	var doc model.Document
	path := "/v1/accounts"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return doc, c.Do(ctx, http.MethodGet, path, nil, &doc)
}

// Refresh forces a read of the selected accounts.
func (c *Client) Refresh(ctx context.Context, selectors []string) (model.Document, error) {
	var doc model.Document
	return doc, c.Do(ctx, http.MethodPost, "/v1/refresh", map[string]any{"ids": selectors}, &doc)
}

// Transitions returns the verdict transitions since the given epoch.
func (c *Client) Transitions(ctx context.Context, since float64) ([]model.Transition, error) {
	var out struct {
		Transitions []model.Transition `json:"transitions"`
	}
	err := c.Do(ctx, http.MethodGet, fmt.Sprintf("/v1/transitions?since=%f", since), nil, &out)
	return out.Transitions, err
}
