# Roadmap

What exists, what is planned, and what this project deliberately will not do.
Nothing here is a commitment to a date.

## Shipped

**Reading** — home timeline, search (latest/top), an account's posts, a post and
its replies, bookmarks, lists, and any x.com link via `read_x_url`. Posts carry
attached images, and X Articles carry their title and body.

**Writing** — post, reply, like, repost, bookmark. Off unless `-allow-writes`,
and unregistered as tools when off. Each requires a confirmation token minted at
startup, paced like a person, and recorded in an append-only audit log.

**Interfaces** — MCP over streamable HTTP, plus a REST API.

## Next

### Keep a browser warm

The largest available speedup. Every uncached read currently launches Chrome,
loads a page, waits for render, and quits. Measured on a real read:

| Step | Cold | Warm |
| --- | --- | --- |
| Chrome launch | 0.83s | — |
| Page load | 2.18s | 0.38s |
| Wait + extract | 0.52s | 0.94s |
| Close | 1.00s | — |
| **Total** | **4.53s** | **1.32s** |

Reusing a browser skips launch and close outright and loads pages roughly five
times faster through connection and cache reuse — about 3.2s per read.

The complication is the profile lock: only one Chrome may hold a profile, so a
warm browser must be released before `start_login` opens its window, and
reclaimed afterwards. It also needs an idle timeout, since an idle Chrome holds
memory and a browser that lives for days is a larger surface than one that
lives for seconds.

### Make caching configurable

`ResultTTL` and `StatusTTL` are fixed at five minutes and are not flags. A
cached read costs nothing at all, so for read-heavy use a longer TTL is a bigger
win than any transport change.

### Better failure messages

Anything that is not "login required" or "rate limited" collapses to
`internal error`. A list id that does not exist should say so rather than
report a server fault.

### Close the DNS-rebinding gap

The MCP handler does not validate `Origin` or `Host`, so a page in an ordinary
browser can reach the loopback server. Documented in SECURITY.md; worth fixing
rather than only documenting.

### More read surfaces

Notifications and mentions are the obvious gaps. Both are timeline-shaped, so
they fit the existing extraction path.

## Known limitations

- **X changes its markup.** Selectors live in `internal/xui` so a break has one
  place to be fixed, but breaks will happen.
- **Alt text is usually generic.** X supplies "Image" for most media, so an
  image-only post is thin for a model without vision.
- **Metrics are approximate.** X abbreviates counts in the DOM ("1.2K"), so the
  numbers are rounded.
- **Self-thread replies do not appear in X's reply counter**, so that counter
  cannot be used to check whether a thread was read completely.
- **macOS launchd is unusable.** Chrome cannot reach the login Keychain from a
  launchd agent and destroys the saved session on every run.

## Non-goals

- **Destructive or irreversible actions** — no delete, unfollow, block, mute or
  DMs. The write actions that exist are the ones a mistake can undo by hand.
- **The official X API.** Being browser-backed is the point.
- **A CLI.** Transport is not the cost: a loopback HTTP round trip is ~0.00s
  against ~4.5s for a read, and a one-shot CLI would be slower than the server
  because it starts a browser per invocation and shares no cache. The REST API
  already covers scripting:

  ```bash
  curl -s 'localhost:18110/api/v1/home?limit=5' | jq '.posts[].text'
  ```

- **Multi-account.** One profile, one session. Several would turn every path
  into a keyed lookup for a case this project does not have.
