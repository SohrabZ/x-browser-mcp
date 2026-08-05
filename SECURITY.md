# Security Policy

## Reporting a vulnerability

Please report security issues privately through
[GitHub security advisories](https://github.com/SohrabZ/x-browser-mcp/security/advisories/new)
rather than a public issue.

## What this software exposes

`x-browser-mcp` serves a **logged-in X session** over an **unauthenticated**
HTTP and MCP API. Anything that can reach the port can read your timeline, and
if writes are enabled, act as you. Reachability is the only access control there
is, which is why the server binds to `127.0.0.1` by default.

Known properties worth understanding before you run it:

### The API has no authentication

There are no tokens or credentials on the HTTP or MCP endpoints. Binding to a
non-loopback address publishes your X session to that network. The server logs a
warning when configured that way, but does not prevent it.

### Web pages are kept off the loopback server

Loopback binding on its own does not keep a browser out. A domain an attacker
controls, re-resolved to `127.0.0.1` after the page has loaded, gives that page a
route to the port — DNS rebinding — and the read tools would answer it.

Every request is now checked before it reaches a handler, including `/mcp`:

- The `Host` header has to name this server. The browser sends the name it
  dialled, so a rebound request arrives saying `attacker.example` and is refused.
  This applies to a loopback bind, which is the case being attacked; binding
  elsewhere is an explicit choice whose clients dial by names this server cannot
  predict.
- An `Origin` is only accepted from this machine. A request without one did not
  come from a browser; a cross-site one did, and is refused whatever `Host` it
  used.

Both refusals are `403`. This closes the drive-by case, not a hostile program
already running as you — that program can send whatever headers it likes, and the
API still has no authentication.

### Post text is untrusted input aimed at your agent

Everything the read tools return is written by strangers and lands in the same
context as your agent's instructions. A post can contain text designed to be
read as a command. Tool responses label the content as untrusted, and that is a
mitigation, not a guarantee.

This is why write actions are disabled by default, are not registered as tools
at all when disabled, and require a confirmation token minted at startup and
shown only in the operator's terminal — text scraped from a web page cannot
supply a value it has never seen.

### Session state is on disk

The browser profile in `~/.x-browser-mcp/` holds live X session cookies,
encrypted by Chrome against your macOS login Keychain. The directory is created
with `0700` and the write audit log with `0600`. Anyone who can read that
directory as your user can act as you on X.

## Scope

In scope: authentication bypass, unintended write execution, confirmation-token
bypass, session-state disclosure, and anything that lets a remote or web-based
caller reach the API.

Out of scope: the unauthenticated-by-design API, and X changing its markup.
