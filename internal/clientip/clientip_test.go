package clientip

import (
	"net"
	"net/http/httptest"
	"testing"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestResolver_IP(t *testing.T) {
	lb := mustCIDR(t, "10.0.0.0/8")
	cases := []struct {
		name    string
		trusted []*net.IPNet
		remote  string
		xff     []string
		want    string
	}{
		{"no proxies: remote addr", nil, "203.0.113.7:5000", nil, "203.0.113.7"},
		// Sin proxies de confianza, X-Forwarded-For es del cliente: se ignora.
		{"spoofed xff ignored", nil, "203.0.113.7:5000", []string{"1.2.3.4"}, "203.0.113.7"},
		{"untrusted remote ignores xff", []*net.IPNet{lb}, "203.0.113.7:5000", []string{"1.2.3.4"}, "203.0.113.7"},
		{"trusted lb: last hop", []*net.IPNet{lb}, "10.0.0.2:5000", []string{"198.51.100.9"}, "198.51.100.9"},
		// El cliente puede anteponer IPs falsas; se toma la que agregó el LB.
		{"client-prepended entries ignored", []*net.IPNet{lb}, "10.0.0.2:5000", []string{"1.2.3.4, 198.51.100.9"}, "198.51.100.9"},
		{"chain of trusted proxies", []*net.IPNet{lb}, "10.0.0.2:5000", []string{"198.51.100.9, 10.0.0.5"}, "198.51.100.9"},
		{"multiple headers", []*net.IPNet{lb}, "10.0.0.2:5000", []string{"1.2.3.4", "198.51.100.9"}, "198.51.100.9"},
		{"garbage hop stops the walk", []*net.IPNet{lb}, "10.0.0.2:5000", []string{"198.51.100.9, nope, 10.0.0.5"}, "10.0.0.5"},
		{"trusted lb without xff", []*net.IPNet{lb}, "10.0.0.2:5000", nil, "10.0.0.2"},
		{"ipv6 remote", nil, "[2001:db8::1]:5000", nil, "2001:db8::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := NewResolver(tc.trusted).IP(r); got != tc.want {
				t.Errorf("IP = %q, want %q", got, tc.want)
			}
		})
	}
}
