// Package identity extrae del request a quién se le cuenta el rate limit:
// el tenant del JWT, la API key o la cuenta de un intento de login.
package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// maxLoginBodyPeek es lo que se lee del body del login para obtener la
// cuenta. El body completo sigue llegando intacto a cobros-platform.
const maxLoginBodyPeek = 64 << 10

// maxApiKeyPrefix acota el prefix que se usa como clave en Redis.
const maxApiKeyPrefix = 64

// accessClaims son los claims del access token de cobros-platform.
type accessClaims struct {
	jwt.RegisteredClaims
	TenantID string `json:"tid"`
}

// Extractor identifica al llamador. Verifica la firma del JWT con el mismo
// secret que cobros-platform: un token sin verificar dejaría a cualquiera
// gastar el cupo de otro tenant poniendo su tid.
type Extractor struct {
	secret []byte
	parser *jwt.Parser
}

func NewExtractor(jwtSecret string) *Extractor {
	return &Extractor{
		secret: []byte(jwtSecret),
		parser: jwt.NewParser(jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired()),
	}
}

// Caller devuelve la clave del llamador autenticado:
//   - "apikey:<prefix>" si trae X-Api-Key (cobros-platform la prioriza sobre
//     el JWT). El prefix no se verifica acá (el secreto se valida con Argon2
//     en cobros); alcanza para separar cupos entre keys.
//   - "tenant:<tid>" si trae un Bearer JWT válido.
//
// false si no hay credencial o el JWT no es válido: ese request solo cuenta
// por IP, y cobros-platform lo rechaza con 401.
func (e *Extractor) Caller(r *http.Request) (string, bool) {
	if raw := r.Header.Get("X-Api-Key"); raw != "" {
		parts := strings.SplitN(raw, "_", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" || len(parts[1]) > maxApiKeyPrefix {
			return "", false
		}
		return "apikey:" + parts[1], true
	}

	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	var claims accessClaims
	if _, err := e.parser.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return e.secret, nil }); err != nil {
		return "", false
	}
	if claims.TenantID == "" {
		return "", false
	}
	return "tenant:" + claims.TenantID, true
}

// LoginAccount devuelve la clave de la cuenta de un POST /auth/login: hash
// de tenant_id + email, para no guardar emails en Redis. Lee el body y lo
// deja intacto para el proxy. false si el body no trae ambos campos (cobros
// responde 400 y el intento ya cuenta por IP).
func LoginAccount(r *http.Request) (string, bool) {
	if r.Body == nil {
		return "", false
	}
	peek, err := io.ReadAll(io.LimitReader(r.Body, maxLoginBodyPeek))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), r.Body), r.Body}
	if err != nil {
		return "", false
	}

	var body struct {
		TenantID string `json:"tenant_id"`
		Email    string `json:"email"`
	}
	if json.Unmarshal(peek, &body) != nil {
		return "", false
	}
	tenant := strings.ToLower(strings.TrimSpace(body.TenantID))
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if tenant == "" || email == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(tenant + "|" + email))
	return hex.EncodeToString(sum[:16]), true
}
