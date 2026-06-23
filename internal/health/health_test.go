package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealth_Handler_Success(t *testing.T) {
	st := New()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	st.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type: %q", ct)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS header")
	}
	var resp healthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Errorf("status field: %q", resp.Status)
	}
	if resp.Database.State != "success" {
		t.Errorf("database.state: %q", resp.Database.State)
	}
}

func TestHealth_Handler_Degraded(t *testing.T) {
	st := New()
	st.Set(StatusSnapshot{State: StatusFailed, Err: "boom"})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	st.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", w.Code)
	}
	var resp healthResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != "degraded" {
		t.Errorf("status field: %q", resp.Status)
	}
	if resp.Database.State != "failed" || resp.Database.Error != "boom" {
		t.Errorf("database: %+v", resp.Database)
	}
}

func TestHealth_SkippedIsHealthy(t *testing.T) {
	st := New()
	st.Set(StatusSnapshot{State: StatusSkipped})
	if !st.Healthy() {
		t.Error("skipped should be healthy")
	}
}

func TestDegradedPage_EscapesError(t *testing.T) {
	page := DegradedPage(`<script>alert("x")</script>`)
	if strings.Contains(page, "<script>") {
		t.Error("degraded page must HTML-escape the error message to prevent injection")
	}
	if !strings.Contains(page, "Database migration failed") {
		t.Error("degraded page missing title")
	}
}
