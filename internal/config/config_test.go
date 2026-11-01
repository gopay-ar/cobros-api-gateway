package config

import (
	"strings"
	"testing"
	"time"

	"github.com/juantevez/api-gateway-cobros/internal/ratelimit"
)

const goodSecret = "change-me-in-production-needs-32-chars!!"

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("JWT_SECRET", goodSecret)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != ":8000" || cfg.UpstreamURL != "http://127.0.0.1:8080" || !cfg.RateLimitFailOpen {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.Limits.LoginAccount != (ratelimit.Limit{Count: 10, Period: 15 * time.Minute}) {
		t.Errorf("login account limit = %v", cfg.Limits.LoginAccount)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Error("no trusted proxies by default: X-Forwarded-For is ignored")
	}
}

func TestLoad_Overrides(t *testing.T) {
	t.Setenv("JWT_SECRET", goodSecret)
	t.Setenv("RATE_LIMIT_TENANT", "50/10s")
	t.Setenv("RATE_LIMIT_FAIL_OPEN", "false")
	t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8, 192.168.1.10, 2001:db8::1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Limits.Tenant != (ratelimit.Limit{Count: 50, Period: 10 * time.Second}) || cfg.RateLimitFailOpen {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if len(cfg.TrustedProxies) != 3 || cfg.TrustedProxies[1].String() != "192.168.1.10/32" || cfg.TrustedProxies[2].String() != "2001:db8::1/128" {
		t.Errorf("trusted proxies = %v", cfg.TrustedProxies)
	}
}

// Todos los errores juntos, para corregir la configuración de una vez.
func TestLoad_ReportsAllErrors(t *testing.T) {
	t.Setenv("JWT_SECRET", "short")
	t.Setenv("COBROS_API_URL", "ftp://x")
	t.Setenv("REDIS_URL", "redis://:supersecret@")
	t.Setenv("RATE_LIMIT_LOGIN_IP", "lots")
	t.Setenv("TRUSTED_PROXIES", "not-an-ip")

	_, err := Load()
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, want := range []string{"JWT_SECRET", "COBROS_API_URL", "REDIS_URL", "RATE_LIMIT_LOGIN_IP", "TRUSTED_PROXIES"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %s: %v", want, msg)
		}
	}
	if strings.Contains(msg, "supersecret") {
		t.Error("the redis password must not appear in the error")
	}
}
