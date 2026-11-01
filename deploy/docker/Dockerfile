# Dockerfile — build multi-stage del API gateway de cobros-platform.

FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cachear dependencias antes de copiar el código fuente.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/gateway ./cmd/gateway

FROM alpine:3.19

# wget: para el healthcheck de Docker. Corre como usuario sin privilegios.
RUN apk add --no-cache ca-certificates wget && adduser -D -H gateway
USER gateway

COPY --from=builder /bin/gateway /gateway

EXPOSE 8000
ENTRYPOINT ["/gateway"]
