# api-gateway-cobros

API gateway de [cobros-platform](../cobros-platform). Escucha en el puerto **8000**, aplica **rate limiting con Redis** y reenvía todo a la API de cobros (`:8080`), que sigue siendo quien autentica y autoriza.

```
cliente ──► gateway :8000 ──(rate limit en Redis)──► cobros-platform API :8080
```

## Rate limiting

Cada request pasa por estas reglas, en orden. La primera que se supera corta el request con `429 Too Many Requests` y el request no llega a cobros.

| Regla | Aplica a | Clave | Default | Variable |
|---|---|---|---|---|
| `login_account` | `POST /api/v1/auth/login` | hash de `tenant_id` + `email` del body | `10/15m` | `RATE_LIMIT_LOGIN_ACCOUNT` |
| `login_ip` | `POST /api/v1/auth/login` | IP | `20/1m` | `RATE_LIMIT_LOGIN_IP` |
| `refresh_ip` | `POST /api/v1/auth/refresh` | IP | `60/1m` | `RATE_LIMIT_REFRESH_IP` |
| `signup_ip` | `POST /api/v1/tenants` (alta pública) | IP | `5/1h` | `RATE_LIMIT_SIGNUP_IP` |
| `tenant` | requests con JWT válido o `X-Api-Key` | `tenant:<tid>` / `apikey:<prefix>` | `1200/1m` | `RATE_LIMIT_TENANT` |
| `global_ip` | todo request | IP | `300/1m` | `RATE_LIMIT_GLOBAL_IP` |

- **Login:** el límite por cuenta frena la fuerza bruta distribuida en muchas IPs, y el límite por IP frena el *password spraying* (una IP probando muchas cuentas). El gateway lee el body del login para obtener la cuenta y lo reenvía intacto. En Redis guarda un hash, nunca el email.
- **Tenant:** el gateway verifica la firma del JWT con el mismo `JWT_SECRET` de cobros (HS256). Sin esa verificación, cualquiera podría gastar el cupo de otro tenant poniendo su `tid`. Con un JWT inválido o sin credenciales, el request solo cuenta por IP y cobros responde 401. Las API keys se cuentan por su `prefix`; el secreto lo valida cobros con Argon2.
- **Formato de los límites:** `N/duración`, por ejemplo `10/1m`, `300/30s` o `5/1h`. Se permiten ráfagas de hasta `N` requests y después uno cada `duración/N`.

### Algoritmo

GCRA (*generic cell rate algorithm*), equivalente a un token bucket. Corre como script Lua en Redis ([limiter.go](internal/ratelimit/limiter.go)):

- **Atómico:** varias réplicas del gateway comparten el límite sin carreras.
- **Una sola clave por llamador:** `rl:<regla>:<clave>`, con TTL hasta que se recupera el cupo, así Redis no acumula claves viejas.
- **Los rechazos no consumen cupo:** quien insiste con 429 no alarga su propia espera.

### Respuestas

Headers del draft de la IETF (`draft-ietf-httpapi-ratelimit-headers`). En los requests permitidos se informa la regla con menos margen.

```
HTTP/1.1 429 Too Many Requests
RateLimit-Limit: 10
RateLimit-Remaining: 0
RateLimit-Reset: 900
Retry-After: 90

{"error":"too many requests"}
```

### Si Redis se cae

`RATE_LIMIT_FAIL_OPEN=true` (el default): el gateway sigue sirviendo **sin límite** y lo loguea, porque una caída de Redis no debe cortar los cobros. Con `false` responde `503`. `/gateway/health/ready` informa `"redis":"down"`, y responde 503 solo en modo *fail-closed*.

### IP del cliente

Por defecto se usa la IP de la conexión y se **ignora** `X-Forwarded-For`, que el cliente puede falsificar para rotar de IP en cada request. Si hay un balanceador delante, declarar sus redes en `TRUSTED_PROXIES` (`10.0.0.0/8,192.168.1.10`). En ese caso el gateway recorre `X-Forwarded-For` de derecha a izquierda y toma la primera IP que no es de confianza. Hacia cobros, `X-Forwarded-For` se reemplaza por esa IP.

## Configuración

| Variable | Default | Descripción |
|---|---|---|
| `GATEWAY_PORT` | `:8000` | Puerto del gateway |
| `COBROS_API_URL` | `http://127.0.0.1:8080` | API de cobros-platform |
| `REDIS_URL` | `redis://127.0.0.1:6379/0` | Redis (`rediss://` para TLS) |
| `JWT_SECRET` | — (obligatorio, ≥ 32) | El mismo de cobros-platform |
| `TRUSTED_PROXIES` | vacío | CIDR de los balanceadores |
| `RATE_LIMIT_FAIL_OPEN` | `true` | Qué hacer si Redis no responde |
| `RATE_LIMIT_*` | ver tabla | Límites de cada regla |

Una configuración inválida corta el arranque y muestra todos los errores juntos.

## Endpoints propios

| Ruta | Descripción |
|---|---|
| `GET /gateway/health/live` | El proceso está vivo. No consulta a cobros ni a Redis. |
| `GET /gateway/health/ready` | Estado de Redis. |

El resto de las rutas, incluido `/health` de cobros, se reenvía a cobros-platform.

## Uso local

Con cobros-platform levantado por su propio compose:

```bash
make up      # gateway + Redis en Docker; se une a la red cobros-platform_default
make logs
make down
```

El gateway en Docker llega a la API por la red de cobros (`http://api:8080`). Si cobros corre fuera de Docker (`make run-api`):

```bash
make redis   # solo Redis
make run     # gateway con go run, apuntando a 127.0.0.1:8080
```

Si el puerto 6379 del host está ocupado, usar `REDIS_HOST_PORT=6380 make up`.

## Tests

```bash
make test    # go test -race ./...
```

Los tests usan [miniredis](https://github.com/alicebob/miniredis), que ejecuta el script Lua real, con un reloj controlado: no necesitan un Redis levantado.

## Estructura

```
cmd/gateway/            main: config, Redis, servidor HTTP y graceful shutdown
internal/config/        variables de entorno y validación
internal/ratelimit/     Limit, limitador GCRA en Redis y middleware HTTP
internal/identity/      clave del llamador (tenant del JWT, API key, cuenta del login)
internal/clientip/      IP real del cliente (TRUSTED_PROXIES)
internal/proxy/         reglas, health checks y reverse proxy hacia cobros
deploy/docker/          Dockerfile y compose (proyecto "api-gateway-cobros")
```
