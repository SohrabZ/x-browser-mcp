// Package httpapi serves the REST endpoints and mounts the MCP handler.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"x-browser-mcp/internal/auth"
	"x-browser-mcp/internal/limit"
	"x-browser-mcp/internal/read"
	"x-browser-mcp/internal/xui"
)

// Deps are what the handlers operate on.
type Deps struct {
	Auth   *auth.Manager
	Reader *read.Reader
	MCP    *mcp.Server
	Log    *slog.Logger
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

	if deps.MCP != nil {
		handler := mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return deps.MCP },
			&mcp.StreamableHTTPOptions{JSONResponse: true, Stateless: true},
		)
		mux.Handle("/mcp", handler)
		mux.Handle("/mcp/", handler)
	}

	return logging(deps.Log, mux)
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

type errorBody struct {
	Error string `json:"error"`
}

func respond(w http.ResponseWriter, log *slog.Logger, res read.Result, err error) {
	if err != nil {
		writeErr(w, log, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeErr maps errors to status codes and keeps internals out of the response.
func writeErr(w http.ResponseWriter, log *slog.Logger, err error) {
	status := http.StatusInternalServerError
	message := "internal error"

	var exhausted *limit.ExhaustedError
	switch {
	case errors.Is(err, auth.ErrLoginRequired):
		status, message = http.StatusPreconditionFailed, err.Error()
	case errors.As(err, &exhausted):
		status, message = http.StatusTooManyRequests, err.Error()
	default:
		// Internal errors can carry filesystem paths and profile locations, so
		// the detail goes to the log and the caller gets a generic message.
		if log != nil {
			log.Error("request failed", "err", err)
		}
	}
	writeJSON(w, status, errorBody{Error: message})
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
