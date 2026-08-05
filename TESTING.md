# Testing

Most of this project's value sits in code that drives a real browser against a
site that changes without notice, which cannot run in CI. The suite is split so
that everything testable without Chrome is covered automatically, and the rest
has a written manual pass.

## Automated

```bash
go build ./...     # compiles
go vet ./...       # static checks
gofmt -l .         # must print nothing
go test ./...      # all tests green
```

One line:

```bash
go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test ./...
```

CI runs exactly these four gates on every push and pull request.

### What the automated tests cover

| Package             | Covered                                                     |
| ------------------- | ----------------------------------------------------------- |
| `internal/model`    | post validation, dedupe, contributor ranking, rune-safe excerpts |
| `internal/config`   | defaults, validation, path resolution, permissions           |
| `internal/browser`  | launcher flags, profile locking                              |
| `internal/limit`    | pacing, budget exhaustion, concurrency                       |
| `internal/auth`     | status caching, login-state transitions                      |
| `internal/xui`      | URL parsing and building, selectors, DOM-payload conversion  |
| `internal/read`     | limit clamping, result caching                               |
| `internal/write`    | the confirmation gate and the audit log                      |
| `internal/mcpapi`   | tool registration, through a real in-memory MCP client       |
| `internal/httpapi`  | routing, status mapping, body limits, timeouts               |

Two of these guard mistakes that are invisible in review and expensive to
rediscover:

- `TestLauncherDisablesMockKeychain` — rod enables `--use-mock-keychain` by
  default. With it, Chrome cannot decrypt cookies a real Chrome wrote, so a
  logged-in profile reads back empty and is then overwritten. A dependency bump
  could reintroduce it and the only symptom would be "login doesn't stick".
- `TestWriteToolsAreNotRegisteredWhenWritesAreDisabled` — the write-safety
  design rests on the tools being absent rather than refusing, so it is asserted
  as a connected client actually sees it.

## Manual

Anything that needs Chrome or a live X session. Run before tagging a release.

### 1. Login

```bash
go build -o x-browser-mcp . && ./x-browser-mcp
curl -X POST http://127.0.0.1:18110/api/v1/login/start
```

Sign in, then **fully quit the window** (`Cmd-Q`). Chrome only flushes cookies
and releases its profile lock on a clean exit.

```bash
curl -s http://127.0.0.1:18110/api/v1/login/status   # expect "state":"ready"
```

Then check the session **survives automation** — run the status check again and
confirm it still reports `ready`. A profile that reads `ready` once and
`login_required` afterwards means something is destroying the cookie store.

### 2. Read surfaces

```bash
curl -s 'http://127.0.0.1:18110/api/v1/home?limit=5'
curl -s 'http://127.0.0.1:18110/api/v1/user/golang?limit=5'
curl -s 'http://127.0.0.1:18110/api/v1/bookmarks?limit=5'
curl -s -X POST http://127.0.0.1:18110/api/v1/search \
  -H 'Content-Type: application/json' -d '{"query":"golang","limit":5}'
```

Each should return real posts. Reads are paced — 15s apart, 8 per 10 minutes —
so running these back to back may return `429`. That is the limiter working.

### 3. Image-only threads

Visual self-threads are published as replies with **no text at all**, and are the
case most easily broken by a change to extraction or post validation:

```bash
curl -s 'http://127.0.0.1:18110/api/v1/thread/LogoDiffusion/2076415564449190234?limit=15'
```

Expect the root plus 10 replies, each carrying one entry in `media` and an empty
`text`. If replies come back empty, post validation is discarding them.

Note that X's own reply counter shows `1` for that post — self-thread replies do
not count toward it, so the counter is not a check on this.

### 4. End-to-end check with an agent

Connectivity first:

```bash
claude mcp list                  # expect: x-browser-mcp ... ✔ Connected
hermes mcp test x-browser-mcp    # expect: ✓ Tools discovered: 9
```

Connectivity is not the interesting part. **Tool descriptions are prompts**, and
the only way to know whether they steer a model to the right call is to let a
model choose. A tool that works perfectly over `curl` still fails in practice if
the description does not make a model reach for it — that failure is invisible
to every other kind of test here.

Always ask the agent to report the tool calls it made. Without that, a fluent
answer assembled from the model's own knowledge is indistinguishable from a real
one.

#### Hermes

Non-interactive, so it scripts well:

```bash
hermes -z "Use x-browser-mcp to read https://x.com/LogoDiffusion/status/2076415564449190234 \
 — how many replies are in that thread, and what do they contain? \
 Then list the exact tool calls you made."
```

#### Claude Code

Paste the same prompt into a session, or:

```bash
claude -p "Use the x-browser-mcp tools to read my X home timeline (5 posts). \
 List each author handle and a one-line summary, then tell me which tool calls you made."
```

#### What a passing run looks like

| Check | Pass | Fail means |
| --- | --- | --- |
| Tool selection | picks `read_x_url` for a pasted link | the description is not steering; the model falls back to `search_x` and loops |
| Content | 10 replies, image-only, from `@LogoDiffusion` | post validation is discarding media-only posts again |
| Reported calls | names the tools it actually invoked | it may have answered from memory without calling anything |
| `cached` flag | `false` on a first read | you are testing the cache, not the browser |

#### Interpreting failures

- **"MCP unreachable"** — the server is not running, or it was restarted
  mid-call. Rebuilding while an agent is mid-test produces exactly this. Check
  `curl -s http://127.0.0.1:18110/health` before blaming the client.
- **Only `search_x` attempted, repeatedly** — the model could not find a tool
  matching the request. Read that as a description problem, not a model problem.
- **Plausible answer, no tool calls listed** — treat as a failed run. Re-ask
  with an explicit instruction to use the tools.
- **A tool the client cannot see** — after adding a tool, clients discover it on
  a fresh session. Re-run `hermes mcp test` to confirm the count changed.

### 5. Write gating

```bash
./x-browser-mcp -allow-writes
```

Check all four properties:

1. Without `-allow-writes`, `tools/list` returns **no** write tools.
2. With it, the five appear and the terminal prints a confirmation token.
3. A write with a wrong token is refused and recorded in
   `~/.x-browser-mcp/writes.log`.
4. A write with no token at all is rejected by schema validation.

```bash
curl -s -X POST http://127.0.0.1:18110/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_to_x",
       "arguments":{"text":"test","confirm":"0000000000000000"}}}'
```

Only test writes against an account you are willing to post from.

## Before tagging a release

- All four automated gates pass.
- The manual pass above runs clean against a live session.
- The version in `internal/mcpapi` matches the tag.

Never move a published tag. Go's module proxy treats a version as immutable, so
re-pointing a tag leaves `go install` serving the original commit forever. Cut a
new version instead.
