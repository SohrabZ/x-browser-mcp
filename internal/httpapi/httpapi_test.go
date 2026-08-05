package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/auth"
	"github.com/SohrabZ/x-browser-mcp/internal/limit"
)

func TestHealthIsAlwaysAvailable(t *testing.T) {
	h := Handler(Deps{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var body map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body["ok"] {
		t.Fatal("expected ok:true")
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	h := Handler(Deps{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

// Method routing is part of the pattern, so a wrong verb must not reach a
// handler that assumes a body.
func TestWrongMethodIsRejected(t *testing.T) {
	h := Handler(Deps{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search", nil))

	if rec.Code == http.StatusOK {
		t.Fatal("GET on a POST-only route should not succeed")
	}
}

func TestSearchRejectsInvalidJSON(t *testing.T) {
	h := Handler(Deps{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/search", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

// An oversized body must be refused rather than buffered into memory.
func TestSearchRejectsOversizedBody(t *testing.T) {
	h := Handler(Deps{})

	huge := strings.NewReader(`{"query":"` + strings.Repeat("a", maxBody+1024) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search", huge)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for an oversized body", rec.Code)
	}
}

func TestLoginRequiredMapsTo412(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, nil, auth.ErrLoginRequired)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("got %d, want 412", rec.Code)
	}
}

func TestBudgetExhaustionMapsTo429(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, nil, &limit.ExhaustedError{RetryAfter: time.Minute})

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "retry") {
		t.Errorf("the response should say when to retry: %s", rec.Body.String())
	}
}

// Internal errors can carry filesystem paths and profile locations; those go to
// the log, never to the caller.
func TestInternalErrorsAreNotLeakedToCallers(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, nil, errWithSecret{})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "/Users/") {
		t.Fatalf("internal detail leaked to the client: %s", rec.Body.String())
	}
}

type errWithSecret struct{}

func (errWithSecret) Error() string {
	return "open /Users/someone/.x-browser-mcp/profile/Cookies: permission denied"
}

// Every timeout must be set: leaving them unset lets slow clients accumulate
// connections until the server stops accepting new ones.
func TestServerSetsAllTimeouts(t *testing.T) {
	s := Server("127.0.0.1:0", Handler(Deps{}))

	if s.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout must be set")
	}
	if s.ReadTimeout == 0 {
		t.Error("ReadTimeout must be set")
	}
	if s.WriteTimeout == 0 {
		t.Error("WriteTimeout must be set")
	}
	if s.IdleTimeout == 0 {
		t.Error("IdleTimeout must be set")
	}
	// A cold browser start routinely takes tens of seconds; a short write
	// timeout would cut off legitimate reads mid-flight.
	if s.WriteTimeout < time.Minute {
		t.Errorf("WriteTimeout %s is too short for a cold browser start", s.WriteTimeout)
	}
}

func TestIntParam(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x?limit=7", nil)
	if got := intParam(req, "limit"); got != 7 {
		t.Errorf("got %d, want 7", got)
	}

	bad := httptest.NewRequest(http.MethodGet, "/x?limit=abc", nil)
	if got := intParam(bad, "limit"); got != 0 {
		t.Errorf("unparseable values should fall back to 0, got %d", got)
	}

	missing := httptest.NewRequest(http.MethodGet, "/x", nil)
	if got := intParam(missing, "limit"); got != 0 {
		t.Errorf("missing values should fall back to 0, got %d", got)
	}
}
