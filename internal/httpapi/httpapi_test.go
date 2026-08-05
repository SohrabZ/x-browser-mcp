package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/auth"
	"github.com/SohrabZ/x-browser-mcp/internal/limit"
	"github.com/SohrabZ/x-browser-mcp/internal/write"
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

// fakeWriter records what a route asked for, which is the whole of what these
// handlers are responsible for.
type fakeWriter struct {
	enabled bool
	calls   []string
	err     error
}

func (f *fakeWriter) Enabled() bool { return f.enabled }

func (f *fakeWriter) record(call string) error {
	f.calls = append(f.calls, call)
	return f.err
}

func (f *fakeWriter) Post(_ context.Context, text, confirm string) error {
	return f.record(fmt.Sprintf("post text=%q confirm=%q", text, confirm))
}

func (f *fakeWriter) Reply(_ context.Context, handle, postID, text, confirm string) error {
	return f.record(fmt.Sprintf("reply %s/%s text=%q confirm=%q", handle, postID, text, confirm))
}

func (f *fakeWriter) Like(_ context.Context, handle, postID, confirm string) error {
	return f.record(fmt.Sprintf("like %s/%s confirm=%q", handle, postID, confirm))
}

func (f *fakeWriter) Repost(_ context.Context, handle, postID, confirm string) error {
	return f.record(fmt.Sprintf("repost %s/%s confirm=%q", handle, postID, confirm))
}

func (f *fakeWriter) Bookmark(_ context.Context, handle, postID, confirm string) error {
	return f.record(fmt.Sprintf("bookmark %s/%s confirm=%q", handle, postID, confirm))
}

func (f *fakeWriter) Unbookmark(_ context.Context, handle, postID, confirm string) error {
	return f.record(fmt.Sprintf("unbookmark %s/%s confirm=%q", handle, postID, confirm))
}

// The same guarantee the MCP tools have: with writes off the routes do not
// exist, so nothing that reaches the port can call them. A 403 would still be
// telling a caller the capability is there.
func TestWriteRoutesAreNotRegisteredWhenWritesAreDisabled(t *testing.T) {
	h := Handler(Deps{Writer: &fakeWriter{enabled: false}})

	for _, path := range []string{"post", "reply", "like", "repost", "bookmark", "unbookmark"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/"+path, strings.NewReader("{}")))
		if rec.Code != http.StatusNotFound {
			t.Errorf("/api/v1/%s: got %d, want 404 when writes are disabled", path, rec.Code)
		}
	}
}

func TestNoWriterMeansNoWriteRoutes(t *testing.T) {
	h := Handler(Deps{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/like", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404 without a writer", rec.Code)
	}
}

// Each route must reach its own action. Registration alone would still pass if
// unbookmark were wired to Bookmark, which is the mistake worth catching.
func TestEachWriteRouteCallsItsOwnAction(t *testing.T) {
	cases := []struct {
		path string
		body string
		want string
	}{
		{"post", `{"text":"hello","confirm":"tok"}`, `post text="hello" confirm="tok"`},
		{"reply", `{"handle":"someone","post_id":"222","text":"hi","confirm":"tok"}`, `reply someone/222 text="hi" confirm="tok"`},
		{"like", `{"handle":"someone","post_id":"222","confirm":"tok"}`, `like someone/222 confirm="tok"`},
		{"repost", `{"handle":"someone","post_id":"222","confirm":"tok"}`, `repost someone/222 confirm="tok"`},
		{"bookmark", `{"handle":"someone","post_id":"222","confirm":"tok"}`, `bookmark someone/222 confirm="tok"`},
		{"unbookmark", `{"handle":"someone","post_id":"222","confirm":"tok"}`, `unbookmark someone/222 confirm="tok"`},
	}

	for _, c := range cases {
		writer := &fakeWriter{enabled: true}
		h := Handler(Deps{Writer: writer})

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/"+c.path, strings.NewReader(c.body)))

		if rec.Code != http.StatusOK {
			t.Errorf("/api/v1/%s: got %d, want 200: %s", c.path, rec.Code, rec.Body.String())
			continue
		}
		if len(writer.calls) != 1 || writer.calls[0] != c.want {
			t.Errorf("/api/v1/%s called %v, want [%s]", c.path, writer.calls, c.want)
			continue
		}
		var got struct {
			OK     bool   `json:"ok"`
			Action string `json:"action"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Errorf("/api/v1/%s: decode: %v", c.path, err)
			continue
		}
		if !got.OK || got.Action != c.path {
			t.Errorf("/api/v1/%s reported %+v, want ok with its own action", c.path, got)
		}
	}
}

// A write reports why it failed, unlike a read. "like did not stick" is the
// answer, and replacing it with "internal error" would leave the caller unable
// to tell a refusal from an action X dropped.
func TestAFailedWriteReportsWhyAndBlamesTheRightSide(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"refused", write.ErrBadConfirmation, http.StatusForbidden},
		{"writes off", write.ErrDisabled, http.StatusForbidden},
		{"not signed in", auth.ErrLoginRequired, http.StatusPreconditionFailed},
		{"bad input", &write.InvalidError{Reason: "post text is required"}, http.StatusBadRequest},
		{"X dropped it", errors.New("like did not stick"), http.StatusBadGateway},
	}

	for _, c := range cases {
		h := Handler(Deps{Writer: &fakeWriter{enabled: true, err: c.err}})

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/like",
			strings.NewReader(`{"handle":"someone","post_id":"222","confirm":"tok"}`)))

		if rec.Code != c.status {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.status)
		}
		var body struct{ Error string }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Error != c.err.Error() {
			t.Errorf("%s: reported %q, want the reason %q", c.name, body.Error, c.err.Error())
		}
	}
}

func TestWriteRoutesRejectInvalidJSON(t *testing.T) {
	writer := &fakeWriter{enabled: true}
	h := Handler(Deps{Writer: writer})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/like", strings.NewReader("{not json")))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rec.Code)
	}
	if len(writer.calls) != 0 {
		t.Errorf("a malformed body reached the account: %v", writer.calls)
	}
}
