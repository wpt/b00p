package boosty

import (
	"fmt"
	neturl "net/url"
)

// API URL builders. Exported so library callers can combine them with
// Client.GetJSON for endpoints not covered by a typed iterator.

// PostsURL returns the URL for listing blog posts.
// The offset is opaque server-supplied data which can contain `+`, `=`, `&`,
// or `%` and so must be query-escaped before being concatenated into the URL.
func PostsURL(blogName string, limit int, offset string) string {
	u := fmt.Sprintf("%s/v1/blog/%s/post/?limit=%d", BaseURL, blogName, limit)
	if offset != "" {
		u += "&offset=" + neturl.QueryEscape(offset)
	}
	return u
}

// PostURL returns the URL for a single post.
func PostURL(blogName, postID string) string {
	return fmt.Sprintf("%s/v1/blog/%s/post/%s", BaseURL, blogName, postID)
}

// defaultReplyLimit caps how many replies Boosty inlines per top-level
// comment. Without this query param the server inlines 0 replies even when
// replyCount > 0 (the parent endpoint returns replies.data=[] with
// isLast=true regardless of the actual replyCount), so we silently lose
// every reply body. 100 covers the vast majority of threads. Threads with
// MORE than 100 replies trip the per-thread cap detection in
// syncer.downloadComments which marks the post with state.CommentsCapped;
// classifyPost then suppresses the catch-up refetch (disk count cannot
// reach API count via this endpoint). See pkg/syncer/save.go and
// pkg/syncer/classify.go for the suppression contract.
const defaultReplyLimit = 100

// CommentsURL returns the URL for post comments. reply_limit is set to
// defaultReplyLimit to force the server to inline replies; see the constant
// for why.
func CommentsURL(blogName, postID string, limit, offset int) string {
	return fmt.Sprintf("%s/v1/blog/%s/post/%s/comment/?limit=%d&offset=%d&reply_limit=%d",
		BaseURL, blogName, postID, limit, offset, defaultReplyLimit)
}

// UserSubscriptionsURL returns the URL for the current user's subscriptions.
func UserSubscriptionsURL() string {
	return BaseURL + "/v1/user/subscriptions"
}
