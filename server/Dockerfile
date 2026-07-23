# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.26-alpine AS build

WORKDIR /src

# Cache dependencies separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO stays off: the SQLite driver is pure Go, so the binary is static and
# runs on a bare image.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/sparkdb ./cmd

# ---- runtime ----
FROM alpine:3.20

# Certificates are needed only when the Docker API is reached over HTTPS.
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 sparkdb \
    && mkdir -p /data \
    && chown sparkdb:sparkdb /data

COPY --from=build /out/sparkdb /usr/local/bin/sparkdb

USER sparkdb
WORKDIR /data

# 8080 control plane, 5432 Postgres data plane.
EXPOSE 8080 5432

ENTRYPOINT ["/usr/local/bin/sparkdb"]
