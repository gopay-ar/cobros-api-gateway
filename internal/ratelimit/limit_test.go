package ratelimit

import (
	"testing"
	"time"
)

func TestParseLimit(t *testing.T) {
	valid := map[string]Limit{
		"10/1m":    {Count: 10, Period: time.Minute},
		" 5/1h ":   {Count: 5, Period: time.Hour},
		"300/30s":  {Count: 300, Period: 30 * time.Second},
		"1000/1s":  {Count: 1000, Period: time.Second},
		"10/1m30s": {Count: 10, Period: 90 * time.Second},
	}
	for raw, want := range valid {
		if got, err := ParseLimit(raw); err != nil || got != want {
			t.Errorf("ParseLimit(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}

	for _, raw := range []string{"", "10", "0/1m", "-1/1m", "x/1m", "10/", "10/abc", "10/-1m", "2000/1s"} {
		if _, err := ParseLimit(raw); err == nil {
			t.Errorf("ParseLimit(%q): expected error", raw)
		}
	}
}

// Los logs muestran el límite como se configura, no "10/15m0s".
func TestLimit_String(t *testing.T) {
	cases := map[Limit]string{
		{Count: 10, Period: 15 * time.Minute}:             "10/15m",
		{Count: 5, Period: time.Hour}:                     "5/1h",
		{Count: 1, Period: 90 * time.Minute}:              "1/1h30m",
		{Count: 300, Period: 30 * time.Second}:            "300/30s",
		{Count: 10, Period: time.Minute + 30*time.Second}: "10/1m30s",
		{Count: 2, Period: time.Hour + 30*time.Second}:    "2/1h0m30s",
		{Count: 1000, Period: time.Second}:                "1000/1s",
	}
	for l, want := range cases {
		if got := l.String(); got != want {
			t.Errorf("%#v.String() = %q, want %q", l, got, want)
		}
	}
}
