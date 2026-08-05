package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SohrabZ/x-browser-mcp/internal/auth"
	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/limit"
	"github.com/SohrabZ/x-browser-mcp/internal/pool"
	"github.com/SohrabZ/x-browser-mcp/internal/read"
	"github.com/SohrabZ/x-browser-mcp/internal/write"
)

func TestHealthIsAlwaysAvailable(t *testing.T) {
	h := Handler(Deps{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(http.MethodGet, "/health", nil))

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
	h.ServeHTTP(rec, newRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

// Method routing is part of the pattern, so a wrong verb must not reach a
// handler that assumes a body.
func TestWrongMethodIsRejected(t *testing.T) {
	h := Handler(Deps{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(http.MethodGet, "/api/v1/search", nil))

	if rec.Code == http.StatusOK {
		t.Fatal("GET on a POST-only route should not succeed")
	}
}

func TestSearchRejectsInvalidJSON(t *testing.T) {
	h := Handler(Deps{})

	req := newRequest(http.MethodPost, "/api/v1/search", strings.NewReader("{not json"))
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
	req := newRequest(http.MethodPost, "/api/v1/search", huge)
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
	req := newRequest(http.MethodGet, "/x?limit=7", nil)
	if got := intParam(req, "limit"); got != 7 {
		t.Errorf("got %d, want 7", got)
	}

	bad := newRequest(http.MethodGet, "/x?limit=abc", nil)
	if got := intParam(bad, "limit"); got != 0 {
		t.Errorf("unparseable values should fall back to 0, got %d", got)
	}

	missing := newRequest(http.MethodGet, "/x", nil)
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
		h.ServeHTTP(rec, newRequest(http.MethodPost, "/api/v1/"+path, strings.NewReader("{}")))
		if rec.Code != http.StatusNotFound {
			t.Errorf("/api/v1/%s: got %d, want 404 when writes are disabled", path, rec.Code)
		}
	}
}

func TestNoWriterMeansNoWriteRoutes(t *testing.T) {
	h := Handler(Deps{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(http.MethodPost, "/api/v1/like", strings.NewReader("{}")))
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
		h.ServeHTTP(rec, newRequest(http.MethodPost, "/api/v1/"+c.path, strings.NewReader(c.body)))

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
		{"X dropped it", &write.NotAppliedError{Reason: "like did not stick"}, http.StatusBadGateway},
		// Liking a deleted or private post is the caller's answer, not a fault.
		{"no such post", &write.NotFoundError{Reason: "no post at that address"}, http.StatusNotFound},
		// A plain error from anywhere is a fault of this process until it is
		// classified, so its text is not the caller's to read.
		{"unclassified", errors.New("cdp connection closed"), http.StatusInternalServerError},
	}

	for _, c := range cases {
		h := Handler(Deps{Writer: &fakeWriter{enabled: true, err: c.err}})

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newRequest(http.MethodPost, "/api/v1/like",
			strings.NewReader(`{"handle":"someone","post_id":"222","confirm":"tok"}`)))

		if rec.Code != c.status {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.status)
		}
		var body struct{ Error string }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		want := c.err.Error()
		if c.status == http.StatusInternalServerError {
			want = "internal error"
		}
		if body.Error != want {
			t.Errorf("%s: reported %q, want %q", c.name, body.Error, want)
		}
	}
}

func TestWriteRoutesRejectInvalidJSON(t *testing.T) {
	writer := &fakeWriter{enabled: true}
	h := Handler(Deps{Writer: writer})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(http.MethodPost, "/api/v1/like", strings.NewReader("{not json")))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rec.Code)
	}
	if len(writer.calls) != 0 {
		t.Errorf("a malformed body reached the account: %v", writer.calls)
	}
}

// Same trap on this side: a nil *write.Writer in the interface field must leave
// the routes unregistered rather than panic while building the handler.
func TestATypedNilWriterMeansNoWriteRoutes(t *testing.T) {
	var writer *write.Writer // nil, but not a nil write.Actions

	h := Handler(Deps{Writer: writer})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(http.MethodPost, "/api/v1/like", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404 for a nil writer", rec.Code)
	}
}

// Anything the caller could act on has to say what it was. Collapsing a list id
// that does not exist into "internal error" leaves them re-sending a request
// that will never work, and looking for a server fault that is not there.
func TestFailuresTheCallerCanActOnSayWhatTheyWere(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		want   string
	}{
		{"bad request", &read.InvalidError{Reason: "list id is required"},
			http.StatusBadRequest, "list id is required"},
		{"nothing there", &read.NotFoundError{Reason: "no posts found; the list may not exist"},
			http.StatusNotFound, "no posts found; the list may not exist"},
		{"profile busy", browser.ErrProfileInUse,
			http.StatusServiceUnavailable, browser.ErrProfileInUse.Error()},
		{"shutting down", pool.ErrClosed,
			http.StatusServiceUnavailable, pool.ErrClosed.Error()},
		{"too slow", context.DeadlineExceeded,
			http.StatusGatewayTimeout, "the request did not finish in time"},
	}

	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeErr(rec, nil, c.err)

		if rec.Code != c.status {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.status)
		}
		var body struct{ Error string }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Error != c.want {
			t.Errorf("%s: said %q, want %q", c.name, body.Error, c.want)
		}
	}
}

