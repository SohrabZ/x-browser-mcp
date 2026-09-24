package xui

import "testing"

func TestIsCreatePostMatchesTheOperationByName(t *testing.T) {
	cases := map[string]bool{
		"https://x.com/i/api/graphql/GYdIGqVWfZNho79bQ2XDoA/CreateTweet":     true,
		"https://x.com/i/api/graphql/anotherQueryID/CreateTweet?variables=1": true,
		"https://x.com/i/api/graphql/abc/CreateNoteTweet":                    true,
		"http://127.0.0.1:5555/i/api/graphql/abc/CreateTweet":                true,
		"https://x.com/i/api/graphql/og4a4SdSF3WiQkkwaPCdPg/HomeTimeline":    false,
		"https://x.com/i/api/graphql/abc/CreateRetweet":                      false,
		"https://x.com/i/api/graphql/abc/CreateScheduledTweet":               false,
		"https://x.com/CreateTweet":                                          false,
		"https://x.com/someone/status/1/CreateTweet":                         false,
		"https://x.com/i/api/1.1/graphql/app_context.json":                   false,
		"::not a url": false,
	}
	for url, want := range cases {
		if got := IsCreatePost(url); got != want {
			t.Errorf("IsCreatePost(%q) = %v, want %v", url, got, want)
		}
	}
}

// The id is read where X's own client reads it, so the verdict is the one X
// shows the user.
func TestCreatedPostReadsTheIDWhereXsClientDoes(t *testing.T) {
	cases := map[string]string{
		"a post":                        `{"data":{"create_tweet":{"tweet_results":{"result":{"rest_id":"111","legacy":{"id_str":"111"}}}}}}`,
		"a long post":                   `{"data":{"notetweet_create":{"tweet_results":{"result":{"legacy":{"id_str":"222"}}}}}}`,
		"an unexpected field beside it": `{"data":{"viewer":7,"create_tweet":{"tweet_results":{"result":{"legacy":{"id_str":"333"}}}}}}`,
	}
	want := map[string]string{"a post": "111", "a long post": "222", "an unexpected field beside it": "333"}
	for name, body := range cases {
		id, refusal := CreatedPost([]byte(body))
		if id != want[name] || refusal != "" {
			t.Errorf("%s: got id %q, refusal %q; want id %q", name, id, refusal, want[name])
		}
	}
}

// X refuses some posts outright -- a duplicate, a rate limit -- and says why. An
// answer with no id and no reason is not a post either.
func TestCreatedPostReportsARefusalAndNothingElse(t *testing.T) {
	id, refusal := CreatedPost([]byte(`{"errors":[{"message":"Authorization: Status is a duplicate. (187)","code":187}],"data":{}}`))
	if id != "" || refusal != "Authorization: Status is a duplicate. (187)" {
		t.Errorf("duplicate: got id %q, refusal %q", id, refusal)
	}

	for name, body := range map[string]string{
		"no id in the result":      `{"data":{"create_tweet":{"tweet_results":{}}}}`,
		"an id of the wrong shape": `{"data":{"create_tweet":{"tweet_results":{"result":{"legacy":{"id_str":"12a"}}}}}}`,
		"not JSON":                 `<html>rate limited</html>`,
		"nothing":                  ``,
	} {
		if id, refusal := CreatedPost([]byte(body)); id != "" || refusal != "" {
			t.Errorf("%s: got id %q, refusal %q; want neither", name, id, refusal)
		}
	}
}
