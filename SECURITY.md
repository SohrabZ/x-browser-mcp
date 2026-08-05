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

### Local web pages can reach a loopback server

The MCP handler does not validate `Origin` or `Host`, so a page you visit in an
ordinary browser can reach `127.0.0.1:18110` via DNS rebinding and read what the
read tools return. Loopback binding does not prevent this. If that matters for
your threat model, run the server only while you need it.

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
