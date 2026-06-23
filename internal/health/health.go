// Package health implements the /health endpoint and the degraded-mode page
// shown when database migration fails. Ported from the equivalent logic in
// src/server.ts.
package health

import (
	"encoding/json"
	"html"
	"net/http"
	"sync/atomic"

	"github.com/taozhang/llmrelay/internal/cors"
)

// Status is the shared migration status, atomically readable so the health
// handler and the main process agree on it.
type Status struct {
	state atomic.Pointer[StatusSnapshot]
}

// The status state values. Mirrors the TS MigrationStatus union.
const (
	StatusSuccess = "success"
	StatusSkipped = "skipped"
	StatusFailed  = "failed"
)

// StatusSnapshot is the value carried by Status.
type StatusSnapshot struct {
	State  string // "success" | "skipped" | "failed"
	Err    string // set when State == "failed"
	Reason string // set when State == "skipped"
}

// New returns a Status initialized to "success".
func New() *Status {
	s := &Status{}
	s.state.Store(&StatusSnapshot{State: "success"})
	return s
}

// Set updates the current status.
func (s *Status) Set(snapshot StatusSnapshot) { s.state.Store(&snapshot) }

// Get returns the current status snapshot.
func (s *Status) Get() StatusSnapshot { return *s.state.Load() }

// Healthy reports whether the service can serve normally.
func (s *Status) Healthy() bool {
	st := s.Get().State
	return st == "success" || st == "skipped"
}

// healthResponse mirrors the JSON returned by /health in server.ts.
type healthResponse struct {
	Status   string             `json:"status"`
	Database databaseStatusJSON `json:"database"`
}

type databaseStatusJSON struct {
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Handler returns an http.HandlerFunc for GET /health. It applies CORS and
// returns 200/503 based on migration status.
func (s *Status) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := s.Get()
		healthy := snap.State == "success" || snap.State == "skipped"
		statusStr := "ok"
		if !healthy {
			statusStr = "degraded"
		}
		body := healthResponse{
			Status: statusStr,
			Database: databaseStatusJSON{
				State:  snap.State,
				Error:  snap.Err,
				Reason: snap.Reason,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		cors.Apply(w.Header(), r)
		if healthy {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(body)
	}
}

// DegradedPage returns the HTML shown at "/" when migration failed. Mirrors
// showMigrationGuide: it renders the error into a template. We keep the
// template inline (the original loads degraded.html); P7 will swap in the
// real file.
func DegradedPage(errMsg string) string {
	const tmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>LRS — Database Migration Required</title>
<style>
  body{font-family:system-ui,sans-serif;background:#0d1117;color:#c9d1d9;margin:0;padding:2rem;line-height:1.6}
  .card{max-width:640px;margin:4rem auto;background:#161b22;border:1px solid #30363d;border-radius:8px;padding:2rem}
  h1{color:#f85149;margin-top:0;font-size:1.4rem}
  code{background:#0d1117;padding:0.15rem 0.4rem;border-radius:4px;color:#79c0ff;word-break:break-all}
  .err{background:#0d1117;border:1px solid #f8514933;border-radius:6px;padding:1rem;margin-top:1rem;font-family:ui-monospace,monospace;font-size:0.85rem;white-space:pre-wrap;color:#ffa198}
  a{color:#58a6ff}
</style>
</head>
<body>
<div class="card">
  <h1>Database migration failed</h1>
  <p>The service started but the database schema could not be migrated. The gateway is running in <strong>degraded</strong> mode.</p>
  <p>POST <code>/api/db/reset</code> to drop all tables and re-run migrations, or fix the connection and restart.</p>
  <div class="err">%s</div>
</div>
</body>
</html>`
	return replaceError(tmpl, errMsg)
}

// replaceError escapes errMsg and substitutes it into the template's {{ERROR}}
// placeholder. We escape to prevent injection of raw HTML from the error.
func replaceError(tmpl, errMsg string) string {
	// The template uses a single %s for the error; Sprintf with html-escaped
	// input is safe here.
	return simpleReplace(tmpl, html.EscapeString(errMsg))
}

// simpleReplace inserts esc into tmpl at the first "%s". (Kept simple to avoid
// fmt.Sprintf interpreting braces/percent in the template body.)
func simpleReplace(tmpl, esc string) string {
	for i := 0; i < len(tmpl)-1; i++ {
		if tmpl[i] == '%' && tmpl[i+1] == 's' {
			return tmpl[:i] + esc + tmpl[i+2:]
		}
	}
	return tmpl
}
