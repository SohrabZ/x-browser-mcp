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

Each should return real posts. Reads are paced — 3s apart, 30 per 10 minutes —
so a long pass may still return `429`. That is the limiter working, not a bug.
The budget is in memory, so restarting the server clears it.

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

### 4. Notifications and mentions

These two look like one feature and are not. `/notifications` is mostly cells with
no post in them, so it has its own extraction; `/notifications/mentions` is posts.

```bash
curl -s 'http://127.0.0.1:18110/api/v1/mentions?limit=5'
curl -s 'http://127.0.0.1:18110/api/v1/notifications?limit=10' \
  | python3 -c 'import sys,json; [print(n["kind"] or "?", "|", n["text"][:70]) for n in json.load(sys.stdin)["notifications"]]'
```

Expect mentions to come back as ordinary posts. For notifications, expect roughly
as many rows as the page shows cells — if the count collapses to one or two, the
post extractor is being used and everything that is not a post has been dropped.
`kind` is read from X's own words and will be empty under a non-English interface;
`text` still says what happened.

### 5. X Articles

Long-form posts render nothing under `tweetText`; their headline and body live
under their own testids, so this breaks separately from ordinary posts:

```bash
curl -s 'http://127.0.0.1:18110/api/v1/thread/Alfred_Lin/2084636778791858256?limit=3' \
  | python3 -c 'import sys,json; r=json.load(sys.stdin)["root"]; print(r["title"], len(r["text"]))'
```

Expect a non-empty `title` and a body of a few thousand characters. An empty
`text` means the article selectors have drifted.

### 6. End-to-end check with an agent

Connectivity first:

```bash
claude mcp list                  # expect: x-browser-mcp ... ✔ Connected
hermes mcp test x-browser-mcp    # expect: ✓ Tools discovered: 11 (17 with -allow-writes)
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

### 7. Write gating

```bash
./x-browser-mcp -allow-writes
```

Check all four properties:

1. Without `-allow-writes`, `tools/list` returns **no** write tools.
2. With it, the six appear and the terminal prints a confirmation token.
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

### 8. Writes that actually take effect

The gate checks above pass whether or not a write does anything. Check the
action separately, and check it the only way that works:

**Never accept the tool's own success report.** A write reports what it did to
the page, and X's page is not X. Confirm from a fresh page load, in a different
browser, after the write browser has gone:

```bash
./x-browser-mcp -allow-writes            # note the confirmation token

hermes -z "Use the x-browser-mcp tool like_post on handle <you>, \
 postID <a post of yours>, confirm <token>. Report the result verbatim."
```

Then open the post yourself and look. If the tool said `Liked.` and the post
shows a like button rather than an unlike button, the tool is lying to you.

Run each of `post_to_x`, `reply_to_post`, `like_post`, `repost_post`,
`bookmark_post` and `unbookmark_post` this way at least once before a release. They fail differently:
posting works while replying does not, because their composers behave
differently; liking fails in a way that leaves the page looking correct.

**Why the indirection.** X applies engagement over the network and updates its
controls optimistically, before the request completes. A check that reads the
page after the click sees the optimistic state and passes. Worse, anything that
disturbs the page in that window -- closing the browser, navigating, or
reloading in order to check -- cancels the request, so an over-eager check
causes the failure it is looking for. Three consecutive versions of this check
passed while the like was being silently discarded.

**Do not trust an unchanged UI either.** Undo the action by hand between runs
(unlike the post, delete the reply). A tool that does nothing looks identical to
one that correctly detected the action was already applied.

## Before tagging a release

- All four automated gates pass.
- The manual pass above runs clean against a live session.
- Every write tool has been run end to end and confirmed from a separate
  browser, not from its own success report.
- The version in `internal/mcpapi` matches the tag.

Never move a published tag. Go's module proxy treats a version as immutable, so
re-pointing a tag leaves `go install` serving the original commit forever. Cut a
new version instead.
