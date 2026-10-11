// Package client talks to dbdiagram-api's source push/pull routes
// (PUT /v1/sources/{id}/push, GET /v1/sources/{id}/pull) with an API token.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// MAX_RETRY_AFTER caps how long the single automatic 429 retry waits.
	MAX_RETRY_AFTER = 2 * time.Minute
	// MAX_RESPONSE_BYTES bounds what the client reads from the API
	// (a pull carries ≤ 1 MiB of DBML plus the layout document).
	MAX_RESPONSE_BYTES = 32 << 20
)

// Client is an API client. Token is sent only in the Authorization header
// and never logged.
type Client struct {
	BaseURL   string
	Token     string
	UserAgent string
	HTTP      *http.Client
	// Verbose, when set, receives one line per request (method, URL path,
	// status, duration, request id) — never a header or body.
	Verbose io.Writer
	// Sleep waits before the automatic retry (overridable in tests).
	Sleep func(ctx context.Context, d time.Duration) error
}

// New returns a client with sane defaults.
func New(base, token, userAgent string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(base, "/"),
		Token:     token,
		UserAgent: userAgent,
		HTTP:      NoRedirects(&http.Client{Timeout: 90 * time.Second}),
		Sleep:     sleepCtx,
	}
}

// NoRedirects returns a copy of hc that never follows a redirect: the
// request carries the token, and a 3xx is reported as an error instead of
// being replayed (with or without the token) somewhere else.
func NoRedirects(hc *http.Client) *http.Client {
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PushRequest is the push body. DBML holds the content in Format.
type PushRequest struct {
	DBML    string `json:"dbml"`
	Format  string `json:"format,omitempty"`
	SHA     string `json:"sha,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Message string `json:"message,omitempty"`
}

// Actor is who performed a run.
type Actor struct {
	ID   *string `json:"id"`
	Name string  `json:"name"`
}

// Run is a sync run.
type Run struct {
	ID               string     `json:"id"`
	Trigger          string     `json:"trigger"`
	Status           string     `json:"status"`
	Format           string     `json:"format"`
	SHA              *string    `json:"sha"`
	Ref              *string    `json:"ref"`
	InputBytes       int        `json:"input_bytes"`
	DiagnosticsCount int        `json:"diagnostics_count"`
	VersionCreated   *int       `json:"version_created"`
	Error            *string    `json:"error"`
	Actor            *Actor     `json:"actor"`
	StartedAt        *time.Time `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at"`
}

// DiagramSummary is the diagram part of a push answer.
type DiagramSummary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// Diagnostic is one compile diagnostic.
type Diagnostic struct {
	Code     json.RawMessage `json:"code"`
	Message  string          `json:"message"`
	Severity json.RawMessage `json:"severity"`
}

// Compile is the compile report of an applied push.
type Compile struct {
	ParseOK          bool         `json:"parse_ok"`
	DiagnosticsCount int          `json:"diagnostics_count"`
	Diagnostics      []Diagnostic `json:"diagnostics"`
}

// PushResult is the push answer.
type PushResult struct {
	Run     Run            `json:"run"`
	Diagram DiagramSummary `json:"diagram"`
	Compile *Compile       `json:"compile"`
	// Message is the envelope message ("pushed" / "no changes").
	Message string `json:"-"`
}

// Source is a diagram source.
type Source struct {
	ID                string  `json:"id"`
	Kind              string  `json:"kind"`
	Name              string  `json:"name"`
	Status            string  `json:"status"`
	LastSyncedVersion *int    `json:"last_synced_version"`
	LastError         *string `json:"last_error"`
	Detached          bool    `json:"detached"`
}

// PulledDiagram is the diagram part of a pull answer.
type PulledDiagram struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	DBML      string    `json:"dbml"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PullResult is the pull answer.
type PullResult struct {
	Source  *Source         `json:"source"`
	Diagram PulledDiagram   `json:"diagram"`
	Layout  json.RawMessage `json:"layout"`
}

type envelope struct {
	Success      bool            `json:"success"`
	Message      string          `json:"message"`
	Data         json.RawMessage `json:"data"`
	Error        json.RawMessage `json:"error"`
	ErrorMessage string          `json:"error_message"`
	ErrorCode    int             `json:"error_code"`
}

// Push sends content to a source. A 429 is retried once after its
// Retry-After (capped at MAX_RETRY_AFTER).
func (c *Client) Push(ctx context.Context, sourceID string, req PushRequest) (*PushResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var out PushResult
	msg, err := c.do(ctx, http.MethodPut, "/v1/sources/"+url.PathEscape(sourceID)+"/push", body, &out)
	if err != nil {
		return nil, err
	}
	out.Message = msg
	return &out, nil
}

// Pull fetches a source's diagram and layout.
func (c *Client) Pull(ctx context.Context, sourceID string) (*PullResult, error) {
	var out PullResult
	if _, err := c.do(ctx, http.MethodGet, "/v1/sources/"+url.PathEscape(sourceID)+"/pull", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) (string, error) {
	for attempt := 0; ; attempt++ {
		msg, err := c.once(ctx, method, path, body, out)
		apiErr, ok := err.(*APIError)
		if !ok || apiErr.Status != http.StatusTooManyRequests || attempt > 0 {
			return msg, err
		}
		wait := apiErr.RetryAfter
		if wait <= 0 {
			wait = time.Second
		}
		if wait > MAX_RETRY_AFTER {
			return msg, err
		}
		c.logf("rate limited; retrying once in %s", wait)
		if serr := c.Sleep(ctx, wait); serr != nil {
			return msg, err
		}
	}
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, out any) (string, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	started := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		c.logf("%s %s → error after %s", method, path, time.Since(started).Round(time.Millisecond))
		return "", fmt.Errorf("could not reach %s: %w", c.BaseURL, redactErr(err, c.Token))
	}
	defer resp.Body.Close()
	c.logf("%s %s → %d in %s (request id %s)", method, path, resp.StatusCode,
		time.Since(started).Round(time.Millisecond), orDash(resp.Header.Get("X-Request-ID")))
	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		return "", &APIError{Status: resp.StatusCode, Redirect: resp.Header.Get("Location")}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MAX_RESPONSE_BYTES))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	var env envelope
	jsonErr := json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || jsonErr != nil || !env.Success {
		e := &APIError{Status: resp.StatusCode, Code: env.ErrorCode, Message: env.ErrorMessage,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
		if e.Message == "" && len(env.Error) > 0 {
			var s string
			if json.Unmarshal(env.Error, &s) == nil {
				e.Message = s
			}
		}
		if e.Status >= 200 && e.Status <= 299 {
			e.Message = "unexpected response from the API"
		}
		return "", e
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return "", fmt.Errorf("decode response: %w", err)
		}
	}
	return env.Message, nil
}

func (c *Client) logf(format string, args ...any) {
	if c.Verbose != nil {
		fmt.Fprintf(c.Verbose, "[dbdiagram] "+format+"\n", args...)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// redactErr scrubs the token from a transport error (defence in depth: the
// token is only ever in a header, which net/http never echoes).
func redactErr(err error, token string) error {
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "[REDACTED]"))
}

// parseRetryAfter reads delta-seconds or an HTTP date.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
