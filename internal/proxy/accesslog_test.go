package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func logEntries(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestAccessLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := accessLog(logger, func(*http.Request) string { return "203.0.113.7" }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/boom":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("hello"))
		}
	}))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/payments?card=4111", nil)
	r.Header.Set("X-Correlation-ID", "c-1")
	h.ServeHTTP(httptest.NewRecorder(), r)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/gateway/health/live", nil))

	entries := logEntries(t, &buf)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (health checks are not logged)", len(entries))
	}
	ok := entries[0]
	want := map[string]any{
		"level": "INFO", "msg": "request", "method": "POST", "path": "/api/v1/payments",
		"status": float64(201), "bytes": float64(5), "client_ip": "203.0.113.7", "correlation_id": "c-1",
	}
	for k, v := range want {
		if ok[k] != v {
			t.Errorf("%s = %v, want %v", k, ok[k], v)
		}
	}
	if _, has := ok["duration_ms"]; !has {
		t.Error("duration_ms missing")
	}
	// La query no se loguea: puede traer datos del cliente.
	if strings.Contains(buf.String(), "4111") {
		t.Error("query string must not be logged")
	}
	if entries[1]["level"] != "ERROR" || entries[1]["status"] != float64(502) {
		t.Errorf("5xx entry = %v, want ERROR 502", entries[1])
	}
}

func TestWithCorrelationID(t *testing.T) {
	var seen string
	h := withCorrelationID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Correlation-ID")
	}))
	do := func(id string) (string, string) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if id != "" {
			r.Header.Set("X-Correlation-ID", id)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return seen, rec.Header().Get("X-Correlation-ID")
	}

	if fwd, resp := do("abc-123"); fwd != "abc-123" || resp != "abc-123" {
		t.Errorf("client id: forwarded %q, response %q", fwd, resp)
	}
	for name, bad := range map[string]string{"missing": "", "too long": strings.Repeat("x", 129), "with spaces": "a b", "control chars": "a\tb"} {
		fwd, resp := do(bad)
		if len(fwd) != 32 || fwd != resp || fwd == bad {
			t.Errorf("%s: forwarded %q, response %q; want a new 32-char id", name, fwd, resp)
		}
	}
}

// El reverse proxy hace flush para respuestas en streaming: el recorder no
// debe ocultar el Flusher del writer original.
func TestStatusRecorder_Flush(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}
	if err := http.NewResponseController(sr).Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if !rec.Flushed {
		t.Error("underlying writer was not flushed")
	}
}
