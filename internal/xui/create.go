package xui

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/SohrabZ/x-browser-mcp/internal/model"
)

// createOps are the operations X's web client sends to publish a post: the
// ordinary one, and the one for a post longer than the standard limit. A reply
// is the same request with the post it answers among its variables.
var createOps = map[string]bool{"CreateTweet": true, "CreateNoteTweet": true}

// IsCreatePost reports whether a request URL publishes a post.
//
// X addresses a GraphQL operation as /i/api/graphql/<query id>/<name>. The query
// id changes with every client release and the name has not, so the name is what
// is matched.
func IsCreatePost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	n := len(parts)
	return n >= 5 && parts[n-5] == "i" && parts[n-4] == "api" && parts[n-3] == "graphql" && createOps[parts[n-1]]
}

// CreatedPost reads X's answer to a request that publishes a post. It reports
// the new post's id when X created one, and otherwise what X said went wrong,
// which is empty when it said nothing usable.
//
// The id is read where X's own client looks for it: data.create_tweet, or
// data.notetweet_create for a long post, then tweet_results.result.legacy.id_str.
// The client treats a missing id there as "failed to create", so this does too,
// whatever else the answer holds.
func CreatedPost(body []byte) (id, refusal string) {
	var answer struct {
		// Each operation is decoded on its own, so an unexpected field elsewhere
		// in the answer cannot make the whole of it unreadable.
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", ""
	}

	for _, op := range []string{"create_tweet", "notetweet_create"} {
		var created struct {
			TweetResults struct {
				Result struct {
					Legacy struct {
						IDStr string `json:"id_str"`
					} `json:"legacy"`
				} `json:"result"`
			} `json:"tweet_results"`
		}
		if raw, ok := answer.Data[op]; !ok || json.Unmarshal(raw, &created) != nil {
			continue
		}
		if id := created.TweetResults.Result.Legacy.IDStr; ValidPostID(id) {
			return id, ""
		}
	}
	for _, e := range answer.Errors {
		if msg := model.Normalize(e.Message); msg != "" {
			return "", model.Excerpt(msg, maxRefusal)
		}
	}
	return "", ""
}

// maxRefusal bounds how much of X's error message is passed on. X's messages are
// a sentence; anything longer is not one of them.
const maxRefusal = 200
