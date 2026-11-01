package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"github.com/juantevez/api-gateway-cobros/internal/config"
	"github.com/juantevez/api-gateway-cobros/internal/ratelimit"
)

const secret = "test-secret-with-at-least-32-chars!!"

type env struct {
	gateway  http.Handler
	upstream *httptest.Server
	hits     *atomic.Int32
	lastReq  *http.Request
	lastBody string
	redis    *miniredis.Miniredis
}

func limits() config.Limits {
	l := func(n int, d time.Duration) ratelimit.Limit { return ratelimit.Limit{Count: n, Period: d} }
	return config.Limits{
		GlobalIP: l(100, time.Minute), LoginIP: l(5, time.Minute), LoginAccount: l(2, time.Minute),
		RefreshIP: l(3, time.Minute), SignupIP: l(1, time.Hour), Tenant: l(3, time.Minute),
	}
}

func newEnv(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	e := &env{hits: &atomic.Int32{}}
	e.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		e.lastReq, e.lastBody = r, string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(e.upstream.Close)

	e.redis = miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: e.redis.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := &config.Config{UpstreamURL: e.upstream.URL, JWTSecret: secret, RateLimitFailOpen: true, Limits: limits()}
	if mutate != nil {
		mutate(cfg)
	}
	e.gateway = NewHandler(Deps{
		Config: cfg, Limiter: ratelimit.NewRedisLimiter(rdb), Redis: pinger{rdb},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return e
}

type pinger struct{ c *redis.Client }

func (p pinger) Ping(ctx context.Context) error { return p.c.Ping(ctx).Err() }

func (e *env) do(method, path, body, ip string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = ip + ":40000"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.gateway.ServeHTTP(rec, r)
	return rec
}

func TestGateway_ProxiesToCobros(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.do(http.MethodPost, "/api/v1/payments?x=1", `{"amount":100}`, "203.0.113.7",
		map[string]string{"X-Forwarded-For": "1.2.3.4", "X-Correlation-ID": "c-1"})

	if rec.Code != http.StatusOK || e.hits.Load() != 1 {
		t.Fatalf("status=%d hits=%d", rec.Code, e.hits.Load())
	}
	r := e.lastReq
	if r.URL.Path != "/api/v1/payments" || r.URL.RawQuery != "x=1" || e.lastBody != `{"amount":100}` {
		t.Errorf("upstream got %s?%s body=%q", r.URL.Path, r.URL.RawQuery, e.lastBody)
	}
	// La IP falsificada por el cliente no llega a cobros-platform.
	if got := r.Header.Get("X-Forwarded-For"); got != "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q, want the real client ip", got)
	}
	if r.Header.Get("X-Correlation-ID") != "c-1" {
		t.Error("headers must pass through")
	}
}

func TestGateway_LoginLimitedPerAccountAndIP(t *testing.T) {
	e := newEnv(t, nil)
	login := func(email, ip string) int {
		body := `{"tenant_id":"t1","email":"` + email + `","password":"x"}`
		return e.do(http.MethodPost, "/api/v1/auth/login", body, ip, nil).Code
	}

	// Por cuenta: 2 intentos, aunque cambie la IP (fuerza bruta distribuida).
	if login("ana@x.com", "198.51.100.1") != 200 || login("ana@x.com", "198.51.100.2") != 200 {
		t.Fatal("first two attempts must pass")
	}
	if code := login("ana@x.com", "198.51.100.3"); code != http.StatusTooManyRequests {
		t.Fatalf("3rd attempt on the same account = %d, want 429", code)
	}
	// El cuerpo llegó entero a cobros (el gateway lo leyó y lo restituyó).
	if !strings.Contains(e.lastBody, `"password":"x"`) {
		t.Errorf("upstream body = %q", e.lastBody)
	}

	// Por IP: 5 intentos con cuentas distintas (password spraying).
	ip := "203.0.113.50"
	for i := range 5 {
		if code := login("user"+string(rune('a'+i))+"@x.com", ip); code != 200 {
			t.Fatalf("attempt %d from the ip = %d", i+1, code)
		}
	}
	if code := login("other@x.com", ip); code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt from the same ip = %d, want 429", code)
	}
}

func TestGateway_SignupAndRefreshLimitedPerIP(t *testing.T) {
	e := newEnv(t, nil)
	if e.do(http.MethodPost, "/api/v1/tenants", `{}`, "203.0.113.9", nil).Code != 200 {
		t.Fatal("first signup must pass")
	}
	if code := e.do(http.MethodPost, "/api/v1/tenants", `{}`, "203.0.113.9", nil).Code; code != http.StatusTooManyRequests {
		t.Fatalf("2nd signup = %d, want 429", code)
	}
	if code := e.do(http.MethodPost, "/api/v1/tenants", `{}`, "203.0.113.10", nil).Code; code != 200 {
		t.Fatalf("signup from another ip = %d", code)
	}

	for range 3 {
		e.do(http.MethodPost, "/api/v1/auth/refresh", `{}`, "203.0.113.9", nil)
	}
	if code := e.do(http.MethodPost, "/api/v1/auth/refresh", `{}`, "203.0.113.9", nil).Code; code != http.StatusTooManyRequests {
		t.Fatalf("4th refresh = %d, want 429", code)
	}
}

