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
