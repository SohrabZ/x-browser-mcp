// Package httpapi serves the REST endpoints and mounts the MCP handler.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SohrabZ/x-browser-mcp/internal/auth"
	"github.com/SohrabZ/x-browser-mcp/internal/fault"
	"github.com/SohrabZ/x-browser-mcp/internal/read"
	"github.com/SohrabZ/x-browser-mcp/internal/write"
	"github.com/SohrabZ/x-browser-mcp/internal/xui"
)

// Deps are what the handlers operate on.
type Deps struct {
	Auth   *auth.Manager
	Reader *read.Reader
	Writer write.Actions
	MCP    *mcp.Server
	Log    *slog.Logger

	// ListenAddr is the address the server was told to bind, and AllowedHosts
	// any further names it answers to. The handler needs them to know which Host
	// names itself; see guard.
	ListenAddr   string
	AllowedHosts []string
}

// maxBody caps request bodies. The endpoints take a handful of small fields, so
// anything larger is a mistake or an attempt to exhaust memory.
const maxBody = 64 << 10

// Handler builds the full HTTP surface.
func Handler(deps Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/v1/login/status", func(w http.ResponseWriter, r *http.Request) {
		status, err := deps.Auth.Status(r.Context())
		if err != nil {
			writeErr(w, deps.Log, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})

	mux.HandleFunc("POST /api/v1/login/start", func(w http.ResponseWriter, r *http.Request) {
		deadline, err := deps.Auth.StartLogin(r.Context())
		if err != nil {
			writeErr(w, deps.Log, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"deadline": deadline.UTC(),
			"message":  "sign in, then fully quit the browser window",
		})
	})

	mux.HandleFunc("GET /api/v1/home", func(w http.ResponseWriter, r *http.Request) {
		res, err := deps.Reader.Home(r.Context(), intParam(r, "limit"))
		respond(w, deps.Log, res, err)
	})

	mux.HandleFunc("GET /api/v1/bookmarks", func(w http.ResponseWriter, r *http.Request) {
		res, err := deps.Reader.Bookmarks(r.Context(), intParam(r, "limit"))
		respond(w, deps.Log, res, err)
	})

	mux.HandleFunc("GET /api/v1/mentions", func(w http.ResponseWriter, r *http.Request) {
		res, err := deps.Reader.Mentions(r.Context(), intParam(r, "limit"))
		respond(w, deps.Log, res, err)
	})

	// Notifications answer with their own shape rather than a Result, because
	// most of them are not posts.
	mux.HandleFunc("GET /api/v1/notifications", func(w http.ResponseWriter, r *http.Request) {
		res, err := deps.Reader.Notifications(r.Context(), intParam(r, "limit"))
		if err != nil {
			writeErr(w, deps.Log, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("GET /api/v1/user/{handle}", func(w http.ResponseWriter, r *http.Request) {
		res, err := deps.Reader.UserPosts(r.Context(), r.PathValue("handle"), intParam(r, "limit"))
		respond(w, deps.Log, res, err)
	})

	mux.HandleFunc("GET /api/v1/list/{id}", func(w http.ResponseWriter, r *http.Request) {
		res, err := deps.Reader.List(r.Context(), r.PathValue("id"), intParam(r, "limit"))
		respond(w, deps.Log, res, err)
	})

	mux.HandleFunc("GET /api/v1/thread/{handle}/{id}", func(w http.ResponseWriter, r *http.Request) {
		thread, err := deps.Reader.Thread(r.Context(), r.PathValue("handle"), r.PathValue("id"), intParam(r, "limit"))
		if err != nil {
			writeErr(w, deps.Log, err)
			return
		}
		writeJSON(w, http.StatusOK, thread)
	})

	mux.HandleFunc("POST /api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
			Mode  string `json:"mode"`
			Limit int    `json:"limit"`
		}
		if err := decode(w, r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON body"})
			return
		}
		res, err := deps.Reader.Search(r.Context(), read.Query{
			Text:  body.Query,
			Mode:  xui.SearchMode(body.Mode),
			Limit: body.Limit,
		})
		respond(w, deps.Log, res, err)
	})

	// The write routes are registered only when writes are enabled, for the same
	// reason the MCP write tools are: a route that is not there cannot be reached
	// by anything that gets hold of the port, whereas one that refuses at call
	// time still advertises the capability.
	if deps.Writer != nil && deps.Writer.Enabled() {
		registerWrites(mux, deps)
	}

	if deps.MCP != nil {
		handler := mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return deps.MCP },
			&mcp.StreamableHTTPOptions{JSONResponse: true, Stateless: true},
		)
		mux.Handle("/mcp", handler)
		mux.Handle("/mcp/", handler)
	}

	return logging(deps.Log, guard(deps.ListenAddr, deps.AllowedHosts, mux))
}