// A wrapped one still has to be recognised, since the reader returns these from
// under its own context.
func TestAWrappedFailureIsStillClassified(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, nil, fmt.Errorf("read list: %w", &read.NotFoundError{Reason: "no posts found"}))

	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404 for a wrapped not-found", rec.Code)
	}
}

// newRequest builds a request the way a client on this machine makes one.
//
// The Host matters now: the handler refuses anything that does not address this
// server by name, which is what stops a web page reaching the port by DNS
// rebinding. httptest would otherwise say "example.com".
func newRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Host = "127.0.0.1:18110"
	return r
}

// A page on a domain the attacker controls, re-resolved to 127.0.0.1, reaches
// the port with the name it dialled still in Host. That name is the tell.
func TestARequestThatDoesNotAddressThisServerIsRefused(t *testing.T) {
	h := Handler(Deps{ListenAddr: "127.0.0.1:18110"})

	for _, host := range []string{
		"evil.example", "attacker.test:18110", "", "127.0.0.1.evil.example",
		// Normalising case and the trailing dot must not become a way in.
		"EVIL.EXAMPLE", "evil.example.", "localhost.evil.example",
	} {
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		r.Host = host

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: got %d, want 403", host, rec.Code)
		}
	}
}

func TestTheServersOwnNamesAreAccepted(t *testing.T) {
	h := Handler(Deps{ListenAddr: "127.0.0.1:18110"})

	// Case and a trailing dot name the same host, so a client using either is
	// this server's own and must not be turned away.
	for _, host := range []string{
		"127.0.0.1:18110", "localhost:18110", "127.0.0.1", "localhost", "[::1]:18110", "::1",
		"LOCALHOST:18110", "LocalHost", "localhost.", "localhost.:18110",
	} {
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		r.Host = host

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q: got %d, want 200", host, rec.Code)
		}
	}
}

// Any Origin means a browser is calling, whatever Host it used. None of them are
// accepted: no response here carries CORS headers, so even a page served from
// this machine could not read one, and allowing it would only widen what a
// hostile page can set in motion.
func TestARequestFromAnyBrowserPageIsRefused(t *testing.T) {
	h := Handler(Deps{ListenAddr: "127.0.0.1:18110"})

	for _, origin := range []string{
		"https://evil.example", "http://attacker.test:3000", "null",
		"http://localhost:3000", "http://127.0.0.1:18110",
	} {
		r := newRequest(http.MethodGet, "/health", nil)
		r.Header.Set("Origin", origin)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Origin %q: got %d, want 403", origin, rec.Code)
		}
	}
}

// A client that is not a browser sends no Origin, which is every client this
// server actually has.
func TestAClientThatIsNotABrowserIsAccepted(t *testing.T) {
	h := Handler(Deps{ListenAddr: "127.0.0.1:18110"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rec.Code)
	}
}

// The guard covers the MCP endpoint too, which is where the gap was reported.
func TestTheGuardCoversTheMCPEndpoint(t *testing.T) {
	h := Handler(Deps{ListenAddr: "127.0.0.1:18110",
		MCP: mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)})

	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	r.Host = "evil.example"

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403 on /mcp", rec.Code)
	}
}

// Binding beyond loopback must not drop the Host check. Rebinding works just as
// well against a LAN address, and choosing to expose the session to a network is
// not choosing to expose it to every website.
func TestANonLoopbackBindStillChecksHost(t *testing.T) {
	h := Handler(Deps{ListenAddr: "0.0.0.0:18110"})

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	r.Host = "evil.example:18110" // rebound to the victim's LAN address
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403 for a rebound name on a wildcard bind", rec.Code)
	}
}

// Which is why a name of its own has to be given rather than guessed: no client
// sends "0.0.0.0", so a LAN deployment reached by hostname names it explicitly.
func TestAnAllowedHostIsAccepted(t *testing.T) {
	h := Handler(Deps{ListenAddr: "0.0.0.0:18110", AllowedHosts: []string{"my-machine.local"}})

	for host, want := range map[string]int{
		"my-machine.local:18110": http.StatusOK,
		"MY-MACHINE.LOCAL":       http.StatusOK,
		"127.0.0.1:18110":        http.StatusOK,
		"evil.example:18110":     http.StatusForbidden,
	} {
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		r.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("Host %q: got %d, want %d", host, rec.Code, want)
		}
	}
}

// A bind to one address answers to that address without being told.
func TestTheBoundAddressIsAccepted(t *testing.T) {
	h := Handler(Deps{ListenAddr: "192.168.1.5:18110"})

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	r.Host = "192.168.1.5:18110"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("got %d, want 200 for the address it bound", rec.Code)
	}
}

// A recognised failure reports its own message, not the one it arrived wrapped
// in -- that is where the internals are. A profile already in use carries the
// path of the lock that says so.
func TestAClassifiedFailureDoesNotLeakItsWrapper(t *testing.T) {
	secret := "/Users/someone/.x-browser-mcp/profile/SingletonLock"

	for _, err := range []error{
		fmt.Errorf("%w (%s)", browser.ErrProfileInUse, secret),
		fmt.Errorf("read list: %w", &read.NotFoundError{Reason: "no posts found"}),
		fmt.Errorf("open %s: %w", secret, pool.ErrClosed),
	} {
		rec := httptest.NewRecorder()
		writeErr(rec, nil, err)

		if body := rec.Body.String(); strings.Contains(body, secret) {
			t.Errorf("the response carried a local path: %s", body)
		}
	}
}
