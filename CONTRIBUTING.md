# Contributing

Thanks for your interest in improving x-browser-mcp! This is a small Go project
that drives a real Chrome profile to read X — contributions of all sizes are
welcome.

## Development setup

```bash
git clone <your-fork-url>
cd x-browser-mcp
go build -o x-browser-mcp .

# start the server, then complete a one-time login
./x-browser-mcp
curl -X POST http://127.0.0.1:18110/api/v1/login/start
```

Requires **Go 1.25+** and a local Chrome install.

Session state lives in `~/.x-browser-mcp/` and is never committed.

## Before you open a PR

Please make sure all of these pass:

```bash
go build ./...     # compiles
go vet ./...       # static checks
gofmt -l .         # must print nothing (run `gofmt -w .` to fix)
go test ./...      # all tests green
```

A one-liner:

```bash
go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test ./...
```

CI runs exactly these four gates.

## Coding guidelines

- **Formatting**: always `gofmt`. CI will reject unformatted code.
- **Comments** explain *why*, not *what*. Match the surrounding style.
- **Tests live next to code** as `*_test.go` in the same package, so they can
  exercise unexported functions. Add or update tests for any behavior change.

## Project layout

| Package            | Responsibility                                        |
| ------------------ | ----------------------------------------------------- |
| `main`             | flag parsing, wiring, graceful shutdown               |
| `internal/model`   | domain types; imports nothing else in the project     |
| `internal/config`  | settings, defaults, path resolution, validation       |
| `internal/browser` | Chrome lifecycle: launcher flags, sessions, profile lock |
| `internal/auth`    | login state machine, status caching, login supervision |
| `internal/xui`     | X page primitives: URLs, selectors, extraction scripts |
| `internal/read`    | read surfaces built on `xui`                          |
| `internal/write`   | write actions, gating, audit log                      |
| `internal/limit`   | pacing and budget, shared by reads and writes         |
| `internal/mcpapi`  | MCP tool definitions and rendering                    |
| `internal/httpapi` | REST handlers, middleware, JSON envelopes             |

Dependencies run strictly downward: transports depend on `read`/`write`, which
depend on `xui` → `browser` → `config`/`model`. Nothing depends upward.

## Testing against a real browser

Most of the value is in code that talks to Chrome, which is awkward to test.
Two rules keep it tractable:

- **Pure configuration is unit-testable.** `NewAutomatedLauncher` builds the
  launcher without creating directories or starting a process, so its flags can
  be asserted on directly. Prefer that shape for anything new.
- **Guard inherited defaults explicitly.** rod injects its own Chrome flags, and
  one of them (`--use-mock-keychain`) silently breaks profile reuse — Chrome
  cannot decrypt cookies a real Chrome wrote, so it drops them and re-encrypts
  the store, destroying the saved session. A dependency bump can quietly
  reintroduce that class of bug, so it is pinned by a test. Do the same for any
  other default you have to opt out of.

## Things that will bite you

- **The login browser must be fully quit** (⌘Q, not just closing the window).
  Chrome only clears `SingletonLock` and flushes cookies on a clean exit, and a
  stale lock makes every read fail with "profile is already in use".
- **Do not run the server from a `launchd` agent on macOS.** Chrome cannot reach
  the login Keychain from there, fails to decrypt the profile cookies, and
  destroys the saved session on every run.
- **Do not poll `check_login_status` in a loop.** Every uncached check drives a
  real browser at X. Verdicts are cached deliberately; keep it that way.

## Security

This server exposes a logged-in X session over an **unauthenticated** HTTP and
MCP endpoint. It binds to `127.0.0.1` by default and should stay that way.

- Never commit session state — `~/.x-browser-mcp/` and any `*_cookies.json` are
  outside the repo and git-ignored on purpose.
- Post text returned by the read tools is **untrusted third-party input** that
  lands directly in an agent's context. Treat it as data, never as instructions.
- Please report security issues privately via a GitHub security advisory rather
  than a public issue.

## Reporting issues

Open an issue with: what you ran, what you expected, what happened, and the
relevant output — with any handles, post content, or session paths redacted.
