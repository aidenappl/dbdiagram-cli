package client

import (
	"fmt"
	"net/http"
	"time"
)

// API error codes the CLI recognises (dbdiagram-api AGENTS.md).
const (
	CODE_FORBIDDEN     = 4003 // token lacks the scope / role too low
	CODE_PENDING       = 4004
	CODE_UNVERIFIED    = 4005
	CODE_IMPORT_FAILED = 4221 // SQL / schema.rb import failed
)

// APIError is a refusal from the API, with a human explanation in Error().
type APIError struct {
	Status     int
	Code       int
	Message    string
	RetryAfter time.Duration
	// Redirect is the Location of a 3xx answer (never followed).
	Redirect string
}

func (e *APIError) Error() string {
	server := e.Message
	if server == "" {
		server = http.StatusText(e.Status)
	}
	switch {
	case e.Status >= 300 && e.Status <= 399:
		return fmt.Sprintf("the API answered a redirect (%d to %q); it is not followed, so the token is never sent elsewhere. "+
			"Check --api / DBDIAGRAM_API (it must be the API origin itself)", e.Status, e.Redirect)
	case e.Status == http.StatusBadRequest:
		return "the API rejected the request: " + server
	case e.Status == http.StatusUnauthorized:
		return "authentication failed: the token is missing, invalid, revoked or expired. " +
			"Run `dbdiagram auth login --token dbd_…` or set DBDIAGRAM_TOKEN"
	case e.Status == http.StatusForbidden && e.Code == CODE_FORBIDDEN:
		return "forbidden: the token lacks the `sync` scope (or your team role is below editor). " +
			"Create a team API token with the sync scope"
	case e.Status == http.StatusForbidden:
		return fmt.Sprintf("forbidden (%d): %s", e.Code, server)
	case e.Status == http.StatusNotFound:
		return "source not found: check source_id, and that the token belongs to the source's team"
	case e.Status == http.StatusConflict:
		return "conflict: " + server +
			" (a paused source refuses pushes until it is resumed in the web app; an archived team or diagram must be restored)"
	case e.Status == http.StatusRequestEntityTooLarge:
		return "too large: " + server + " (limits: DBML ≤ 1 MiB; SQL / schema.rb ≤ 4 MiB, and the imported DBML ≤ 1 MiB)"
	case e.Status == http.StatusUnprocessableEntity && e.Code == CODE_IMPORT_FAILED:
		return "import failed: " + server
	case e.Status == http.StatusTooManyRequests:
		return "rate limited by the API" + retryHint(e.RetryAfter) + " (push and pull share 30/min per token, or per user for a signed-in session; pushes are also capped at 120/hour per team; " +
			"SQL / schema.rb imports additionally wait for a compiler slot — 503 when busy)"
	case e.Status == http.StatusServiceUnavailable:
		return "the API is busy" + retryHint(e.RetryAfter) + ": " + server
	case e.Status >= 500:
		return fmt.Sprintf("server error (%d): %s", e.Status, server)
	}
	return fmt.Sprintf("request failed (%d): %s", e.Status, server)
}

func retryHint(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return fmt.Sprintf("; retry after %s", d.Round(time.Second))
}
