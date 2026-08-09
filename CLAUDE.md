# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go mod tidy                       # required on a fresh clone, see below
go build -o matter2mqtt .
go test ./...
go test -run TestTopicSegmentKeepsTheLabelVerbatim ./...   # single test
go test -run 'TestConsole.*' -v ./...                      # by pattern
gofmt -l .                        # must print nothing
go vet ./...
docker compose up -d --build
```

`go mod tidy` is not optional on a fresh clone: since Go 1.18 `go mod download`
no longer records module content hashes, so a `go.sum` produced by download
alone is incomplete and the build fails with `missing go.sum entry`. The
Dockerfile runs `tidy` for the same reason.

Running the binary locally reads `.env` from the working directory (see
Configuration below), so `./matter2mqtt` picks up the checked-out dev settings.

Regenerating the Matter data model requires a running matter-server container:

```bash
docker exec -i matter-server python3 - < tools/dump-model.py > model.json
```

The `-i` matters — without it docker does not forward stdin and you get an
empty file.

## Architecture

One flat `package main`. The dependency direction is
`matterws.go → bridge.go → {MQTT, httpserver.go}`, with `model.go` and
`naming.go` as pure lookup/translation helpers used by the bridge.

- **`matterws.go`** — the python-matter-server WebSocket transport, and the only
  file that knows the wire protocol. `MatterClient` is a *single disposable
  session*: it is never reused across reconnects. When the socket dies, `Events`
  closes, `Done()` fires, and the caller dials a fresh one. Request/response
  correlation is by `message_id` through the `pending` map; unsolicited frames
  with an `event` field go to `Events`.
- **`bridge.go`** — all translation and all state. Owns the reconnect loop, the
  live fabric snapshot (`nodes`), MQTT publish/subscribe, and the console's SSE
  fan-out. This is where nearly all behaviour lives (~800 lines).
- **`model.go`** — numeric Matter ids ↔ names, from `model.json` embedded with
  `go:embed` (`MATTER_MODEL_PATH` overrides at runtime). The shipped file is a
  partial seed. Global attributes (`FeatureMap`, `ClusterRevision`, …) are
  hardcoded and merged in at lookup time, so they resolve for clusters absent
  from the model.
- **`naming.go`** — NodeLabel → `named/` topic segment. See invariants below.
- **`httpserver.go` + `web/index.html`** — the console. The HTML is a single
  embedded file: no build step, no CDN, no network at runtime. Every mutating
  route funnels through `Bridge.Invoke`, so console and MQTT share one path.
- **`nodered/`** — a separate npm package (`node-red-contrib-matter2mqtt`), not
  part of the Go build. Consumes the bridge over MQTT like any other client:
  `cd nodered && npm test` (`node --test`, no broker needed). Its scaling table
  is keyed by Matter cluster id, which is where the unit conversion is defined —
  it is spec-mandated per cluster, never vendor-specific.

### Invariants worth preserving

**The numeric tree is canonical; `named/` is a best-effort mirror.** An alias is
published only when *both* the cluster and the attribute resolve in the model
(`Model.NamePath` returns ok=false otherwise). A partial model must yield fewer
aliases, never a half-numeric path.

**NodeLabel (`0/40/5`) is the only source of a name.** No local alias store, no
fallback to ProductName or node id. A device without a label is simply absent
from `named/` while staying complete on the numeric tree.

**The label becomes the topic segment verbatim** — case, spaces, punctuation and
accents preserved (`TEMP Ikea` stays `TEMP Ikea`). Only what MQTT cannot carry
in a topic *name* is rewritten: `/`, `+`, `#` → `-`, and control characters
dropped. Do not reintroduce slugging/lowercasing. Because `Namer.Lookup` folds
case for command topics, collisions are detected case-insensitively and *every*
member of a colliding set is suffixed with its node id (`Sensor-4`,
`Sensor-9`) — never one keeping the bare name.

**Retained topics are tracked per node** in `Bridge.topics` so a rename, a lost
label, or a node removal can clear them by publishing empty retained payloads.
Any new retained publish should go through `publishTracked`, not `publish`.

**Events are dropped, never blocked.** Both the WebSocket→bridge channel and the
bridge→console SSE fan-out use non-blocking sends. Losing an update beats
stalling the read pump and losing the connection.

**`bridge/request/<command>` is a generic passthrough** to any matter-server
command, so new matter-server API surface works without a code change here.
Prefer extending that over adding bespoke topics.

## Configuration

Environment only — the service is stateless and container-friendly. `LoadDotEnv`
in `config.go` reads `.env` (or `ENV_FILE`) into the process environment before
`LoadConfig`, using compose `env_file` semantics: `#` comments only at line
start, no inline comments, `export ` tolerated, quotes stripped. **The real
environment always wins** over the file. A missing default `.env` is a silent
no-op; a missing `ENV_FILE` is a startup error.

`LoadConfig` fails startup — rather than warning — when `HTTP_ADDR` is
non-loopback without `HTTP_USERNAME`/`HTTP_PASSWORD`, since the console can
commission and remove devices. Keep that check intact.

`.env` is untracked and there is no `.gitignore`; it is the natural home for
`MQTT_PASSWORD` / `HTTP_PASSWORD`, so avoid `git add -A`.

## Tests

No broker and no controller are needed. `publish` is a no-op when `b.mqtt` is
nil, so tests exercise real publish paths and then assert against `b.topics`
directly. `fakeServer` / `connectFake` (in `matterws_test.go`,
`httpserver_test.go`) stand up a WebSocket fake for anything needing a live
matter-server session. `LoadModel("")` gives the embedded seed model.
