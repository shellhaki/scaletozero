<div align="center">

# SCALE-TO-ZERO

### *Spin up databases on demand. Kill them when nobody's looking. Save money while you sleep.*

<img src="https://media.giphy.com/media/3oKIPnAiaMCws8nOsE/giphy.gif" width="420" alt="magic hands gif" />

<br/>

![Go](https://img.shields.io/badge/Go-00ADD8?style=for-the-badge&logo=go&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-2496ED?style=for-the-badge&logo=docker&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-4169E1?style=for-the-badge&logo=postgresql&logoColor=white)
![Traefik](https://img.shields.io/badge/Traefik-24A1C1?style=for-the-badge&logo=traefikproxy&logoColor=white)

![status](https://img.shields.io/badge/status-works%20on%20my%20machine-success?style=flat-square)
![bugs](https://img.shields.io/badge/bugs-features%20in%20disguise-orange?style=flat-square)
![coffee](https://img.shields.io/badge/powered%20by-caffeine-6F4E37?style=flat-square)

</div>

---

## What Is This Thing?

You know that friend who only shows up when there's free food? **Scale-to-Zero** is the responsible opposite.

It's a Go service that talks to the Docker Engine API and **provisions database containers on demand** — Postgres today, more tomorrow — hands each one a unique port, boots it up, and (eventually) puts it back to sleep when traffic hits zero. No traffic, no containers, no cloud bill making you cry into your cereal.

> **TL;DR:** Databases that exist only when someone actually needs them. Like a fridge light.

<div align="center">
<img src="https://media.giphy.com/media/JIX9t2j0ZTN9S/giphy.gif" width="360" alt="cat typing gif" />
</div>

---

## Features (a.k.a. Things That Currently Work)

| Feature | Status | Vibe |
|---------|--------|------|
| Create Postgres containers via Docker API | Working | *chef's kiss* |
| Unique random port assignment (10000–65535) | Working | crypto-secure, no less |
| Auto-start container after creation | Working | it's alive |
| Basic-auth protected Docker socket proxy | Working | no gremlins allowed |
| Delete / stop / pause containers | Coming | patience, young dev |
| Actual scale-**to**-zero logic | Coming | the name is aspirational for now |

<div align="center">
<img src="https://media.giphy.com/media/13HgwGsXF0aiGY/giphy.gif" width="320" alt="works in production gif" />
</div>

---

## How It Works

```
   ┌──────────────┐      Basic Auth       ┌───────────────────┐      unix socket      ┌──────────────┐
   │  main.go     │ ───── HTTPS ────────▶ │  docker-proxy      │ ──────────────────▶  │ Docker Engine │
   │ (the brains) │   docker.sparkdb.pro  │ (socket-proxy +    │   /var/run/docker    │  (the muscle) │
   └──────────────┘                       │  Traefik + auth)   │                      └──────┬───────┘
          │                               └───────────────────┘                              │
          │ 1. pick a lucky port                                                              ▼
          │ 2. POST /containers/create                                          ┌──────────────────────┐
          │ 3. POST /containers/{id}/start                                      │  postgres:latest      │
          ▼                                                                     │  living its best life │
   PortManager                                                                 └──────────────────────┘
```

The `docker-proxy` (see [`docker-compose.yml`](./docker-compose.yml)) is the bouncer. It exposes **only** the Docker API endpoints we allow (`CONTAINERS`, `IMAGES`, `NETWORKS`, `VOLUMES`, `EXEC`...) behind basic auth, so nobody yeets your whole host by knowing one URL.

---

## Quick Start

### 1. Prerequisites
- Go 1.21+
- Docker + Docker Compose
- A healthy disrespect for cloud bills

### 2. Fire up the Docker socket proxy

```bash
docker compose up -d docker-proxy
```

This starts [`tecnativa/docker-socket-proxy`](https://github.com/Tecnativa/docker-socket-proxy) — the safe way to expose the Docker API without handing over the keys to the kingdom.

### 3. Set your environment

Create a `.env` in the project root:

```env
DOCKER_API_URL=https://docker.sparkdb.pro
DOCKER_API_USERNAME=haki
DOCKER_API_PASSWORD=your-super-secret-password
```

> **Do NOT** commit your `.env`. It's in `.gitignore` for a reason. Future-you says thanks.

### 4. Run it

```bash
go run main.go
```

Watch the logs. If a Postgres container spins up, you win.

<div align="center">
<img src="https://media.giphy.com/media/26u4cqiYI30juCOGY/giphy.gif" width="320" alt="celebration gif" />
</div>

---

## Project Layout

```
scaletozero/
├── main.go                       # entrypoint — creates & starts a Postgres container
├── docker-compose.yml            # the docker-socket-proxy bouncer
├── config/
│   └── config.go                 # loads .env, yells if something's missing
├── internals/
│   └── shared/
│       ├── models.go             # Docker API payload structs
│       └── portManager.go        # crypto-random unique port picker
└── readme.md                     # you are here
```

---

## Configuration

| Variable | Required | What It Does |
|----------|:--------:|--------------|
| `DOCKER_API_URL` | yes | Where the Docker socket proxy lives |
| `DOCKER_API_USERNAME` | yes | Basic-auth user |
| `DOCKER_API_PASSWORD` | yes | Basic-auth password (keep it secret, keep it safe) |

Miss one and `config.Load()` will politely refuse to continue. It has boundaries.

---

## Roadmap

- [x] Create + start Postgres containers
- [x] Unique, collision-free port assignment
- [x] Locked-down Docker socket proxy
- [ ] Stop / pause / delete endpoints
- [ ] The *actual* scale-to-zero part (idle detection → sleep)
- [ ] Wake-on-request (cold start when traffic returns)
- [ ] More engines: MySQL, MongoDB, Redis
- [ ] Metrics + health checks

<div align="center">
<img src="https://media.giphy.com/media/xT9C25UNTwfZuk85WP/giphy.gif" width="300" alt="soon gif" />
</div>

---

## Known "Features"

- `panic()` is currently our error-handling strategy of choice. It's very direct. We respect that. It's on the list.
- The name says "scale to zero" but the zero-scaling isn't wired up yet. We're building the *scale* first, then the *zero*. Order matters.

---

<div align="center">

### Made with Go and an unreasonable dislike for idle cloud instances.

*If this saved you money, tell your accountant. If it didn't, tell nobody.*

</div>
