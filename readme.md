<div align="center">

# SCALE-TO-ZERO

### *Spin up databases on demand. Kill them when nobody's looking. Save money while you sleep.*

<img src="https://media.giphy.com/media/3oKIPnAiaMCws8nOsE/giphy.gif" width="420" alt="magic hands gif" />

<br/>

![Go](https://img.shields.io/badge/Go-00ADD8?style=for-the-badge&logo=go&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-2496ED?style=for-the-badge&logo=docker&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-4169E1?style=for-the-badge&logo=postgresql&logoColor=white)
![Traefik](https://img.shields.io/badge/Traefik-24A1C1?style=for-the-badge&logo=traefikproxy&logoColor=white)
![SQLite](https://img.shields.io/badge/SQLite-003B57?style=for-the-badge&logo=sqlite&logoColor=white)

![status](https://img.shields.io/badge/status-works%20on%20my%20machine-success?style=flat-square)
![bugs](https://img.shields.io/badge/bugs-features%20in%20disguise-orange?style=flat-square)
![coffee](https://img.shields.io/badge/powered%20by-caffeine-6F4E37?style=flat-square)

</div>

---

## What Is This Thing?

You know that friend who only shows up when there's free food? **Scale-to-Zero** is the responsible opposite.

It provisions database containers on demand, then **drops the compute when nobody is connected** — keeping the volume, so your data stays put. The moment a client connects, it wakes the database back up and forwards the connection like nothing happened.

> **TL;DR:** Databases that exist only when someone actually needs them. Like a fridge light.

<div align="center">
<img src="https://media.giphy.com/media/JIX9t2j0ZTN9S/giphy.gif" width="360" alt="cat typing gif" />
</div>

---

## The Trick

Your client connects normally. It has no idea any of this is happening.

```
psql postgres://haki:pw@postgres.sparkdb.pro:5432/mydb
                                                 └──── database name lives here
```

1. Connection lands on the **TCP proxy** (`:5432`).
2. Proxy reads the Postgres **StartupMessage** and pulls out `database=mydb`. That's the routing key — plaintext Postgres has no SNI, so the packet itself tells us where to go.
3. Look up `mydb`. **Running?** Forward immediately. **Scaled to zero?** Start the container, wait until it actually serves, *then* forward.
4. The handshake bytes we consumed for routing get **replayed** to the backend, so the database sees an unbroken protocol stream.
5. Both directions are spliced together with `io.Copy` — on Linux this hits `splice(2)`, so payload bytes never enter user space.
6. **2 minutes idle with zero open connections** → compute dropped. Volume kept.

Cold start costs one container start, not an `initdb` — the data directory is initialised eagerly at provision time.

---

## Architecture

```
                          ┌──────────────────────────────────────────┐
                          │              traefik                     │
   psql ──── :5432 ──────▶│  TCP entrypoint (HostSNI *)              │
   curl ──── :443  ──────▶│  HTTPS + letsencrypt + basic auth        │
                          └───────────────┬──────────────────────────┘
                                          │
                          ┌───────────────▼──────────────────────────┐
                          │              sparkdb                     │
                          │                                          │
                          │  ┌────────────────┐  ┌────────────────┐  │
                          │  │  TCP proxy     │  │  admin API     │  │
                          │  │  :5432         │  │  :8080         │  │
                          │  └───────┬────────┘  └───────┬────────┘  │
                          │          └────────┬──────────┘           │
                          │              ┌────▼─────┐                │
                          │              │ manager  │ singleflight   │
                          │              │          │ wake + reaper  │
                          │              └────┬─────┘                │
                          │        ┌──────────┴──────────┐           │
                          │   ┌────▼────┐          ┌─────▼─────┐     │
                          │   │ sqlite  │          │  docker   │     │
                          │   │ (state) │          │  client   │     │
                          │   └─────────┘          └─────┬─────┘     │
                          └──────────────────────────────┼───────────┘
                                                         │ internal only
                                          ┌──────────────▼───────────┐
                                          │   docker-socket-proxy    │
                                          └──────────────┬───────────┘
                                                         │
                          ┌──────────────────────────────▼───────────┐
                          │   postgres containers (one per database) │
                          │   reached by NAME on sparkdb-network     │
                          └──────────────────────────────────────────┘
```

**No host ports are published.** Containers are reached by name over the Docker network, so there's no port range to exhaust and no database accidentally exposed to the host.

**The Docker API is never exposed publicly.** `sparkdb` talks to the socket proxy over the internal network. Nothing in the compose file routes it to the internet.

---

## Any Database, Not Just Postgres

The scale-to-zero core is **engine agnostic**. Adding a database means implementing one interface:

```go
type Engine interface {
    Name() string
    Image() string
    InternalPort() int
    DataDir() string
    Env(user, password, database string) []string
    Routing() RoutingMode
    Resolve(rw io.ReadWriter) (database string, replay []byte, err error)
    Ready(ctx context.Context, addr string) error
}
```

There's a catch worth knowing about, and it's protocol-level, not a design choice:

| Protocol style | Examples | Routing |
|---|---|---|
| **Client speaks first** | Postgres | `SharedPortStartupRouting` — all databases share one port, target read from the first packet |
| **Server speaks first** | MySQL, MongoDB | `PortPerDatabase` — the server would have to greet the client *before* the client names a database, so routing happens by port instead |

Postgres is implemented. The interface supports both modes so the rest slot in without touching the core.

---

## Quick Start

### 1. Prerequisites
- Docker + Docker Compose
- A domain pointed at your host (`api.` and `postgres.` subdomains → same IP)
- A healthy disrespect for cloud bills

### 2. Configure

```bash
cp .env.example .env
```

Fill in `ACME_EMAIL` and `API_BASIC_AUTH` (generate with `htpasswd -nbB admin 'your-password'`, then **double every `$`**).

### 3. Launch

```bash
docker compose up -d --build
```

### 4. Provision a database

```bash
curl -u admin:your-password -X POST https://api.sparkdb.pro/api/postgres/create \
  -H 'Content-Type: application/json' \
  -d '{"name":"mydb","username":"haki","password":"hunter2"}'
```

It comes back already scaled to zero — volume initialised, compute off.

### 5. Just connect

```bash
psql postgres://haki:hunter2@postgres.sparkdb.pro:5432/mydb
```

First connection wakes it. Two idle minutes later it goes back to sleep. You do nothing.

<div align="center">
<img src="https://media.giphy.com/media/26u4cqiYI30juCOGY/giphy.gif" width="320" alt="celebration gif" />
</div>

---

## Control Plane

All lifecycle routes are engine-scoped, so `postgres` becomes `mysql` the day MySQL lands.

| Method | Route | Body | Does |
|---|---|---|---|
| `POST` | `/api/postgres/create` | `{name, username, password}` | Volume + container + eager init, then sleep |
| `POST` | `/api/postgres/start` | `{name}` | Wake now |
| `POST` | `/api/postgres/stop` | `{name}` | Scale to zero now |
| `POST` | `/api/postgres/pause` | `{name}` | Freeze processes (Docker freezer) |
| `POST` | `/api/postgres/delete` | `{name}` | Remove container **and volume** — destroys data |
| `GET` | `/api/postgres/list` | — | Databases for this engine |
| `GET` | `/api/databases` | — | Every database |
| `GET` | `/health` | — | Liveness |

Passwords are stored for container provisioning but **never returned** by the API.

---

## Configuration

| Variable | Default | What It Does |
|---|---|---|
| `DOCKER_API_URL` | *(required)* | Socket proxy address |
| `DOCKER_API_USERNAME` / `_PASSWORD` | *(empty)* | Only for an authenticated public Docker endpoint. Both or neither |
| `DOCKER_NETWORK` | `sparkdb-network` | Network containers join, and how the proxy reaches them |
| `SQLITE_PATH` | `sparkdb.db` | Metadata + state |
| `IDLE_TIMEOUT` | `2m` | Idle time before compute is dropped |
| `REAP_INTERVAL` | `15s` | How often the reaper scans |
| `WAKE_TIMEOUT` | `60s` | How long to wait for a woken container to serve |
| `PORT` | `:8080` | Admin API |
| `PG_LISTEN_ADDR` | `:5432` | Postgres data plane |

---

## Behaviour Worth Knowing

- **An open connection is never cut.** The reaper refuses to scale down while any connection is live, no matter how idle.
- **A connection burst on a cold database triggers exactly one start.** Concurrent waking connections coalesce and queue on the same start.
- **State survives restarts.** The manager hydrates from SQLite on boot.
- **Unroutable connections get a real error**, not a dropped socket — `psql` prints `database "ghost" does not exist` (SQLSTATE `3D000`).
- **`delete` destroys the volume.** That is the only destructive operation here; `stop` and `pause` always keep data.

---

## Tests

```bash
cd server && go test ./...
```

118 tests: Postgres wire-protocol parsing (SSL/GSS negotiation, cancel requests, malformed packets), store persistence, Docker API call shapes, wake coalescing under concurrency, reaper rules, and end-to-end proxy forwarding.

> The race detector needs cgo, so it hasn't been run on a toolchain without a C compiler. Worth doing on Linux CI: `CGO_ENABLED=1 go test -race ./...`

---

## Roadmap

- [x] Provision Postgres with a persistent volume
- [x] Scale to zero on idle, wake on connection
- [x] Connection-aware reaping (never cuts a live client)
- [x] Engine interface covering both routing modes
- [x] Docker API kept off the public internet
- [ ] MySQL (proves the `PortPerDatabase` path)
- [ ] Redis, MongoDB
- [ ] Metrics: wake latency, cold-start counts, idle savings
- [ ] Connection pooling to shave the cold start further

<div align="center">
<img src="https://media.giphy.com/media/xT9C25UNTwfZuk85WP/giphy.gif" width="300" alt="soon gif" />
</div>

---

<div align="center">

### Made with Go and an unreasonable dislike for idle cloud instances.

*If this saved you money, tell your accountant. If it didn't, tell nobody.*

</div>
