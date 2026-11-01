package identity

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const secret = "test-secret-with-at-least-32-chars!!"

func sign(t *testing.T, key string, method jwt.SigningMethod, claims jwt.MapClaims) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(method, claims).SignedString([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestExtractor_Caller(t *testing.T) {
	e := NewExtractor(secret)
	valid := jwt.MapClaims{"sub": "u1", "tid": "t1", "exp": time.Now().Add(time.Minute).Unix()}

	cases := []struct {
		name    string
		headers map[string]string
		want    string
		ok      bool
	}{
		{"valid jwt", map[string]string{"Authorization": "Bearer " + sign(t, secret, jwt.SigningMethodHS256, valid)}, "tenant:t1", true},
		// Sin verificar la firma, cualquiera gastaría el cupo de otro tenant.
		{"wrong secret", map[string]string{"Authorization": "Bearer " + sign(t, "another-secret-with-32-characters!!", jwt.SigningMethodHS256, valid)}, "", false},
		{"expired", map[string]string{"Authorization": "Bearer " + sign(t, secret, jwt.SigningMethodHS256, jwt.MapClaims{"tid": "t1", "exp": time.Now().Add(-time.Minute).Unix()})}, "", false},
		{"no exp", map[string]string{"Authorization": "Bearer " + sign(t, secret, jwt.SigningMethodHS256, jwt.MapClaims{"tid": "t1"})}, "", false},
		{"other alg", map[string]string{"Authorization": "Bearer " + sign(t, secret, jwt.SigningMethodHS512, valid)}, "", false},
		{"no tid", map[string]string{"Authorization": "Bearer " + sign(t, secret, jwt.SigningMethodHS256, jwt.MapClaims{"exp": time.Now().Add(time.Minute).Unix()})}, "", false},
		{"garbage token", map[string]string{"Authorization": "Bearer nope"}, "", false},
		{"basic auth", map[string]string{"Authorization": "Basic abc"}, "", false},
		{"no credentials", nil, "", false},
		{"api key", map[string]string{"X-Api-Key": "production_Xk3mPQrS_secret"}, "apikey:Xk3mPQrS", true},
		// cobros-platform prioriza la API key sobre el JWT: el gateway también.
		{"api key wins over jwt", map[string]string{"X-Api-Key": "test_Ab12Cd34_s", "Authorization": "Bearer " + sign(t, secret, jwt.SigningMethodHS256, valid)}, "apikey:Ab12Cd34", true},
		{"malformed api key", map[string]string{"X-Api-Key": "garbage"}, "", false},
		{"api key empty prefix", map[string]string{"X-Api-Key": "test__secret"}, "", false},
		{"api key huge prefix", map[string]string{"X-Api-Key": "test_" + strings.Repeat("x", 65) + "_s"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			got, ok := e.Caller(r)
			if got != tc.want || ok != tc.ok {
				t.Errorf("Caller = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestLoginAccount(t *testing.T) {
	key := func(body string) (string, bool, string) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
		k, ok := LoginAccount(r)
		rest, _ := io.ReadAll(r.Body)
		return k, ok, string(rest)
	}

	body := `{"tenant_id":"T-1","email":"Ana@Example.com","password":"x"}`
	k1, ok, rest := key(body)
	if !ok || len(k1) != 32 {
		t.Fatalf("key = %q, %v", k1, ok)
	}
	// El body llega intacto a cobros-platform.
	if rest != body {
		t.Errorf("body after peek = %q, want %q", rest, body)
	}
	// Mayúsculas y espacios no generan otra cuenta (se esquivaría el límite).
	if k2, _, _ := key(`{"tenant_id":" t-1 ","email":"ana@example.COM ","password":"y"}`); k2 != k1 {
		t.Errorf("normalized key differs: %q vs %q", k2, k1)
	}
	if k3, _, _ := key(`{"tenant_id":"t-2","email":"ana@example.com"}`); k3 == k1 {
		t.Error("another tenant must be another account")
	}
	if strings.Contains(k1, "ana") {
		t.Error("the key must not contain the email")
	}

	for _, bad := range []string{"", "not json", `{"email":"a@b.c"}`, `{"tenant_id":"t"}`} {
		if _, ok, rest := key(bad); ok || rest != bad {
			t.Errorf("body %q: ok=%v rest=%q", bad, ok, rest)
		}
	}
}

func TestLoginAccount_LargeBodyStaysIntact(t *testing.T) {
	body := `{"tenant_id":"t","email":"a@b.c","pad":"` + strings.Repeat("x", maxLoginBodyPeek) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if _, ok := LoginAccount(r); ok {
		t.Error("truncated JSON cannot be parsed: no account key")
	}
	if rest, _ := io.ReadAll(r.Body); string(rest) != body {
		t.Errorf("body length after peek = %d, want %d", len(rest), len(body))
	}
}
