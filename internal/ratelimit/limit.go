package ratelimit

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Limit es "Count requests por Period". El limitador permite ráfagas de hasta
// Count requests y después uno cada Period/Count.
type Limit struct {
	Count  int
	Period time.Duration
}

// String usa el mismo formato que ParseLimit: "10/15m", no "10/15m0s".
func (l Limit) String() string { return fmt.Sprintf("%d/%s", l.Count, shortDuration(l.Period)) }

// shortDuration quita los ceros finales de time.Duration.String:
// 15m0s → 15m, 1h0m0s → 1h, 1h30m0s → 1h30m.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// emission es el intervalo entre requests a ritmo sostenido.
func (l Limit) emission() time.Duration { return l.Period / time.Duration(l.Count) }

// ParseLimit lee "N/duración", p. ej. "10/1m", "300/1m", "5/1h".
func ParseLimit(raw string) (Limit, error) {
	count, period, ok := strings.Cut(strings.TrimSpace(raw), "/")
	if !ok {
		return Limit{}, errors.New(`formato esperado "N/duración", p. ej. "10/1m"`)
	}
	n, err := strconv.Atoi(count)
	if err != nil || n <= 0 {
		return Limit{}, fmt.Errorf("cantidad inválida %q", count)
	}
	d, err := time.ParseDuration(period)
	if err != nil || d <= 0 {
		return Limit{}, fmt.Errorf("duración inválida %q", period)
	}
	l := Limit{Count: n, Period: d}
	// El limitador trabaja en milisegundos.
	if l.emission() < time.Millisecond {
		return Limit{}, fmt.Errorf("%s supera 1000 requests por segundo", l)
	}
	return l, nil
}
