# HTTP tests

`.http` files for the VS Code **REST Client** extension (and JetBrains HTTP
Client). Click **Send Request** above any `###` block.

## Setup

Variables (`{{host}}`, `{{engine}}`, `{{dbName}}`, …) come from
[`http-client.env.json`](./http-client.env.json). Pick an environment:

- **VS Code REST Client:** click the env name in the status bar (bottom right),
  choose `local` or `prod`.
- **JetBrains:** select the environment from the run gutter.

`local` targets `http://localhost:8080` (the dev stack). `prod` targets
`https://api.sparkdb.pro`.

## Files

| File | What it covers |
|------|----------------|
| `00-flow.http` | Full scale-to-zero walkthrough, top to bottom |
| `01-admin.http` | Root, health, listing |
| `02-provision.http` | Creating databases |
| `03-lifecycle.http` | start / stop / pause / delete |
| `04-errors.http` | Validation, 404, 409, unknown engine |

## Before running

Bring up the dev stack so the API is reachable and `create` actually works
(sparkdb runs inside the Docker network, database containers reachable by name):

```bash
docker compose -f docker-compose.dev.yml up -d --build
```

## Note on `prod`

Production is behind Traefik basic auth. Add credentials to each request, e.g.:

```
Authorization: Basic {{$processEnv API_BASIC_AUTH_B64}}
```

or just append `-u admin:password` when reproducing with `curl`.

## The database connection itself

Connecting to a provisioned database is raw TCP (the Postgres wire protocol),
not HTTP — it cannot be expressed in a `.http` file. Use `psql`:

```bash
psql postgres://haki:hunter2@localhost:5432/mydb
```

That connection is what wakes a scaled-to-zero database.
