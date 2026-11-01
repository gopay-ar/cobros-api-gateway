// Package clientip obtiene la IP real del cliente.
package clientip

import (
	"net"
	"net/http"
	"strings"
)

// Resolver obtiene la IP del cliente. X-Forwarded-For solo se usa cuando la
// conexión viene de un proxy de confianza: si no, cualquiera lo falsifica y
// rota de "IP" en cada request para esquivar el rate limit.
type Resolver struct {
	trusted []*net.IPNet
}

func NewResolver(trusted []*net.IPNet) *Resolver {
	return &Resolver{trusted: trusted}
}

// IP devuelve la IP del cliente. Si la conexión viene de un proxy de
// confianza, recorre X-Forwarded-For de derecha a izquierda (cada proxy
// agrega al final) y devuelve la primera IP que no es de confianza.
func (r *Resolver) IP(req *http.Request) string {
	remote := hostOnly(req.RemoteAddr)
	if !r.isTrusted(remote) {
		return remote
	}
	hops := strings.Split(strings.Join(req.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if net.ParseIP(hop) == nil {
			break // valor inválido: no seguir confiando en lo que haya a la izquierda
		}
		if !r.isTrusted(hop) {
			return hop
		}
		remote = hop
	}
	return remote
}

func (r *Resolver) isTrusted(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range r.trusted {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
