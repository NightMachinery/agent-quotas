package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NightMachinery/agent-quotas/internal/model"
)

const (
	// UsageURL is the undocumented endpoint Claude Code itself uses.
	UsageURL  = "https://api.anthropic.com/api/oauth/usage"
	oauthBeta = "oauth-2025-04-20"
	maxBody   = 4 << 20
)

// StatusError is an HTTP answer other than 429 or 5xx (a 401, a 403, ...).
// The poller should not back off faster than its normal interval for it:
// retrying will not help until a human acts.
type StatusError struct {
	Status int
	Msg    string
}

func (e *StatusError) Error() string { return e.Msg }

// reason is the message the old tool would have shown for err: the bare
// text, without the UpstreamError prefix.
func reason(err error) string {
	var up *model.UpstreamError
	if errors.As(err, &up) && up.Err != nil {
		return up.Err.Error()
	}
	return err.Error()
}

func clip200(s string) string {
	if utf8.RuneCountInString(s) <= 200 {
		return s
	}
	r := []rune(s)
	return string(r[:200])
}

// fetch performs one upstream read. The token never appears in an error.
func (r *Reader) fetch(ctx context.Context, token string) (*value, error) {
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", r.cfg.Claude.UserAgent)
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, &model.UpstreamError{Err: fmt.Errorf("usage endpoint unreachable: %v", stripURLError(err))}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, &model.UpstreamError{Status: 0, Err: fmt.Errorf("usage endpoint unreachable: %v", err)}
	}
	body := strings.ToValidUTF8(string(data), "�")
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := fmt.Sprintf("usage endpoint returned HTTP %d", resp.StatusCode)
		switch resp.StatusCode {
		case 401:
			msg += " (token rejected); " + loginHint
		case 429:
			msg += " (rate limited; the endpoint recovers slowly -- rely on the cache and avoid --refresh loops)"
		}
		if d := clip200(body); d != "" {
			msg += ": " + d
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			return nil, &model.UpstreamError{
				Status:     resp.StatusCode,
				RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), r.clock()),
				Err:        errors.New(msg),
			}
		}
		return nil, &StatusError{Status: resp.StatusCode, Msg: msg}
	}
	v, err := parseValue(data)
	if err != nil {
		return nil, fmt.Errorf("usage endpoint returned invalid JSON: %s", clip200(body))
	}
	if !v.isObject() {
		return nil, fmt.Errorf("usage endpoint returned non-object JSON: %s", clip200(body))
	}
	return v, nil
}

// stripURLError drops the *url.Error wrapper, whose text repeats the URL.
func stripURLError(err error) error {
	var u *url.Error
	if errors.As(err, &u) && u.Err != nil {
		return u.Err
	}
	return err
}

// parseRetryAfter reads delta-seconds or an HTTP date; 0 when absent.
func parseRetryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(h); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
