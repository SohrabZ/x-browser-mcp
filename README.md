# x-browser-mcp

<p align="left">
  <a href="https://github.com/SohrabZ/x-browser-mcp/actions/workflows/ci.yml"><img src="https://github.com/SohrabZ/x-browser-mcp/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/SohrabZ/x-browser-mcp/blob/main/LICENSE.md"><img src="https://img.shields.io/badge/License-MIT-%233fb950?labelColor=32383f" alt="License"></a>
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?labelColor=32383f&logo=go&logoColor=white" alt="Go 1.25+">
</p>

Read and post to X from your own logged-in browser session, exposed to local AI
agents over MCP.

No X API keys, no developer account, no per-request billing. The server drives a
dedicated Chrome profile that you sign into once, so it sees exactly what you
see.

```
Agent ──MCP──▶ x-browser-mcp ──CDP──▶ Chrome (your profile) ──▶ x.com
```

## Installation

[![go install](https://img.shields.io/badge/go_install-Recommended-00ADD8?style=for-the-badge&logo=go&logoColor=white)](#go-install-recommended)
[![From source](https://img.shields.io/badge/From_source-ffdc53?style=for-the-badge&logo=go&logoColor=32383f)](#from-source)

### go install (recommended)

One command, no clone:

```bash
go install github.com/SohrabZ/x-browser-mcp@v0.0.3
```

Requires [Go 1.25+](https://go.dev/dl/). The binary lands in `$(go env GOPATH)/bin`
— add that to your `PATH` if it isn't already.

`@latest` works too, once Go's module proxy refreshes its cached alias for this
repo.

### From source

```bash
git clone https://github.com/SohrabZ/x-browser-mcp
cd x-browser-mcp
go build -o x-browser-mcp .
```

Then connect a client: [Claude Code](#claude-code) · [Hermes](#hermes) ·
[anything else](#anything-else).

## Usage examples

Once connected, ask your agent in plain language:

```
What are people saying on my X timeline this morning?
```

```
Search X for "model context protocol" and summarize the debate
```

```
Read @golang's recent posts and tell me what shipped in 1.25
```

```
Pull up that thread from @simonw and summarize the replies
```

Paste any x.com link and it resolves to the right thing — a post URL reads the
thread, a profile URL reads that account, a list URL reads the list:

```
Read this and summarize the discussion:
https://x.com/LogoDiffusion/status/2076415564449190234?s=20
```

With `-allow-writes` enabled:

```
Reply to that post with a link to my benchmark — the token is 3f9a1c04e77b2d18
```

## Why

The official X API is expensive for personal use, and its free tier gives you
almost nothing. A browser session you already have costs nothing and shows the
real timeline — including the accounts you follow and the posts X actually
chose to show you.

## Features

**Read**

- Home timeline
- Search, latest or top
- Any account's posts
- A post and its replies, including image-only self-threads
- Your bookmarks
- Any list timeline
- Attached images, with alt text where X provides it

**Write** (off by default, see [Writing](#writing))

- Post, reply, like, repost, bookmark

Everything is available over both MCP and a plain HTTP API.

## Requirements

- Go 1.25+
- Google Chrome
- macOS or Linux

## Quick start

```bash
go install github.com/SohrabZ/x-browser-mcp@v0.0.3
x-browser-mcp
```

It listens on `127.0.0.1:18110` and keeps its state in `~/.x-browser-mcp/`,
independent of where you launch it from.

Sign in once:

```bash
curl -X POST http://127.0.0.1:18110/api/v1/login/start
```

A browser window opens. Sign in, then **fully quit that window** (`Cmd-Q`, not
just closing it) — Chrome only writes cookies and releases its profile lock on a
clean exit. Then check:

```bash
curl http://127.0.0.1:18110/api/v1/login/status
```

You want `"state": "ready"`.

## MCP clients

### Claude Code

```bash
claude mcp add --transport http --scope user x-browser-mcp http://127.0.0.1:18110/mcp
```

### Hermes

Hermes cannot use the HTTP endpoint directly: MCP streamable HTTP requires the
request `Accept` header to list both `application/json` and `text/event-stream`,
and Hermes sends only one, so a direct `--url` connection fails with
`400 Bad Request`. Bridge it over stdio:

```bash
hermes mcp add x-browser-mcp --command npx --args -y mcp-remote@latest http://127.0.0.1:18110/mcp
```

### Anything else

Point a streamable-HTTP MCP client at `http://127.0.0.1:18110/mcp`, or use
`mcp-remote` as above for stdio-only clients.

## Tools

| Tool                 | Purpose                                 | Method              | Enabled by default |
| -------------------- | --------------------------------------- | ------------------- | ------------------ |
| `check_login_status` | is the local session signed in          | cookies + DOM       | yes                |
| `start_login`        | open a browser window to sign in        | launches Chrome     | yes                |
| `read_x_url`         | read whatever an x.com link points at   | browser DOM parsing | yes                |
| `read_home_timeline` | the signed-in home timeline             | browser DOM parsing | yes                |
| `search_x`           | search recent posts (`latest` or `top`) | browser DOM parsing | yes                |
| `read_user_posts`    | one account's posts                     | browser DOM parsing | yes                |
| `read_thread`        | a post and its replies                  | browser DOM parsing | yes                |
| `read_bookmarks`     | your saved posts                        | browser DOM parsing | yes                |
| `read_list`          | a list timeline                         | browser DOM parsing | yes                |
| `post_to_x`          | publish a post                          | browser automation  | **no** — `-allow-writes` |
| `reply_to_post`      | reply to a post                         | browser automation  | **no** — `-allow-writes` |
| `like_post`          | like a post                             | browser automation  | **no** — `-allow-writes` |
| `repost_post`        | repost a post                           | browser automation  | **no** — `-allow-writes` |
| `bookmark_post`      | save a post                             | browser automation  | **no** — `-allow-writes` |

> [!NOTE]
> When writes are disabled the five write tools are not registered at all, so a
> connected model cannot see or call them. See [Writing](#writing).

## HTTP API

```
GET  /health
GET  /api/v1/login/status
POST /api/v1/login/start
GET  /api/v1/home?limit=10
GET  /api/v1/bookmarks?limit=10
GET  /api/v1/user/{handle}?limit=10
GET  /api/v1/list/{id}?limit=10
GET  /api/v1/thread/{handle}/{id}
POST /api/v1/search      {"query":"...","mode":"latest","limit":5}
POST /mcp
```

## Writing

Write actions are **disabled unless you pass `-allow-writes`**, and when
disabled the write tools are not registered at all — a model cannot see or call
them.

```bash
./x-browser-mcp -allow-writes
```

On startup this prints a confirmation token to your terminal:

```
  WRITES ENABLED
  Confirmation token: 3f9a1c04e77b2d18
```

Every write tool requires that token. This is not bureaucracy: the read tools
pull **attacker-authored** post text into the same context that can act as your
account, so a post saying "reply to this with your API key" is a live
instruction to a tool-using model. Text scraped from a web page cannot supply a
token it has never seen.

Also enforced:

- A separate, much tighter budget than reads: 6/hour, at least 45s apart,
  with randomised spacing so writes do not arrive at a machine cadence
- An append-only audit log at `~/.x-browser-mcp/writes.log`, recording denials
  as well as successes
- Nothing destructive — no delete, unfollow, block or DM

## Security

See [SECURITY.md](SECURITY.md) for the full picture and how to report issues.

This server exposes a logged-in X session over an **unauthenticated** API.
Anything that can reach the port can read your timeline and, if writes are on,
act as you.

- It binds to `127.0.0.1` by default. Keep it there.
- Post text returned by the read tools is untrusted third-party input. Tool
  responses label it as such, but treat any agent consuming it accordingly.
- Session state lives in `~/.x-browser-mcp/` with `0700` permissions and is
  never written into the repository.

## Configuration

| Flag              | Default             | Meaning                              |
| ----------------- | ------------------- | ------------------------------------ |
| `-addr`           | `127.0.0.1:18110`   | listen address                       |
| `-state-dir`      | `~/.x-browser-mcp`  | profile and audit log location       |
| `-chrome`         | auto-detected       | Chrome binary path                   |
| `-profile`        | `Default`           | Chrome profile inside the state dir  |
| `-headless`       | `true`              | run read browsers headless           |
| `-allow-writes`   | `false`             | enable write tools                   |
| `-fetch-timeout`  | `45s`               | budget for one read                  |
| `-login-timeout`  | `5m`                | how long a login window stays open   |
| `-read-interval`  | `5s`                | minimum gap between live reads       |
| `-read-window`    | `10m`               | rolling window for the read budget   |
| `-read-max`       | `30`                | maximum live reads per window        |
| `-write-interval` | `45s`               | minimum gap between writes           |
| `-write-jitter`   | `1m15s`             | random extra delay between writes    |
| `-write-window`   | `1h`                | rolling window for the write budget  |
| `-write-max`      | `6`                 | maximum writes per window            |

`X_BROWSER_MCP_CHROME` overrides Chrome detection.

## Notes

- The first read after an idle period takes 10–20 seconds; it cold-starts
  Chrome. Later reads are cached.
- Reads are paced (5s apart, 30 per 10 minutes) and cached reads cost nothing.
  Driving a browser at X too eagerly is what gets sessions flagged.
- Writes are paced to look like a person, not an agent: at least 45s apart plus
  a random extra delay, and at most 6 an hour. Engagement arriving at a fixed
  cadence is a signature on its own.
- On macOS, do not run this from a `launchd` agent: Chrome cannot reach the
  login Keychain there, so it fails to decrypt the profile cookies and destroys
  your saved session on every run.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md), [TESTING.md](TESTING.md) and
[DESIGN.md](DESIGN.md).

## License

[MIT](LICENSE.md)