// guard rejects requests a web page could have made.
//
// The server binds loopback and has no authentication, so the only thing between
// it and a page you happen to visit is supposed to be that the page cannot reach
// the port. It can. A domain the attacker controls, re-resolved to 127.0.0.1
// after the page has loaded, gives that page a route to the port -- DNS
// rebinding. The browser still sends the name it dialled, so requiring Host to
// name this server is what closes it, and refusing a cross-site Origin closes the
// simpler case of a page fetching the port directly.
//
// The Host check applies only to a loopback bind, which is the deployment being
// attacked. An operator who bound elsewhere has clients that legitimately dial by
// another name, and has already been warned that doing so exposes the session.
func guard(listenAddr string, extra []string, next http.Handler) http.Handler {
	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	if host := hostOf(listenAddr); !wildcard(host) {
		allowed[host] = true
	}
	for _, h := range extra {
		if host := hostOf(h); host != "" {
			allowed[host] = true
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Any Origin at all means a browser is calling, and nothing here is
		// reachable from a page: no response carries CORS headers, so even a page
		// served from this machine could not read one. Allowing some origins
		// would widen what a hostile page can set in motion -- a read, a login
		// window -- without enabling anything that works.
		if r.Header.Get("Origin") != "" {
			writeJSON(w, http.StatusForbidden, errorBody{Error: "cross-origin request refused"})
			return
		}
		if !allowed[hostOf(r.Host)] {
			writeJSON(w, http.StatusForbidden, errorBody{Error: "refused: the request does not address this server by name"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// wildcard reports whether a bind address names no particular host. There is
// nothing to add to the list for one: no client sends "0.0.0.0" as a Host, so
// reaching a wildcard bind by a name of its own needs -allowed-host.
func wildcard(host string) bool {
	return host == "" || host == "0.0.0.0" || host == "::"
}

// hostOf reduces an address or a Host header to the name it means.
//
// Case and a trailing dot are not part of that: a host name is
// case-insensitive, and "localhost." is the same host as "localhost", so a
// client using either is this server's own client. Neither loosens the check --
// "EVIL.EXAMPLE." reduces to "evil.example" and is still not on the list.
func hostOf(addr string) string {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// Server wraps the handler with timeouts.
//
// WriteTimeout is generous because a cold browser start routinely takes tens of
// seconds, but every timeout is set: leaving them unset lets slow clients
// accumulate connections indefinitely.
func Server(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
}

// registerWrites mounts the mutating endpoints.
//
// Every one of them takes the same body, so they share it rather than declaring
// six near-identical shapes: an action uses the fields it needs and ignores the
// rest.
func registerWrites(mux *http.ServeMux, deps Deps) {
	type body struct {
		Handle  string `json:"handle"`
		PostID  string `json:"post_id"`
		Text    string `json:"text"`
		Confirm string `json:"confirm"`
	}

	act := func(action string, run func(context.Context, body) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var b body
			if err := decode(w, r, &b); err != nil {
				writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON body"})
				return
			}
			respondWrite(w, deps.Log, action, run(r.Context(), b))
		}
	}

	mux.HandleFunc("POST /api/v1/post", act(write.ActionPost, func(ctx context.Context, b body) error {
		return deps.Writer.Post(ctx, b.Text, b.Confirm)
	}))
	mux.HandleFunc("POST /api/v1/reply", act(write.ActionReply, func(ctx context.Context, b body) error {
		return deps.Writer.Reply(ctx, b.Handle, b.PostID, b.Text, b.Confirm)
	}))
	mux.HandleFunc("POST /api/v1/like", act(write.ActionLike, func(ctx context.Context, b body) error {
		return deps.Writer.Like(ctx, b.Handle, b.PostID, b.Confirm)
	}))
	mux.HandleFunc("POST /api/v1/repost", act(write.ActionRepost, func(ctx context.Context, b body) error {
		return deps.Writer.Repost(ctx, b.Handle, b.PostID, b.Confirm)
	}))
	mux.HandleFunc("POST /api/v1/bookmark", act(write.ActionBookmark, func(ctx context.Context, b body) error {
		return deps.Writer.Bookmark(ctx, b.Handle, b.PostID, b.Confirm)
	}))
	mux.HandleFunc("POST /api/v1/unbookmark", act(write.ActionUnbookmark, func(ctx context.Context, b body) error {
		return deps.Writer.Unbookmark(ctx, b.Handle, b.PostID, b.Confirm)
	}))
}

type errorBody struct {
	Error string `json:"error"`
}

// respondWrite reports a write's outcome.
//
// Why a write failed is the useful part of the answer -- "like did not stick" is
// what the caller has to act on -- so a failure that was recognised says what it
// was. One that was not is a fault of this process, and is no more the caller's
// to read here than on a read: a write reaches the browser, so it can come back
// carrying the path of a profile lock.
func respondWrite(w http.ResponseWriter, log *slog.Logger, action string, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": action})
		return
	}

	kind, message := fault.Describe(err)
	if kind == fault.Internal && log != nil {
		log.Error("write failed", "action", action, "err", err)
	}
	writeJSON(w, statusFor(kind), errorBody{Error: message})
}

func respond(w http.ResponseWriter, log *slog.Logger, res read.Result, err error) {
	if err != nil {
		writeErr(w, log, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeErr answers a failed read.
//
// What may be said about a failure, and what has to stay in this process, is
// decided in one place for both transports; this turns that into a status. Only a
// fault the caller can do nothing about is hidden, and its detail goes to the log
// instead -- a list id that does not exist is not a server error, and reporting it
// as one leaves them re-sending a request that will never work.
func writeErr(w http.ResponseWriter, log *slog.Logger, err error) {
	kind, message := fault.Describe(err)
	if kind == fault.Internal && log != nil {
		log.Error("request failed", "err", err)
	}
	writeJSON(w, statusFor(kind), errorBody{Error: message})
}

// statusFor is this transport's vocabulary for a kind of failure.
func statusFor(kind fault.Kind) int {
	switch kind {
	case fault.Invalid:
		return http.StatusBadRequest
	case fault.Missing:
		return http.StatusNotFound
	case fault.Refused:
		return http.StatusForbidden
	case fault.LoginRequired:
		return http.StatusPreconditionFailed
	case fault.Paced:
		return http.StatusTooManyRequests
	case fault.Busy:
		return http.StatusServiceUnavailable
	case fault.Timeout:
		return http.StatusGatewayTimeout
	case fault.NotApplied:
		// The request was carried out and X did not apply it, which is a failure
		// upstream of here rather than a fault of this server's.
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	return json.NewDecoder(r.Body).Decode(dst)
}

func intParam(r *http.Request, name string) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return 0
	}
	return n
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	if log == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path, "took", time.Since(start).Round(time.Millisecond))
	})
}
