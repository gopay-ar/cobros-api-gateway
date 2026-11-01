package ratelimit

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Rule es un límite aplicado a los requests para los que Key devuelve una
// clave. Cada regla tiene su propio espacio de claves en Redis.
type Rule struct {
	Name  string
	Limit Limit
	// Key identifica a quién se le cuenta el request (una IP, un tenant, una
	// cuenta). false: la regla no aplica a este request.
	Key func(*http.Request) (string, bool)
}

// keyPrefix es el espacio de nombres del gateway en Redis.
const keyPrefix = "rl:"

// Middleware aplica las reglas en orden. El primer límite superado corta el
// request con 429; si todas pasan, se informan los headers de la regla con
// menos margen.
//
// Si el limitador falla (Redis caído): con failOpen el request sigue, sin
// límite; sin failOpen se responde 503.
func Middleware(limiter Limiter, rules []Rule, failOpen bool, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var tightest *applied
			for _, rule := range rules {
				key, ok := rule.Key(r)
				if !ok {
					continue
				}
				res, err := limiter.Allow(r.Context(), keyPrefix+rule.Name+":"+key, rule.Limit)
				if err != nil {
					logger.Error("rate limit: limiter error", "rule", rule.Name, "fail_open", failOpen, "error", err)
					if failOpen {
						continue
					}
					writeError(w, http.StatusServiceUnavailable, "rate limiter unavailable")
					return
				}
				a := &applied{rule: rule, res: res}
				if !res.Allowed {
					logger.Warn("rate limit: exceeded", "rule", rule.Name, "limit", rule.Limit.String(),
						"path", r.URL.Path, "retry_after", res.RetryAfter)
					a.setHeaders(w)
					w.Header().Set("Retry-After", strconv.Itoa(ceilSeconds(res.RetryAfter)))
					writeError(w, http.StatusTooManyRequests, "too many requests")
					return
				}
				if tightest == nil || res.Remaining < tightest.res.Remaining {
					tightest = a
				}
			}
			if tightest != nil {
				tightest.setHeaders(w)
			}
			next.ServeHTTP(w, r)
		})
	}
}

type applied struct {
	rule Rule
	res  Result
}

// setHeaders usa los headers RateLimit-* del draft de la IETF
// (draft-ietf-httpapi-ratelimit-headers).
func (a *applied) setHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("RateLimit-Limit", strconv.Itoa(a.rule.Limit.Count))
	h.Set("RateLimit-Remaining", strconv.Itoa(a.res.Remaining))
	h.Set("RateLimit-Reset", strconv.Itoa(ceilSeconds(a.res.ResetAfter)))
}

func ceilSeconds(d time.Duration) int {
	return int(math.Ceil(d.Seconds()))
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