func TestGateway_TenantLimitAcrossIPs(t *testing.T) {
	e := newEnv(t, nil)
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"tid": "tenant-1", "exp": time.Now().Add(time.Minute).Unix(),
	}).SignedString([]byte(secret))
	auth := map[string]string{"Authorization": "Bearer " + tok}

	for i := range 3 {
		ip := "198.51.100." + string(rune('1'+i))
		if code := e.do(http.MethodGet, "/api/v1/payments", "", ip, auth).Code; code != 200 {
			t.Fatalf("request %d = %d", i+1, code)
		}
	}
	rec := e.do(http.MethodGet, "/api/v1/payments", "", "198.51.100.9", auth)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("4th request of the tenant = %d (Retry-After %q), want 429", rec.Code, rec.Header().Get("Retry-After"))
	}
	if e.hits.Load() != 3 {
		t.Errorf("upstream hits = %d, want 3 (the 429 never reaches cobros)", e.hits.Load())
	}

	// Otro tenant tiene su propio cupo.
	other, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"tid": "tenant-2", "exp": time.Now().Add(time.Minute).Unix(),
	}).SignedString([]byte(secret))
	if code := e.do(http.MethodGet, "/api/v1/payments", "", "198.51.100.9", map[string]string{"Authorization": "Bearer " + other}).Code; code != 200 {
		t.Fatalf("another tenant = %d", code)
	}
}

func TestGateway_GlobalIPLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Limits.GlobalIP = ratelimit.Limit{Count: 2, Period: time.Minute} })
	e.do(http.MethodGet, "/api/v1/x", "", "203.0.113.1", nil)
	e.do(http.MethodGet, "/api/v1/x", "", "203.0.113.1", nil)
	if code := e.do(http.MethodGet, "/api/v1/x", "", "203.0.113.1", nil).Code; code != http.StatusTooManyRequests {
		t.Fatalf("3rd request from the ip = %d, want 429", code)
	}
}

func TestGateway_TrustedProxyXFF(t *testing.T) {
	_, lb, _ := net.ParseCIDR("10.0.0.0/8")
	e := newEnv(t, func(c *config.Config) {
		c.TrustedProxies = []*net.IPNet{lb}
		c.Limits.SignupIP = ratelimit.Limit{Count: 1, Period: time.Hour}
	})
	viaLB := func(client string) int {
		return e.do(http.MethodPost, "/api/v1/tenants", `{}`, "10.0.0.2", map[string]string{"X-Forwarded-For": client}).Code
	}
	// Detrás del LB, cada cliente cuenta por su IP, no por la del LB.
	if viaLB("198.51.100.1") != 200 || viaLB("198.51.100.2") != 200 {
		t.Fatal("different clients behind the lb must have separate budgets")
	}
	if viaLB("198.51.100.1") != http.StatusTooManyRequests {
		t.Fatal("same client behind the lb must be limited")
	}
	if got := e.lastReq.Header.Get("X-Forwarded-For"); got != "198.51.100.2" {
		t.Errorf("X-Forwarded-For to cobros = %q", got)
	}
}

func TestGateway_RedisDown(t *testing.T) {
	t.Run("fail open keeps serving", func(t *testing.T) {
		e := newEnv(t, nil)
		e.redis.Close()
		if code := e.do(http.MethodGet, "/api/v1/x", "", "203.0.113.1", nil).Code; code != 200 {
			t.Fatalf("status = %d, want 200", code)
		}
		rec := e.do(http.MethodGet, "/gateway/health/ready", "", "203.0.113.1", nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"redis":"down"`) {
			t.Fatalf("ready = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("fail closed returns 503", func(t *testing.T) {
		e := newEnv(t, func(c *config.Config) { c.RateLimitFailOpen = false })
		e.redis.Close()
		if code := e.do(http.MethodGet, "/api/v1/x", "", "203.0.113.1", nil).Code; code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", code)
		}
		if code := e.do(http.MethodGet, "/gateway/health/ready", "", "203.0.113.1", nil).Code; code != http.StatusServiceUnavailable {
			t.Fatalf("ready = %d, want 503", code)
		}
	})
}

func TestGateway_HealthNotLimitedNorProxied(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Limits.GlobalIP = ratelimit.Limit{Count: 1, Period: time.Hour} })
	for range 3 {
		if code := e.do(http.MethodGet, "/gateway/health/live", "", "203.0.113.1", nil).Code; code != 200 {
			t.Fatalf("live = %d", code)
		}
	}
	if e.hits.Load() != 0 {
		t.Error("health checks must not reach cobros")
	}
}

func TestGateway_UpstreamDown(t *testing.T) {
	e := newEnv(t, nil)
	e.upstream.Close()
	rec := e.do(http.MethodGet, "/api/v1/x", "", "203.0.113.1", nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream unavailable") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
}

// El mismo correlation ID queda en el log del gateway y llega a cobros.
func TestGateway_CorrelationIDReachesCobros(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.do(http.MethodGet, "/api/v1/x", "", "203.0.113.1", nil)
	id := rec.Header().Get("X-Correlation-ID")
	if len(id) != 32 || e.lastReq.Header.Get("X-Correlation-ID") != id {
		t.Fatalf("response id %q, upstream id %q", id, e.lastReq.Header.Get("X-Correlation-ID"))
	}
}
