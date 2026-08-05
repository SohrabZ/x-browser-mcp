# Roadmap — capability reference

`x-browser-mcp` exposes eight read tools (home timeline, search, a user's posts,
a thread, bookmarks, lists, plus login status and sign-in) and five write
actions (post, reply, like, repost, bookmark) that stay unregistered unless the
server is started with `-allow-writes`. This document is the reference point for
deciding what to add next: a complete inventory of the surface an API-backed X
client offers, taken from [`Infatoshi/x-cli`](https://github.com/Infatoshi/x-cli)
at commit `a821755`.

Read it as a menu of candidate capabilities, not a plan of record. Nothing here
is committed to, and a good deal of it stays out of scope — being browser-backed
means the tier restrictions and the whole OAuth credential model are irrelevant
to us, and they appear here as context for what we do not need rather than what
we lack. The parts worth stealing are marked **↪ worth considering**.

Everything below is derived from x-cli's source, not its README or `LLMs.md`, both of which are out of date — see [Documentation drift](#documentation-drift). The clone itself has been deleted; this file is what we keep.

**↪ worth considering, in short:**

- The output-mode design — `-j`/`-p`/`-md` plus a single `-v` verbosity switch, compact-by-default with opt-in metadata. A good fit for token-constrained MCP responses.
- The paginated-search merge strategy: dedupe tweets by `id`, dedupe `includes` by `id`/`media_key`, rewrite `meta` with a true count. See [`tweet search`](#tweet-search-query).
- Pre-flighting auth state locally and failing with the exact remediation command instead of surfacing a remote error — see [`me likes`](#me-likes).
- Sending data to stdout and hints/warnings to stderr, so redirected output stays clean.

**Deliberately out of scope:** anything destructive or irreversible — delete,
unfollow, block, mute, DMs — along with media upload, the five-variable
developer-credential model, and anything gated behind a paid API tier.

The write actions we do implement are deliberately the recoverable ones. A
mistaken like or repost can be undone by hand in seconds; a deleted post cannot.
That line matters more here than in a CLI, because the read tools feed
attacker-authored post text into the same agent context that can act on the
account — see [Writing](README.md#writing) for the gating this implies.

---

## Contents

- [Authentication](#authentication)
- [Global flags](#global-flags)
- [Argument conventions](#argument-conventions)
- [`tweet` — post, read, search](#tweet--post-read-search)
- [`user` — public profile lookups](#user--public-profile-lookups)
- [`me` — authenticated-user data](#me--authenticated-user-data)
- [Top-level engagement actions](#top-level-engagement-actions)
- [`auth` — OAuth 2.0 flow](#auth--oauth-20-flow)
- [Output modes](#output-modes)
- [Auth requirements at a glance](#auth-requirements-at-a-glance)
- [Not offered](#not-offered)
- [Documentation drift](#documentation-drift)

---

## Authentication

Three distinct auth mechanisms coexist, and which one a command uses is not visible from the CLI surface — it's decided per endpoint inside `api.py`.

| Mechanism | Built by | Used for |
|---|---|---|
| **Bearer token** | `X_BEARER_TOKEN` passed straight through | Public reads: tweet lookup, search, profiles, timelines, followers/following |
| **OAuth 1.0a** (HMAC-SHA1) | `generate_oauth_header()`, stdlib `hmac`/`hashlib`, no third-party lib | Writes (post, delete, like, retweet), plus `/users/me` and mentions |
| **OAuth 2.0 User Context** (PKCE) | `auth login` flow, tokens cached on disk | Bookmarks and liked-tweets lookup only |

### Required environment variables

All five are mandatory — `load_credentials()` raises `SystemExit` naming the first one missing, so no command runs without a full set, even a command that wouldn't use them.

```
X_API_KEY                 # consumer key      (OAuth 1.0a)
X_API_SECRET              # consumer secret   (OAuth 1.0a)
X_ACCESS_TOKEN            # user access token (OAuth 1.0a)
X_ACCESS_TOKEN_SECRET     # user token secret (OAuth 1.0a)
X_BEARER_TOKEN            # app-only bearer   (public reads)
```

Optional, needed only for the bookmarks/likes commands:

```
X_OAUTH2_CLIENT_ID
X_OAUTH2_CLIENT_SECRET
X_OAUTH2_REDIRECT_URI     # alternative to --redirect-uri
```

### Credential file resolution

`_load_env_files()` reads, in order (first value wins — later files never override an earlier one, and nothing overrides a variable already in the environment):

1. `~/.config/x-cli/.env`
2. `~/x-cli/.env`
3. a bare `load_dotenv()` call

⚠️ That third step does **not** read the current directory, despite both the README and `LLMs.md` saying it does. `python-dotenv`'s `find_dotenv()` walks upward from *the calling module's source directory* — `.../site-packages/x_cli/` for an installed tool — up to `/`. It also injects *every* key in whatever file it finds, not just `X_*` ones — so `HTTPS_PROXY` or `SSL_CERT_FILE` planted in any writable ancestor directory gets picked up by `httpx` (`trust_env=True` by default) and silently reroutes credentialed traffic. A pattern to avoid if we ever add file-based config: resolve explicit paths and filter to expected keys.

### OAuth 2.0 token cache

`~/.config/x-cli/oauth2_tokens.json`, chmod `0600` after write. Holds `access_token`, `refresh_token` (when `offline.access` was granted), granted `scope`, and a computed `expires_at` (issue time + `expires_in` − 30s safety margin). Refresh is automatic and transparent: `get_valid_access_token()` refreshes on expiry and re-persists, carrying the old refresh token forward if the response omits a new one.

---

## Global flags

Placed **before** the subcommand (`x-cli -j tweet get 123`, not `x-cli tweet get 123 -j`).

| Flag | Effect |
|---|---|
| `-j`, `--json` | Raw JSON to stdout |
| `-p`, `--plain` | TSV, for `awk`/`cut` |
| `-md`, `--markdown` | Markdown |
| *(none)* | `human` — Rich panels and tables (default) |
| `-v`, `--verbose` | Adds timestamps, metrics, metadata, pagination tokens |
| `-h`, `--help` | Available at every level |

The three format flags share one destination, so they're mutually exclusive — the last one wins.

---

## Argument conventions

**`ID_OR_URL`** — every command taking a tweet accepts either form interchangeably, resolved by `parse_tweet_id()`:

```bash
x-cli tweet get 1234567890
x-cli tweet get https://x.com/user/status/1234567890
x-cli tweet get 'https://twitter.com/user/status/1234567890?s=20'   # query string ignored
```

Anything that isn't a matching URL or a pure digit string raises `ValueError`.

**`USERNAME`** — a leading `@` is stripped (`strip_at`), so `@elonmusk` and `elonmusk` behave identically. No further validation is applied; the value is interpolated straight into the request path.

**`--max N`** — every `--max` is silently clamped to the endpoint's own limits rather than rejected. The floors are as much a surprise as the ceilings: `--max 1` on a search still fetches 10 results.

---

## `tweet` — post, read, search

### `tweet post TEXT`

Posts a tweet. `POST /2/tweets`, OAuth 1.0a.

| Option | Type | Default | Meaning |
|---|---|---|---|
| `--poll` | comma-separated string | none | Attaches a poll; options are split on `,` and whitespace-trimmed |
| `--poll-duration` | int (minutes) | `1440` (24h) | Poll lifetime. Not validated locally — the API enforces its own 5–10080 range |

```bash
x-cli tweet post "Hello world"
x-cli tweet post --poll "Yes,No,Maybe" --poll-duration 60 "Do you like polls?"
```

There are no `--reply` or `--quote` options here; those are separate commands below.

### `tweet get ID_OR_URL`

Full single-tweet lookup. `GET /2/tweets/{id}`, **Bearer**.

Requests a wide field set: `created_at`, `public_metrics`, `author_id`, `conversation_id`, `in_reply_to_user_id`, `referenced_tweets`, `attachments`, `entities`, `lang`, `note_tweet`, with `author_id`, `referenced_tweets.id` and `attachments.media_keys` expanded, plus user and media fields. The `note_tweet` field means long-form posts render in full rather than truncated at 280 characters — the formatter prefers `note_tweet.text` over `text` when present.

### `tweet delete ID_OR_URL`

`DELETE /2/tweets/{id}`, OAuth 1.0a. No confirmation prompt — it deletes immediately.

### `tweet reply ID_OR_URL TEXT`

`POST /2/tweets` with `reply.in_reply_to_tweet_id`, OAuth 1.0a.

Prints an unconditional warning to stderr before sending: as of Feb 2026 X restricts programmatic replies on all self-serve tiers (Free, Basic, Pro, Pay-Per-Use), so this succeeds only if the original author @mentioned you or quoted your post. Enterprise is exempt. `tweet quote` is the documented workaround.

### `tweet quote ID_OR_URL TEXT`

`POST /2/tweets` with `quote_tweet_id`, OAuth 1.0a. Not subject to the reply restriction.

### `tweet search QUERY`

The most developed command in the tool. **Bearer**.

| Option | Type | Default | Meaning |
|---|---|---|---|
| `--max N` | int | `10` | Total results wanted (per-request cap without `--all-pages`) |
| `--archive` | flag | off | Use full-archive `/2/tweets/search/all` instead of `/2/tweets/search/recent` |
| `--all-pages` | flag | off | Follow `next_token` until exhausted or `--max` collected |
| `--start-time` | ISO 8601 UTC | none | Oldest timestamp, e.g. `2026-01-01T00:00:00Z` |
| `--end-time` | ISO 8601 UTC | none | Newest timestamp |

Three execution paths:

- **recent** (default) — last ~7 days. `max_results` clamped to **10–100**.
- **`--archive`** — full archive back to X's beginning. `max_results` clamped to **10–500**, and `--start-time` defaults to `2006-03-21T00:00:00Z` if you don't set one. Requires pay-per-use or Enterprise access.
- **`--all-pages`** — loops with a page size of 500 (archive) or 100 (recent), decrementing the remaining budget by the number of tweets actually returned, stopping when `next_token` is absent or a page comes back empty.

Paginated results are merged by `_merge_paginated_responses()`: tweets deduplicated by `id`, `includes` entries deduplicated by `id` or `media_key`, and `meta` rewritten with a true `result_count` plus a `pages` count. The last page's other `meta` keys are preserved.

```bash
x-cli tweet search "machine learning" --max 20
x-cli tweet search "from:elonmusk has:media -is:retweet" --max 50
x-cli tweet search "timelapse from:elliotarledge" --archive --all-pages --max 10000
x-cli tweet search "mcp" --start-time 2026-01-01T00:00:00Z --end-time 2026-02-01T00:00:00Z
```

Query syntax is X's own: `from:`, `to:`, `#tag`, `"exact phrase"`, `has:media`, `is:reply`, `-is:retweet`, `lang:en`, spaces for AND, explicit `OR`.

### `tweet metrics ID_OR_URL`

`GET /2/tweets/{id}` with `tweet.fields=public_metrics,non_public_metrics,organic_metrics`, **OAuth 1.0a** — user context is required because non-public and organic metrics are only visible to the author. Requesting them for someone else's tweet is an API error.

---

## `user` — public profile lookups

### `user get USERNAME`

`GET /2/users/by/username/{username}`, **Bearer**. Returns `created_at`, `description`, `public_metrics`, `verified`, `profile_image_url`, `url`, `location`, `pinned_tweet_id`.

### `user timeline USERNAME`

`GET /2/users/{id}/tweets`, **Bearer**. `--max N`, default `10`, clamped **5–100**.

### `user followers USERNAME`

`GET /2/users/{id}/followers`, **Bearer**. `--max N`, default `100`, clamped **1–1000**.

### `user following USERNAME`

`GET /2/users/{id}/following`, **Bearer**. `--max N`, default `100`, clamped **1–1000**.

All three of the above cost **two API calls**: a `user get` to resolve the username to a numeric ID, then the actual request. That doubles rate-limit consumption and means a typo'd username fails at the lookup step. None of them paginate — you get one page, capped at the clamp.

---

## `me` — authenticated-user data

### `me mentions`

`GET /2/users/{me}/mentions`, **OAuth 1.0a**. `--max N`, default `10`, clamped **5–100**.

### `me bookmarks`

`GET /2/users/{id}/bookmarks`, **OAuth 2.0**. `--max N`, default `10`, clamped **1–100**. Requires Basic tier or above — bookmarks have never been on the Free tier.

### `me bookmark ID_OR_URL`

`POST /2/users/{id}/bookmarks`, **OAuth 2.0**.

### `me unbookmark ID_OR_URL`

`DELETE /2/users/{id}/bookmarks/{tweet_id}`, **OAuth 2.0**.

### `me likes`

`GET /2/users/{id}/liked_tweets`, **OAuth 2.0**. `--max N`, default `10`, clamped **5–100**.

Uniquely, this one pre-flights the stored token: it reads the cached `scope` and fails fast with the exact re-login command if `like.read` is missing, rather than letting the API return a 403.

> **Auth-context caveat across all four OAuth 2.0 commands:** the user ID in the path comes from an **OAuth 1.0a** call to `/users/me`, but the request itself is sent with the **OAuth 2.0** token. If your app credentials and your `auth login` session belong to different accounts, these commands operate against the wrong user ID.

---

## Top-level engagement actions

### `like ID_OR_URL`

`POST /2/users/{me}/likes`, OAuth 1.0a. Prints an unconditional stderr warning: the like endpoint was removed from the Free tier in Aug 2025 and now needs Basic, Pro, or Enterprise.

### `retweet ID_OR_URL`

`POST /2/users/{me}/retweets`, OAuth 1.0a.

Both resolve your own user ID first via `/users/me`, so each is **two API calls** (the ID is cached for the process lifetime, which doesn't help a one-shot CLI).

---

## `auth` — OAuth 2.0 flow

### `auth login`

Interactive Authorization Code + PKCE flow, deliberately built with a manual paste-back step so it works on headless machines with no local callback server.

| Option | Default | Meaning |
|---|---|---|
| `--redirect-uri` | `$X_OAUTH2_REDIRECT_URI` | Must exactly match one registered in the X developer portal |
| `--scopes` | see below | Comma-separated scope list |

Default scopes: `tweet.read`, `users.read`, `bookmark.read`, `bookmark.write`, `like.read`, `offline.access`.

Sequence: generate a 32-byte PKCE verifier and S256 challenge plus a random `state` → print the authorize URL → prompt for the pasted redirect URL or bare code → exchange at `https://api.x.com/2/oauth2/token` using HTTP Basic client auth → save tokens.

```bash
x-cli auth login --redirect-uri http://localhost:8080/callback
x-cli auth login --scopes tweet.read,users.read,bookmark.read,bookmark.write,like.read,offline.access
```

⚠️ The `state` comparison only happens when you paste a full URL. Paste a bare code and the check is silently skipped — PKCE still binds the exchange, but the CSRF guard is effectively optional.

### `auth status`

Reads the token cache and prints its path, validity (`valid` / `expired (will refresh)`), seconds until expiry, granted scopes, and whether a refresh token is present. Purely local — no network call.

There is no `auth logout` or revoke command; the refresh token persists on disk until manually deleted.

---

## Output modes

One router, `format_output(data, mode, title, verbose)`, and every command's response passes through it — the formatters are generic over any dict/list, which is why adding an endpoint requires no formatter work.

### `human` (default)

Rich panels for single tweets and users, Rich tables for user lists. Author IDs are resolved to `@usernames` using the `includes.users` array, falling back to the raw ID when the expansion is absent. **Data goes to stdout; hints and warnings go to stderr** via a separate `Console(stderr=True)` — so `x-cli tweet get 123 > out.txt` keeps the warnings on your terminal.

### `json`

Non-verbose unwraps to just the `data` value, dropping `includes` and `meta`. `-v` emits the entire response. Indent 2, `default=str`.

```bash
x-cli -j tweet search "ai" | jq '.data[].text'   # note: -j alone already unwrapped .data
x-cli -v -j tweet get 123 | jq '.includes.users'
```

### `plain`

TSV. Single objects print as `key<TAB>value` per line; lists print a header row then rows. Column selection is automatic: `username, name, description` for users, `id, author_id, text, created_at` for tweets. Non-verbose also suppresses `public_metrics`, `entities`, `edit_history_tweet_ids`, `attachments`, `referenced_tweets`, `profile_image_url`. Nested values are JSON-encoded inline. `-v` shows every field.

### `markdown`

Tweets as `## title` headings with bold author and an `ID:` footer; users as headings with formatted metrics; user lists as Markdown tables (`|` and newlines stripped from descriptions, truncated to 60 chars). `-v` adds a Followers/Description column, timestamps, location, and join date.

### Verbosity summary

`-v` is the single switch for "give me everything": timestamps, `public_metrics`, `location`/`created_at` on profiles, full JSON including `includes` and `meta`, all TSV columns, and pagination-token hints.

---

## Auth requirements at a glance

| Command | Auth | API calls | Tier notes |
|---|---|---|---|
| `tweet post` | OAuth 1.0a | 1 | Free: 500 posts/mo; Basic: 10k; Pro: 1M |
| `tweet get` | Bearer | 1 | |
| `tweet delete` | OAuth 1.0a | 1 | |
| `tweet reply` | OAuth 1.0a | 1 | Restricted on all self-serve tiers (Feb 2026) |
| `tweet quote` | OAuth 1.0a | 1 | |
| `tweet search` | Bearer | 1–N | `--archive` needs pay-per-use or Enterprise |
| `tweet metrics` | OAuth 1.0a | 1 | Non-public metrics: own tweets only |
| `user get` | Bearer | 1 | |
| `user timeline` | Bearer | 2 | |
| `user followers` | Bearer | 2 | |
| `user following` | Bearer | 2 | |
| `me mentions` | OAuth 1.0a | 2 | |
| `me bookmarks` | OAuth 2.0 (+1a) | 2 | Basic+ |
| `me bookmark` | OAuth 2.0 (+1a) | 2 | Basic+ |
| `me unbookmark` | OAuth 2.0 (+1a) | 2 | Basic+ |
| `me likes` | OAuth 2.0 (+1a) | 2 | Needs `like.read` scope |
| `like` | OAuth 1.0a | 2 | Removed from Free tier (Aug 2025) |
| `retweet` | OAuth 1.0a | 2 | |
| `auth login` | — | 1 (token exchange) | |
| `auth status` | — | 0 | |

---

## Not offered

Gaps worth knowing before treating this as a general-purpose X client:

- **No media upload** — text, polls, and quotes only. No images, video, or GIFs.
- **No DMs**, no lists, no spaces, no communities.
- **No follow / unfollow**, no block, no mute.
- **No unlike, no unretweet** — the actions are one-way (bookmarks are the only reversible pair).
- **No thread posting** — no chained-reply helper, and the reply restriction makes one impractical anyway.
- **No `--next-token` flag**, despite both the `human` and `markdown` formatters printing `Next page: --next-token …` hints in verbose mode. That option does not exist on any command; `--all-pages` on `tweet search` is the only pagination available, and nothing else paginates at all.
- **No pagination on followers/following/timeline/mentions/bookmarks/likes** — one page per invocation, capped by the clamp.
- **No local caching or rate-limit budgeting** — every invocation hits the API. A 429 surfaces the reset timestamp and exits.
- **No configuration file** beyond `.env`; no defaults for output mode or `--max`.
- **No MCP server.** That's a separate project (`x-mcp`) sharing the same credential file.

---

## Documentation drift

The repo's own docs lag the code — useful to know if you read them instead of the source:

- `LLMs.md`'s command reference omits `tweet search`'s `--archive`, `--all-pages`, `--start-time`, `--end-time`; the entire `me likes` command; the entire `auth` group; and the `-v`/`--verbose` flag.
- `LLMs.md` lists only three test files; there are five (`test_api.py` and `test_cli.py` are also present).
- `LLMs.md` says `get_bookmarks` uses OAuth 1.0a; it uses OAuth 2.0.
- Both `README.md` and `LLMs.md` claim a `.env` in the current directory is read. It isn't — see [Credential file resolution](#credential-file-resolution).
- `LLMs.md` dates the reply restriction to Feb 2024 in one place and the README to Feb 2026 in another.
