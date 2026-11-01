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

func (l Limit) String() string { return fmt.Sprintf("%d/%s", l.Count, l.Period) }

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
