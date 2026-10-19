# Contributing

The self-hosted ITMO.Widgets agent: one static Go binary that long-polls the ITMO.Widgets Backend and books queued
sport lessons with the user's own My ITMO session. Keep it small: idle RSS under 15 MB, standard library first.

## Build and test

Go 1.26 or newer (`crypto/hpke`).

```sh
make            # gofmt, go vet, go test -race, govulncheck, build: what CI runs
make cross      # binaries for every release target into dist/
make image      # the Docker image
```

Without make:

```sh
go vet ./...
go test -race ./...
go build -trimpath -ldflags "-s -w -X main.version=dev" ./cmd/itmo-widgets-agent
```

Build identity goes into `X-Client` on every Backend request (ADR 0034 in ITMO.Widgets):
`-X main.version`, `main.commit`, `main.channel` and `main.source`. A plain build says `channel=custom`,
`source=local` and takes the commit from the git checkout when Go stamps it; the Dockerfile takes the same as
`VERSION`, `COMMIT`, `CHANNEL` and `SOURCE` build args. Only `release.yml` sets `channel=github` and
`source=<owner/repo>`, so forks and self-built agents show apart from official releases.

Cross-build check (what CI runs):

```sh
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 freebsd/amd64 freebsd/arm64 windows/amd64; do
  CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath -ldflags "-s -w" -o /dev/null ./cmd/itmo-widgets-agent
done
docker build -t itmo-widgets-agent:dev .
```

Tests never reach the network: Backend, ITMO.ID, My ITMO and ntfy are `httptest` fakes in `internal/fake`, and the
clock is injected.

## Layout

| Path | What |
|---|---|
| `cmd/itmo-widgets-agent` | CLI: `run`, `pair [--session]`, `login`, `status`, `attach`, `unpair`, `health`, `--ui`; exit codes |
| `internal/agent` | the poll loop: cursor, dedup ids, backoff, 401 wipe, waiting for a token, ITMO session state, `unpair`, health and status; `Loop` tracks a running loop for the screen and the socket |
| `internal/control` | the running agent's Unix socket for `attach` and `status`: line-delimited JSON, owner-only |
| `internal/backend` | Backend client for the routes an agent token may call |
| `internal/itmo` | minimal ITMO.ID and My ITMO client (see below) |
| `internal/sport` | booking on `SPORT_*_SIGN_LESSONS_PAYLOAD` events |
| `internal/pair` | pairing: HPKE key, commitment, SAS, QR and link, bundle checks, `login` |
| `internal/state` | `agent-token`, `session.json`, `state.json`; atomic writes, mode 0600 |
| `internal/budget` | per-host request ceiling in the HTTP transport (60 per hour to ITMO) |
| `internal/notify` | ntfy notifications |
| `internal/config` | environment and `agent.env` |
| `internal/tui` | the terminal screen: pairing and status, plain ANSI over `golang.org/x/term` |
| `internal/redact` | masks token-like strings in every log line |
| `deploy/` | launchd, systemd and FreeBSD rc.d examples; `install.sh` fetches them from the release tag and fills in the paths |
| `install.sh`, `install.ps1` | one-command installers with a menu; `scripts/install-test.*` run them in CI against a fake release (`IW_RELEASE_BASE`) |

## The terminal screen

`run` and `pair` draw a screen when stdin and stdout are a terminal (`IW_UI` or `--ui` force `tui` or `plain`).
It is plain ANSI with `golang.org/x/term` for raw mode, not Bubble Tea: measured on 2026-10-14 (linux/arm64,
stripped), Bubble Tea v2 with Lip Gloss v2 added 1.5 MB to the binary, 15 modules and 5.6 MB of idle RSS
(10.4 MB vs 4.8 MB for the same screen), while x/term adds about 0.1 MB. The screen draws an `agent.Live` once a
second: `state.json` and `session.json` read from disk, the poll loop's own state and the last lines of an in-memory
log ring (text lines, redacted; in plain mode the logger writes JSON to stderr and the same ring). Screen captures in
`docs/screens/` are plain text; refresh them by hand when the layout changes.

`attach` draws the same screen from another process. Every `run` listens on `agent.sock` in the state directory
(`internal/control`; the directory is chmod 0700 before the socket exists, the socket 0600; on Windows the profile
folder's ACL). One JSON object per line:

- agent to client: `{"status":{...agent.Live...}}` on connect and whenever it changes (checked once a second), and
  `{"reply":{"cmd":"unpair","message":"...","error":"..."}}` after a command;
- client to agent: `{"cmd":"unpair"}`. The agent stops the loop, unpairs, and starts the loop again, which waits for a
  new token.

A new ITMO session or a new pairing is no command: `attach` runs the pairing screen itself, like `pair --session`
next to a running service, and the agent picks up the new files. Live holds no secret, and every outgoing line also
passes `redact.String`. On start a socket nobody answers on is a leftover and is replaced; one that answers means
another agent runs with this directory and `run` exits. Closing the listener removes the file. Cost, measured on
2026-10-14 (stripped): +66 KB on darwin/arm64, +68 KB on linux/amd64 and windows/amd64, +128 KB on linux/arm64
(section alignment); idle RSS on linux/arm64 about +0.2 MB (6.8-7.4 MB before, 7.2-7.5 MB after over 90 s).

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
