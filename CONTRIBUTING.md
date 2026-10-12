# Contributing

The self-hosted ITMO.Widgets agent: one static Go binary that long-polls the ITMO.Widgets Backend and books queued
sport lessons with the user's own My ITMO session. Keep it small: idle RSS under 15 MB, standard library first.

## Build and test

Go 1.26 or newer (`crypto/hpke`).

```sh
go vet ./...
go test -race ./...
go build -trimpath -ldflags "-s -w -X main.version=dev" ./cmd/itmo-widgets-agent
```

Cross-build check (what CI runs):

```sh
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath -ldflags "-s -w" -o /dev/null ./cmd/itmo-widgets-agent
done
docker build -t itmo-widgets-agent:dev .
```

Tests never reach the network: Backend, ITMO.ID, My ITMO and ntfy are `httptest` fakes in `internal/fake`, and the
clock is injected.

## Layout

| Path | What |
|---|---|
| `cmd/itmo-widgets-agent` | CLI: `run`, `pair [--session]`, `login`, `status`, `health`; exit codes |
| `internal/agent` | the poll loop: cursor, dedup ids, backoff, 401 wipe, ITMO session state, health and status |
| `internal/backend` | Backend client for the routes an agent token may call |
| `internal/itmo` | minimal ITMO.ID and My ITMO client (see below) |
| `internal/sport` | booking on `SPORT_*_SIGN_LESSONS_PAYLOAD` events |
| `internal/pair` | pairing: HPKE key, commitment, SAS, QR and link, bundle checks, `login` |
| `internal/state` | `agent-token`, `session.json`, `state.json`; atomic writes, mode 0600 |
| `internal/budget` | per-host request ceiling in the HTTP transport (60 per hour to ITMO) |
| `internal/notify` | ntfy notifications |
| `internal/config` | environment and `agent.env` |
| `deploy/` | launchd and systemd examples |

## Contracts

- Backend: `docs/contracts/agent.md` and `docs/contracts/pairing.md` in
  [itmo-widgets-backend](https://github.com/alllexey-dev/itmo-widgets-backend). Event payloads are the push payloads
  of `docs/contracts/notifications.md`.
- Pairing cryptography (HPKE suite, `info`, commitment, SAS, bundle) follows the ITMO.Widgets session hand-off design;
  `internal/pair/pair_test.go` holds an independent sender that must keep passing.

## The ITMO client exception

In the ITMO.Widgets ecosystem [MyItmoApi](https://github.com/alllexey123/MyItmoApi) is the only client for official
ITMO endpoints. The agent is an approved exception, because MyItmoApi targets only the JVM and iOS. `internal/itmo`
implements exactly three calls and nothing else:

- ITMO.ID token refresh (`refresh_token` grant, client `student-personal-cabinet`, plural `scopes`);
- `GET /api/sport/sign/chosen`;
- `POST /api/sport/sign/schedule/lessons`.

MyItmoApi stays the reference: when a wire detail changes, change it there first and mirror it here (envelope
`error_code`/`code`, 401 handling, the sign-up outcome mapping of `SportSignResult`). A new ITMO call needs the owner's
decision.

## Rules

- Never log tokens, refresh tokens, pairing secrets, names or lesson titles. `TestLogsNeverContainTokens` guards it.
- Never call ITMO.ID logout or revoke: the session is shared with the phone.
- No polling of My ITMO on a timer except the daily token keep-alive; act only on Backend events.
- Commit messages: one short line.

## Releases

Pushing a `v*` tag runs `.github/workflows/release.yml` after a reviewer approves the `release` environment: binaries
in a draft GitHub release and a multi-arch image on GHCR. Tags and publishing only on the owner's word.
