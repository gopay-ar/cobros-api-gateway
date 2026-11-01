package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/juantevez/api-gateway-cobros/internal/ratelimit"
)

// minJWTSecretLength es el mismo mínimo que exige cobros-platform al firmar.
const minJWTSecretLength = 32

// Config es la configuración del gateway, leída de variables de entorno.
type Config struct {
	Port        string
	UpstreamURL string // API de cobros-platform
	RedisURL    string
	JWTSecret   string // el mismo JWT_SECRET de cobros-platform (HS256)

	// TrustedProxies son los CIDR de los balanceadores delante del gateway:
	// solo de ellos se acepta X-Forwarded-For para obtener la IP del cliente.
	TrustedProxies []*net.IPNet

	// RateLimitFailOpen: si Redis no responde, se deja pasar el tráfico (true)
	// o se responde 503 (false).
	RateLimitFailOpen bool

	Limits Limits
}

// Limits son los límites de cada regla, en formato "N/duración" (p. ej. "10/1m").
type Limits struct {
	GlobalIP     ratelimit.Limit // todo request, por IP
	LoginIP      ratelimit.Limit // POST /auth/login, por IP
	LoginAccount ratelimit.Limit // POST /auth/login, por tenant + email
	RefreshIP    ratelimit.Limit // POST /auth/refresh, por IP
	SignupIP     ratelimit.Limit // POST /tenants (alta pública), por IP
	Tenant       ratelimit.Limit // requests autenticados, por tenant (JWT) o API key
}

// Load lee la configuración. Los errores de formato se acumulan y los
// devuelve junto con los de Validate, para mostrarlos todos al arrancar.
func Load() (*Config, error) {
	var errs []error
	limit := func(env, fallback string) ratelimit.Limit {
		raw := getEnv(env, fallback)
		l, err := ratelimit.ParseLimit(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: %w", env, raw, err))
		}
		return l
	}

	cfg := &Config{
		Port:              getEnv("GATEWAY_PORT", ":8000"),
		UpstreamURL:       getEnv("COBROS_API_URL", "http://127.0.0.1:8080"),
		RedisURL:          getEnv("REDIS_URL", "redis://127.0.0.1:6379/0"),
		JWTSecret:         getEnv("JWT_SECRET", ""),
		RateLimitFailOpen: getEnv("RATE_LIMIT_FAIL_OPEN", "true") != "false",
		Limits: Limits{
			GlobalIP:     limit("RATE_LIMIT_GLOBAL_IP", "300/1m"),
			LoginIP:      limit("RATE_LIMIT_LOGIN_IP", "20/1m"),
			LoginAccount: limit("RATE_LIMIT_LOGIN_ACCOUNT", "10/15m"),
			RefreshIP:    limit("RATE_LIMIT_REFRESH_IP", "60/1m"),
			SignupIP:     limit("RATE_LIMIT_SIGNUP_IP", "5/1h"),
			Tenant:       limit("RATE_LIMIT_TENANT", "1200/1m"),
		},
	}

	proxies, err := parseCIDRs(getEnv("TRUSTED_PROXIES", ""))
	if err != nil {
		errs = append(errs, fmt.Errorf("TRUSTED_PROXIES: %w", err))
	}
	cfg.TrustedProxies = proxies

	errs = append(errs, cfg.Validate())
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate falla ante una URL de upstream o de Redis inválida o un secret JWT
// corto: una mala configuración no debe llegar a runtime.
func (c *Config) Validate() error {
	var errs []error
	if err := validateURL(c.UpstreamURL, "http", "https"); err != nil {
		errs = append(errs, fmt.Errorf("COBROS_API_URL=%q: %w", c.UpstreamURL, err))
	}
	if err := validateURL(c.RedisURL, "redis", "rediss"); err != nil {
		errs = append(errs, fmt.Errorf("REDIS_URL: %w", err)) // puede traer password
	}
	if len(c.JWTSecret) < minJWTSecretLength {
		errs = append(errs, fmt.Errorf("JWT_SECRET debe tener al menos %d caracteres (el mismo de cobros-platform)", minJWTSecretLength))
	}
	return errors.Join(errs...)
}

func validateURL(raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("URL inválida")
	}
	ok := false
	for _, s := range schemes {
		ok = ok || u.Scheme == s
	}
	if !ok {
		return fmt.Errorf("el esquema debe ser %s", strings.Join(schemes, " o "))
	}
	if u.Host == "" {
		return errors.New("falta el host")
	}
	return nil
}

// parseCIDRs convierte "10.0.0.0/8, 192.168.1.10" en redes. Una IP suelta
// equivale a /32 (o /128).
func parseCIDRs(value string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			ip := net.ParseIP(item)
			if ip == nil {
				return nil, fmt.Errorf("%q no es una IP ni un CIDR", item)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			item = fmt.Sprintf("%s/%d", item, bits)
		}
		_, n, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", item, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
