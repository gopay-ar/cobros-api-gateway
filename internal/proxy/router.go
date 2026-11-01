// Package proxy arma el handler del gateway: health checks, rate limit y el
// reverse proxy hacia cobros-platform.
package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/juantevez/api-gateway-cobros/internal/clientip"
	"github.com/juantevez/api-gateway-cobros/internal/config"
	"github.com/juantevez/api-gateway-cobros/internal/identity"
	"github.com/juantevez/api-gateway-cobros/internal/ratelimit"
)

// Timeouts hacia cobros-platform: sin ellos un upstream colgado retiene
// conexiones del gateway indefinidamente.
const (
	upstreamDialTimeout           = 5 * time.Second
	upstreamResponseHeaderTimeout = 30 * time.Second
	readinessTimeout              = 2 * time.Second
)

// Pinger verifica la conexión con Redis (readiness).
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps son las dependencias del handler.
type Deps struct {
	Config  *config.Config
	Limiter ratelimit.Limiter
	Redis   Pinger
	Logger  *slog.Logger
}

// NewHandler devuelve el handler del gateway. Todo lo que no es health check
// pasa por el rate limit y se reenvía tal cual a cobros-platform, que sigue
// siendo quien autentica y autoriza.
func NewHandler(d Deps) http.Handler {
	ips := clientip.NewResolver(d.Config.TrustedProxies)
	callers := identity.NewExtractor(d.Config.JWTSecret)
	upstream := newReverseProxy(d.Config.UpstreamURL, ips)

	byIP := func(r *http.Request) (string, bool) { return ips.IP(r), true }
	only := func(method, path string, key func(*http.Request) (string, bool)) func(*http.Request) (string, bool) {
		return func(r *http.Request) (string, bool) {
			if r.Method != method || r.URL.Path != path {
				return "", false
			}
			return key(r)
		}
	}

	l := d.Config.Limits
	rules := []ratelimit.Rule{
		// Las específicas primero: un login bloqueado no gasta el cupo global.
		{Name: "login_account", Limit: l.LoginAccount, Key: only(http.MethodPost, "/api/v1/auth/login", identity.LoginAccount)},
		{Name: "login_ip", Limit: l.LoginIP, Key: only(http.MethodPost, "/api/v1/auth/login", byIP)},
		{Name: "refresh_ip", Limit: l.RefreshIP, Key: only(http.MethodPost, "/api/v1/auth/refresh", byIP)},
		{Name: "signup_ip", Limit: l.SignupIP, Key: only(http.MethodPost, "/api/v1/tenants", byIP)},
		{Name: "tenant", Limit: l.Tenant, Key: callers.Caller},
		{Name: "global_ip", Limit: l.GlobalIP, Key: byIP},
	}
	limited := ratelimit.Middleware(d.Limiter, rules, ratelimit.Options{
		FailOpen: d.Config.RateLimitFailOpen, Logger: d.Logger, ClientIP: ips.IP,
	})(upstream)

	mux := http.NewServeMux()
	// Health del propio gateway: no consultan a cobros-platform, para que su
	// caída no saque de rotación al gateway (cobros expone su propio /health).
	mux.HandleFunc("GET /gateway/health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /gateway/health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		if err := d.Redis.Ping(ctx); err != nil {
			// Con fail-open el gateway sigue sirviendo sin Redis (sin límite).
			status := http.StatusOK
			if !d.Config.RateLimitFailOpen {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, map[string]string{"status": "degraded", "redis": "down"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "redis": "up"})
	})
	mux.Handle("/", limited)
	return withCorrelationID(accessLog(d.Logger, ips.IP, mux))
}

// newReverseProxy ignora el error de url.Parse: config.Validate ya garantiza
// una URL válida. X-Forwarded-For se reemplaza por la IP resuelta del
// cliente: no se reenvía el que mandó el cliente, que puede ser falso.
func newReverseProxy(target string, ips *clientip.Resolver) *httputil.ReverseProxy {
	u, _ := url.Parse(target)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: upstreamDialTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = upstreamResponseHeaderTimeout

	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = u.Host
			pr.Out.Header.Set("X-Forwarded-For", ips.IP(pr.In))
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			proto := "http"
			if pr.In.TLS != nil {
				proto = "https"
			}
			pr.Out.Header.Set("X-Forwarded-Proto", proto)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("proxy: upstream error", "path", r.URL.Path,
				"correlation_id", r.Header.Get(ratelimit.CorrelationHeader), "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream unavailable"})
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
