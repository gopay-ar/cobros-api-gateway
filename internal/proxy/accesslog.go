package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/juantevez/api-gateway-cobros/internal/ratelimit"
)

// maxCorrelationID acota el correlation ID que manda el cliente: va a los
// logs del gateway y de cobros-platform.
const maxCorrelationID = 128

// withCorrelationID asegura un X-Correlation-ID en cada request: reusa el del
// cliente si es razonable o genera uno. Se reenvía a cobros-platform (que lo
// reusa en sus logs y eventos) y se devuelve en la respuesta.
func withCorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(ratelimit.CorrelationHeader)
		if !validCorrelationID(id) {
			id = newCorrelationID()
			r.Header.Set(ratelimit.CorrelationHeader, id)
		}
		w.Header().Set(ratelimit.CorrelationHeader, id)
		next.ServeHTTP(w, r)
	})
}

func validCorrelationID(id string) bool {
	if id == "" || len(id) > maxCorrelationID {
		return false
	}
	// Solo caracteres imprimibles sin espacios: nada que rompa un log.
	return strings.IndexFunc(id, func(c rune) bool { return c <= ' ' || c > '~' }) == -1
}

func newCorrelationID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// accessLog registra cada request: método, ruta (sin query, que puede traer
// datos del cliente), status, duración, bytes, IP y correlation ID. Los
// health checks del gateway no se registran (los consulta Docker cada 10 s).
// 5xx sale como ERROR; el resto como INFO (los 429 además tienen su WARN
// con la regla, en el middleware de rate limit).
func accessLog(logger *slog.Logger, clientIP func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/gateway/health/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if rec.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		logger.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes", rec.bytes,
			"client_ip", clientIP(r),
			"correlation_id", r.Header.Get(ratelimit.CorrelationHeader),
		)
	})
}

// statusRecorder captura el status y los bytes escritos. Unwrap deja que
// http.ResponseController (que usa el reverse proxy para el flush) llegue al
// writer original.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
