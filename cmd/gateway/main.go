package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/juantevez/api-gateway-cobros/internal/config"
	"github.com/juantevez/api-gateway-cobros/internal/proxy"
	"github.com/juantevez/api-gateway-cobros/internal/ratelimit"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuración inválida", "error", err)
		os.Exit(1)
	}

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		logger.Error("REDIS_URL inválida")
		os.Exit(1)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()

	// Redis caído al arrancar no impide levantar: con fail-open el gateway
	// sirve sin límite hasta que vuelva, y /gateway/health/ready lo informa.
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 3*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		logger.Warn("redis no disponible al arrancar", "fail_open", cfg.RateLimitFailOpen, "error", err)
	}
	cancelPing()

	handler := proxy.NewHandler(proxy.Deps{
		Config:  cfg,
		Limiter: ratelimit.NewRedisLimiter(rdb),
		Redis:   redisPinger{rdb},
		Logger:  logger,
	})

	// Los timeouts protegen contra clientes lentos (slowloris).
	server := &http.Server{
		Addr:              cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown: ante SIGINT/SIGTERM deja de aceptar conexiones y
	// espera hasta 10 s a que terminen los requests en curso.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("api gateway escuchando", "addr", cfg.Port, "upstream", cfg.UpstreamURL)
		serverErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("servidor detenido", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("señal recibida, apagando")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("error durante el apagado", "error", err)
	}
	logger.Info("apagado correctamente")
}

type redisPinger struct{ c *redis.Client }

func (p redisPinger) Ping(ctx context.Context) error { return p.c.Ping(ctx).Err() }
