package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestLimiter usa miniredis, que ejecuta el script Lua, con un reloj
// controlado por el test.
func newTestLimiter(t *testing.T) (*RedisLimiter, *time.Time, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	now := time.UnixMilli(1_700_000_000_000)
	l := NewRedisLimiter(client)
	l.now = func() time.Time { return now }
	return l, &now, mr
}

func TestRedisLimiter_BurstThenDeny(t *testing.T) {
	l, _, _ := newTestLimiter(t)
	ctx := context.Background()
	limit := Limit{Count: 3, Period: time.Minute}

	for i := 2; i >= 0; i-- {
		res, err := l.Allow(ctx, "k", limit)
		if err != nil || !res.Allowed || res.Remaining != i {
			t.Fatalf("request %d: res=%+v err=%v, want allowed with %d remaining", 3-i, res, err, i)
		}
	}

	res, err := l.Allow(ctx, "k", limit)
	if err != nil || res.Allowed {
		t.Fatalf("4th request: res=%+v err=%v, want denied", res, err)
	}
	// A ritmo sostenido entra uno cada 20 s.
	if res.RetryAfter != 20*time.Second {
		t.Errorf("retry after = %v, want 20s", res.RetryAfter)
	}
}

func TestRedisLimiter_RecoversOverTime(t *testing.T) {
	l, now, _ := newTestLimiter(t)
	ctx := context.Background()
	limit := Limit{Count: 2, Period: time.Second}

	for range 2 {
		_, _ = l.Allow(ctx, "k", limit)
	}
	if res, _ := l.Allow(ctx, "k", limit); res.Allowed {
		t.Fatal("burst exhausted: should deny")
	}

	*now = now.Add(500 * time.Millisecond) // un intervalo de emisión
	if res, _ := l.Allow(ctx, "k", limit); !res.Allowed {
		t.Fatal("after one emission interval: should allow one")
	}
	if res, _ := l.Allow(ctx, "k", limit); res.Allowed {
		t.Fatal("only one slot recovered")
	}

	*now = now.Add(time.Second) // ráfaga completa
	for i := range 2 {
		if res, _ := l.Allow(ctx, "k", limit); !res.Allowed {
			t.Fatalf("after full period, request %d denied", i+1)
		}
	}
}

// Un request rechazado no consume cupo: el que insiste no se castiga solo.
func TestRedisLimiter_DeniedRequestsDoNotConsume(t *testing.T) {
	l, now, _ := newTestLimiter(t)
	ctx := context.Background()
	limit := Limit{Count: 1, Period: time.Second}

	_, _ = l.Allow(ctx, "k", limit)
	for range 10 {
		_, _ = l.Allow(ctx, "k", limit)
	}
	*now = now.Add(time.Second)
	if res, _ := l.Allow(ctx, "k", limit); !res.Allowed {
		t.Fatal("denied attempts must not push the window forward")
	}
}

func TestRedisLimiter_KeysAreIndependent(t *testing.T) {
	l, _, _ := newTestLimiter(t)
	ctx := context.Background()
	limit := Limit{Count: 1, Period: time.Minute}

	_, _ = l.Allow(ctx, "a", limit)
	if res, _ := l.Allow(ctx, "b", limit); !res.Allowed {
		t.Fatal("key b must have its own budget")
	}
}

// La clave vence sola: Redis no acumula claves de IPs que no vuelven.
func TestRedisLimiter_KeyExpires(t *testing.T) {
	l, _, mr := newTestLimiter(t)
	_, _ = l.Allow(context.Background(), "k", Limit{Count: 10, Period: time.Minute})

	if ttl := mr.TTL("k"); ttl <= 0 || ttl > 6*time.Second {
		t.Errorf("ttl = %v, want the time until the slot is free (6s)", ttl)
	}
}

func TestRedisLimiter_RedisDown(t *testing.T) {
	l, _, mr := newTestLimiter(t)
	mr.Close()
	if _, err := l.Allow(context.Background(), "k", Limit{Count: 1, Period: time.Second}); err == nil {
		t.Fatal("expected an error with redis down")
	}
}
