package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Result es la decisión del limitador para un request.
type Result struct {
	Allowed    bool
	Remaining  int           // requests que quedan en la ráfaga
	RetryAfter time.Duration // si no se permitió: cuándo reintentar
	ResetAfter time.Duration // cuándo se recupera la ráfaga completa
}

// Limiter decide si un request identificado por key entra dentro de limit.
type Limiter interface {
	Allow(ctx context.Context, key string, limit Limit) (Result, error)
}

// gcraScript implementa GCRA (generic cell rate algorithm), equivalente a un
// token bucket sin tareas de recarga. Guarda por key un solo número, el TAT
// (theoretical arrival time, en ms), con TTL hasta que vence. Es atómico: el
// script corre entero en Redis, así que varias réplicas del gateway
// comparten el límite sin carreras.
//
//	KEYS[1] = key
//	ARGV[1] = ahora (ms)   ARGV[2] = emisión (ms)   ARGV[3] = período (ms)
//
// Devuelve {permitido (0|1), restantes, reintentar_en_ms, reset_en_ms}.
var gcraScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local emission = tonumber(ARGV[2])
local period = tonumber(ARGV[3])

local tat = now
local stored = redis.call('GET', KEYS[1])
if stored then
  tat = math.max(tonumber(stored), now)
end

local new_tat = tat + emission
local allow_at = new_tat - period
if now < allow_at then
  return {0, 0, allow_at - now, tat - now}
end

redis.call('SET', KEYS[1], string.format('%d', new_tat), 'PX', new_tat - now)
local remaining = math.floor((period - (new_tat - now)) / emission)
return {1, remaining, 0, new_tat - now}
`)

// RedisLimiter implementa Limiter con GCRA en Redis.
type RedisLimiter struct {
	client redis.Scripter
	now    func() time.Time
}

// NewRedisLimiter crea el limitador. Usa el reloj del gateway: entre réplicas,
// una diferencia de reloj de pocos ms solo corre el límite en ese margen.
func NewRedisLimiter(client redis.Scripter) *RedisLimiter {
	return &RedisLimiter{client: client, now: time.Now}
}

func (l *RedisLimiter) Allow(ctx context.Context, key string, limit Limit) (Result, error) {
	res, err := gcraScript.Run(ctx, l.client, []string{key},
		l.now().UnixMilli(), limit.emission().Milliseconds(), limit.Period.Milliseconds(),
	).Int64Slice()
	if err != nil {
		return Result{}, fmt.Errorf("rate limit %s: %w", key, err)
	}
	if len(res) != 4 {
		return Result{}, fmt.Errorf("rate limit %s: respuesta inesperada %v", key, res)
	}
	return Result{
		Allowed:    res[0] == 1,
		Remaining:  int(res[1]),
		RetryAfter: time.Duration(res[2]) * time.Millisecond,
		ResetAfter: time.Duration(res[3]) * time.Millisecond,
	}, nil
}
