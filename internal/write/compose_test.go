package write

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/browser"
)

// composePage serves a composer whose submit sends the request X's client sends
// to publish a post, after an unrelated one, and answers it with answer. An
// empty answer leaves the button doing nothing, as when X's page swallows the
// submit.
func composePage(t *testing.T, answer string) (*browser.Page, func()) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs a browser")
	}
	chrome := browser.ChromePathForTest()
	if chrome == "" {
		t.Skip("no Chrome installed")
	}

	submit := ""
	if answer != "" {
		submit = `fetch('/i/api/graphql/q1/HomeTimeline', {method: 'POST', body: '{}'})
			.then(() => fetch('/i/api/graphql/abc/CreateTweet', {method: 'POST', body: '{}'}));`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body>
			<textarea data-testid="tweetTextarea_0"></textarea>
			<button data-testid="tweetButtonInline" onclick="submitPost()">Post</button>
			<script>function submitPost() { %s }</script>
		</body></html>`, submit)
	})
	mux.HandleFunc("/i/api/graphql/q1/HomeTimeline", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{}}`)
	})
	mux.HandleFunc("/i/api/graphql/abc/CreateTweet", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, answer)
	})
	srv := httptest.NewServer(mux)

	session, err := browser.Open(context.Background(), browser.Options{ChromePath: chrome, Headless: true})
	if err != nil {
		srv.Close()
		t.Skipf("cannot start chrome: %v", err)
	}
	page, err := session.Page(context.Background())
	if err != nil {
		_ = session.Close()
		srv.Close()
		t.Fatalf("page: %v", err)
	}
	if err := page.Goto(srv.URL); err != nil {
		page.Close()
		_ = session.Close()
		srv.Close()
		t.Fatalf("goto: %v", err)
	}
	return page, func() {
		page.Close()
		_ = session.Close()
		srv.Close()
	}
}

// A post rests on X's answer: it has no address to reload until X gives it
// one, so the id in the answer is the confirmation.
func TestAPostIsConfirmedByXsAnswer(t *testing.T) {
	p, done := composePage(t, `{"data":{"create_tweet":{"tweet_results":{"result":{"rest_id":"999","legacy":{"id_str":"999"}}}}}}`)
	defer done()

	id, err := compose(p, "hello")
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if id != "999" {
		t.Errorf("id = %q, want the one X gave the post", id)
	}
}

// Waiting for the page to go quiet reported this as posted. X answered, and its
// answer said no.
func TestAPostXRefusedSaysWhy(t *testing.T) {
	p, done := composePage(t, `{"errors":[{"message":"Authorization: Status is a duplicate. (187)","code":187}],"data":{}}`)
	defer done()

	_, err := compose(p, "hello")
	var refused *NotAppliedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "duplicate") {
		t.Errorf("got %v, want X's refusal passed on", err)
	}
}

// A post X never answered may or may not exist. Saying "posted" means nobody
// checks; saying "failed" means it gets sent again.
func TestAPostXNeverAnsweredIsNeitherPostedNorFailed(t *testing.T) {
	defer func(was time.Duration) { createWait = was }(createWait)
	createWait = time.Second

	p, done := composePage(t, "")
	defer done()

	_, err := compose(p, "hello")
	var unsure *UnconfirmedError
	if !errors.As(err, &unsure) {
		t.Fatalf("got %v, want an UnconfirmedError", err)
	}
	if !strings.Contains(unsure.Reason, "check the account before sending it again") {
		t.Errorf("reason %q should tell the caller to check before retrying", unsure.Reason)
	}
}

// An answer is only a confirmation when it holds the new post, which is the test
// X's own client applies.
func TestAnAnswerWithoutAPostIsNotAConfirmation(t *testing.T) {
	p, done := composePage(t, `{"data":{"create_tweet":{"tweet_results":{}}}}`)
	defer done()

	_, err := compose(p, "hello")
	var unsure *UnconfirmedError
	if !errors.As(err, &unsure) {
		t.Errorf("got %v, want an UnconfirmedError", err)
	}
}
