package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "dbd_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "error": msg, "error_message": msg, "error_code": code})
}

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL, testToken, "dbdiagram-cli/test")
	var slept []time.Duration
	c.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	return c, &slept
}

func TestPushSendsContract(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/sources/src-1/push" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("authorization header missing")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["dbml"] != "Table a {}" || body["format"] != "dbml" || body["sha"] != "abc123" || body["ref"] != "main" || body["message"] != "msg" {
			t.Errorf("body = %v", body)
		}
		w.Header().Set("ETag", `"5"`)
		writeJSON(w, 200, map[string]any{"success": true, "message": "pushed", "data": map[string]any{
			"run":     map[string]any{"id": "r1", "trigger": "push", "status": "succeeded", "format": "dbml", "input_bytes": 10, "version_created": 5, "actor": map[string]any{"id": nil, "name": "API token: CI"}},
			"diagram": map[string]any{"id": "d1", "name": "Main", "version": 5},
			"compile": map[string]any{"parse_ok": true, "diagnostics_count": 0, "diagnostics": []any{}},
		}})
	})
	res, err := c.Push(context.Background(), "src-1", PushRequest{DBML: "Table a {}", Format: "dbml", SHA: "abc123", Ref: "main", Message: "msg"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Run.Status != "succeeded" || res.Diagram.Version != 5 || res.Run.VersionCreated == nil || *res.Run.VersionCreated != 5 ||
		res.Compile == nil || !res.Compile.ParseOK || res.Message != "pushed" || res.Run.Actor.Name != "API token: CI" {
		t.Fatalf("result = %+v", res)
	}
}

func TestPushSourceIDIsPathEscaped(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v1/sources/a%2Fb/push" {
			t.Errorf("path = %q", r.URL.EscapedPath())
		}
		apiError(w, 404, 404, "resource not found")
	})
	_, _ = c.Push(context.Background(), "a/b", PushRequest{DBML: "x"})
}

func TestRetryOn429(t *testing.T) {
	cases := []struct {
		name       string
		statuses   []int
		retryAfter string
		wantCalls  int32
		wantErr    bool
		wantSlept  []time.Duration
	}{
		{"retry once then succeed", []int{429, 200}, "3", 2, false, []time.Duration{3 * time.Second}},
		{"retry once then give up", []int{429, 429, 200}, "1", 2, true, []time.Duration{time.Second}},
		{"no header waits 1s", []int{429, 200}, "", 2, false, []time.Duration{time.Second}},
		{"too long: no retry", []int{429, 200}, "3600", 1, true, nil},
		{"409 is not retried", []int{409, 200}, "", 1, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, slept := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				st := tc.statuses[n-1]
				switch st {
				case 200:
					writeJSON(w, 200, map[string]any{"success": true, "message": "OK", "data": map[string]any{
						"run": map[string]any{"status": "skipped"}, "diagram": map[string]any{"version": 2}}})
				case 429:
					if tc.retryAfter != "" {
						w.Header().Set("Retry-After", tc.retryAfter)
					}
					apiError(w, 429, 429, "too many requests")
				default:
					apiError(w, st, st, "this source is paused (the diagram is detached); resume it to sync")
				}
			})
			_, err := c.Push(context.Background(), "s", PushRequest{DBML: "x"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if calls.Load() != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls.Load(), tc.wantCalls)
			}
			if len(*slept) != len(tc.wantSlept) {
				t.Fatalf("slept = %v, want %v", *slept, tc.wantSlept)
			}
			for i := range tc.wantSlept {
				if (*slept)[i] != tc.wantSlept[i] {
					t.Fatalf("slept = %v, want %v", *slept, tc.wantSlept)
				}
			}
		})
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status, code int
		msg          string
		want         string
	}{
		{400, 400, "format must be dbml, mysql, postgres, mssql or schemarb", "rejected the request: format must be"},
		{401, 401, "authentication required", "dbdiagram auth login"},
		{403, 4003, "this token lacks the sync scope", "lacks the `sync` scope"},
		{403, 4005, "email unverified", "forbidden (4005)"},
		{404, 404, "resource not found", "source not found"},
		{409, 409, "this source is paused (the diagram is detached); resume it to sync", "paused"},
		{413, 413, "diagram source is larger than 1 MiB", "DBML ≤ 1 MiB"},
		{422, 4221, "could not import", "import failed: could not import"},
		{429, 429, "too many requests", "rate limited"},
		{503, 503, "the diagram compiler is busy; retry shortly", "busy"},
		{500, 500, "boom", "server error (500)"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 503 {
					w.Header().Set("Retry-After", "5")
				}
				if tc.status == 429 {
					w.Header().Set("Retry-After", "86400") // never retried in this test
				}
				apiError(w, tc.status, tc.code, tc.msg)
			})
			_, err := c.Push(context.Background(), "s", PushRequest{DBML: "x"})
			apiErr, ok := err.(*APIError)
			if !ok {
				t.Fatalf("err = %T %v", err, err)
			}
			if apiErr.Status != tc.status || apiErr.Code != tc.code {
				t.Fatalf("status/code = %d/%d", apiErr.Status, apiErr.Code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %q does not contain %q", err.Error(), tc.want)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatal("error leaks the token")
			}
		})
	}
}

func TestPull(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/sources/s1/pull" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"success": true, "message": "OK", "data": map[string]any{
			"source":  map[string]any{"id": "s1", "kind": "cli", "status": "active"},
			"diagram": map[string]any{"id": "d1", "name": "Main", "version": 3, "dbml": "Table a {}\n", "updated_at": "2026-10-01T00:00:00Z"},
			"layout":  map[string]any{"tables": map[string]any{"public.a": map[string]any{"x": 1, "y": 2}}},
		}})
	})
	res, err := c.Pull(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Diagram.Version != 3 || res.Diagram.DBML != "Table a {}\n" || res.Source.Kind != "cli" || !strings.Contains(string(res.Layout), "public.a") {
		t.Fatalf("pull = %+v", res)
	}
}

func TestVerboseNeverLogsToken(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "req-1")
		apiError(w, 401, 401, "authentication required")
	})
	var log strings.Builder
	c.Verbose = &log
	_, _ = c.Pull(context.Background(), "s1")
	out := log.String()
	if !strings.Contains(out, "GET /v1/sources/s1/pull → 401") || !strings.Contains(out, "req-1") {
		t.Fatalf("verbose log = %q", out)
	}
	if strings.Contains(out, testToken) || strings.Contains(out, "Bearer") {
		t.Fatal("verbose log leaks the token")
	}
}

func TestNonJSONErrorBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	})
	_, err := c.Pull(context.Background(), "s1")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if parseRetryAfter("7") != 7*time.Second || parseRetryAfter("") != 0 || parseRetryAfter("-1") != 0 || parseRetryAfter("soon") != 0 {
		t.Fatal("parseRetryAfter")
	}
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(future); d <= 0 || d > 11*time.Second {
		t.Fatalf("http-date retry-after = %s", d)
	}
}
