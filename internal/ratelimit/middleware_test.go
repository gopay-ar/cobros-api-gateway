package ratelimit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeLimiter responde por clave; registra las claves consultadas.
type fakeLimiter struct {
	results map[string]Result
	err     error
	keys    []string
}

func (f *fakeLimiter) Allow(_ context.Context, key string, _ Limit) (Result, error) {
	f.keys = append(f.keys, key)
	if f.err != nil {
		return Result{}, f.err
	}
	if r, ok := f.results[key]; ok {
		return r, nil
	}
	return Result{Allowed: true, Remaining: 99, ResetAfter: time.Second}, nil
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func static(key string) func(*http.Request) (string, bool) {
	return func(*http.Request) (string, bool) { return key, true }
}

func run(t *testing.T, limiter Limiter, rules []Rule, failOpen bool) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	h := Middleware(limiter, rules, Options{FailOpen: failOpen, Logger: discard})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	return rec, reached
}

func TestMiddleware_DeniedReturns429WithHeaders(t *testing.T) {
	limiter := &fakeLimiter{results: map[string]Result{
		"rl:login:acct": {Allowed: false, RetryAfter: 1500 * time.Millisecond, ResetAfter: 10 * time.Second},
	}}
	rules := []Rule{{Name: "login", Limit: Limit{Count: 5, Period: time.Minute}, Key: static("acct")}}

	rec, reached := run(t, limiter, rules, true)
	if rec.Code != http.StatusTooManyRequests || reached {
		t.Fatalf("status = %d reached=%v, want 429 without reaching upstream", rec.Code, reached)
	}
	want := map[string]string{"Retry-After": "2", "RateLimit-Limit": "5", "RateLimit-Remaining": "0", "RateLimit-Reset": "10"}
	for h, v := range want {
		if got := rec.Header().Get(h); got != v {
			t.Errorf("%s = %q, want %q", h, got, v)
		}
	}
}

func TestMiddleware_FirstDenialStopsLaterRules(t *testing.T) {
	limiter := &fakeLimiter{results: map[string]Result{"rl:a:k": {Allowed: false}}}
	rules := []Rule{
		{Name: "a", Limit: Limit{Count: 1, Period: time.Second}, Key: static("k")},
		{Name: "b", Limit: Limit{Count: 1, Period: time.Second}, Key: static("k")},
	}
	run(t, limiter, rules, true)
	if len(limiter.keys) != 1 {
		t.Errorf("consulted %v, want only rl:a:k", limiter.keys)
	}
}

func TestMiddleware_SkipsRulesWithoutKey(t *testing.T) {
	limiter := &fakeLimiter{}
	rules := []Rule{
		{Name: "a", Limit: Limit{Count: 1, Period: time.Second}, Key: func(*http.Request) (string, bool) { return "", false }},
		{Name: "b", Limit: Limit{Count: 1, Period: time.Second}, Key: static("k")},
	}
	if _, reached := run(t, limiter, rules, true); !reached || len(limiter.keys) != 1 || limiter.keys[0] != "rl:b:k" {
		t.Fatalf("reached=%v keys=%v", reached, limiter.keys)
	}
}

func TestMiddleware_AllowedReportsTightestRule(t *testing.T) {
	limiter := &fakeLimiter{results: map[string]Result{
		"rl:loose:k": {Allowed: true, Remaining: 250, ResetAfter: time.Second},
		"rl:tight:k": {Allowed: true, Remaining: 3, ResetAfter: 4 * time.Second},
	}}
	rules := []Rule{
		{Name: "loose", Limit: Limit{Count: 300, Period: time.Minute}, Key: static("k")},
		{Name: "tight", Limit: Limit{Count: 10, Period: time.Minute}, Key: static("k")},
	}
	rec, reached := run(t, limiter, rules, true)
	if !reached || rec.Header().Get("RateLimit-Remaining") != "3" || rec.Header().Get("RateLimit-Limit") != "10" {
		t.Fatalf("reached=%v headers=%v", reached, rec.Header())
	}
}

func TestMiddleware_LimiterError(t *testing.T) {
	rules := []Rule{{Name: "a", Limit: Limit{Count: 1, Period: time.Second}, Key: static("k")}}

	t.Run("fail open lets the request through", func(t *testing.T) {
		if rec, reached := run(t, &fakeLimiter{err: errors.New("redis down")}, rules, true); !reached || rec.Code != http.StatusOK {
			t.Fatalf("status=%d reached=%v", rec.Code, reached)
		}
	})
	t.Run("fail closed returns 503", func(t *testing.T) {
		if rec, reached := run(t, &fakeLimiter{err: errors.New("redis down")}, rules, false); reached || rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d reached=%v", rec.Code, reached)
		}
	})
}

// El WARN de un rechazo identifica a quién se limitó y desde dónde: sin eso
// se sabía que hubo un ataque pero no de quién.
func TestMiddleware_DenialLogIdentifiesCaller(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	limiter := &fakeLimiter{results: map[string]Result{"rl:login:acct-hash": {Allowed: false, RetryAfter: 3 * time.Second}}}
	rules := []Rule{{Name: "login", Limit: Limit{Count: 10, Period: 15 * time.Minute}, Key: static("acct-hash")}}

	h := Middleware(limiter, rules, Options{Logger: logger, ClientIP: func(*http.Request) string { return "203.0.113.7" }})(http.NotFoundHandler())
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	r.Header.Set(CorrelationHeader, "c-42")
	h.ServeHTTP(httptest.NewRecorder(), r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log is not JSON: %q", buf.String())
	}
	want := map[string]any{
		"msg": "rate limit: exceeded", "rule": "login", "limit": "10/15m", "key": "acct-hash",
		"client_ip": "203.0.113.7", "method": "POST", "path": "/api/v1/auth/login",
		"correlation_id": "c-42", "retry_after_s": float64(3),
	}
	for k, v := range want {
		if entry[k] != v {
			t.Errorf("%s = %v, want %v", k, entry[k], v)
		}
	}
}
