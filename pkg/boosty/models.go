package boosty

import (
	"bytes"
	"encoding/json"
)

// PostsResponse is the paginated list of posts from the API.
type PostsResponse struct {
	Data  []json.RawMessage `json:"data"`
	Extra PaginationExtra   `json:"extra"`
}

// PaginationExtra contains pagination metadata.
type PaginationExtra struct {
	Offset  string `json:"offset"`
	IsLast  bool   `json:"isLast"`
	IsFirst bool   `json:"isFirst"`
}

// Post represents a single blog post.
type Post struct {
	ID                string             `json:"id"`
	Title             string             `json:"title"`
	CreatedAt         int64              `json:"createdAt"`
	PublishTime       int64              `json:"publishTime"`
	UpdatedAt         int64              `json:"updatedAt"`
	HasAccess         bool               `json:"hasAccess"`
	Price             float64            `json:"price"`
	CurrencyPrices    map[string]float64 `json:"currencyPrices,omitempty"`
	SubscriptionLevel *PostSubLevel      `json:"subscriptionLevel,omitempty"`
	User              PostUser           `json:"user"`
	Data              []ContentBlock     `json:"data"`
	Tags              []Tag              `json:"tags"`
	Count             PostCount          `json:"count"`
	SignedQuery       string             `json:"signedQuery"`
	// Raw is the JSON the post was decoded from; nil for a Post built in
	// code. The fields above model only what b00p reads, so post.json is
	// written from Raw: re-marshalling the struct would drop every field
	// and block shape it does not declare. Code that changes a decoded
	// Post must clear Raw, or the stale bytes are what gets archived.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the post and keeps a copy of data in Raw.
func (p *Post) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	type plain Post
	if err := json.Unmarshal(data, (*plain)(p)); err != nil {
		return err
	}
	p.Raw = bytes.Clone(data)
	return nil
}

// IsStub reports whether a per-post payload cannot be archived: no access
// (the subscription lapsed, or the post was locked server-side, between a
// list call and the per-post call) or no id. Callers must not write such a
// payload over an existing post.json — parsing its empty Data into empty
// media would tick every artefact channel OK and advance state against a
// corrupted copy. An accessible post with no blocks is not a stub on its
// own: some posts are empty; see DegradedFrom.
func (p *Post) IsStub() bool {
	return !p.HasAccess || p.ID == ""
}

// DegradedFrom reports whether p, fetched from the per-post endpoint, is a
// worse copy of listed, the same post from the listing: a stub, a different
// post, or an empty block list where the listing had blocks.
func (p *Post) DegradedFrom(listed *Post) bool {
	return p.IsStub() || p.ID != listed.ID || (len(p.Data) == 0 && len(listed.Data) > 0)
}

// TierName returns the subscription tier name required for the post, or ""
// when the post carries no tier (free post, or the field is absent).
func (p *Post) TierName() string {
	if p.SubscriptionLevel == nil {
		return ""
	}
	return p.SubscriptionLevel.Name
}

// PostSubLevel is the minimum subscription tier required to access a post.
type PostSubLevel struct {
	ID             int64              `json:"id"`
	Name           string             `json:"name"`
	Price          float64            `json:"price"`
	CurrencyPrices map[string]float64 `json:"currencyPrices,omitempty"`
	IsArchived     bool               `json:"isArchived"`
}

// PostUser is the author of a post.
type PostUser struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	BlogURL   string `json:"blogUrl"`
	AvatarURL string `json:"avatarUrl"`
}

// Tag is a post tag.
type Tag struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

// PostCount holds like/comment counts.
type PostCount struct {
	Likes    int `json:"likes"`
	Comments int `json:"comments"`
}

// ContentBlock is a piece of post content (text, image, video, link, list,
// smile, ...).
type ContentBlock struct {
	Type           string      `json:"type"`
	ID             string      `json:"id,omitempty"`
	URL            string      `json:"url,omitempty"`
	Title          string      `json:"title,omitempty"`
	Duration       float64     `json:"duration,omitempty"`
	ViewsCounter   int         `json:"viewsCounter,omitempty"`
	Preview        string      `json:"preview,omitempty"`
	DefaultPreview string      `json:"defaultPreview,omitempty"`
	PlayerURLs     []PlayerURL `json:"playerUrls,omitempty"`
	Content        string      `json:"content,omitempty"`
	Modificator    string      `json:"modificator,omitempty"`
	Width          int         `json:"width,omitempty"`
	Height         int         `json:"height,omitempty"`
	// Style ("ordered" / "unordered") and Items are set on "list" blocks.
	Style string     `json:"style,omitempty"`
	Items []ListItem `json:"items,omitempty"`
	// Name is a "smile" block's emoticon name.
	Name string `json:"name,omitempty"`
}

// ListItem is one entry of a "list" block: its inline runs (text, link,
// smile) and the nested list under it, which takes the parent block's Style.
type ListItem struct {
	Data  []ContentBlock `json:"data"`
	Items []ListItem     `json:"items,omitempty"`
}

// PlayerURL is a video playback URL with quality type.
type PlayerURL struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// CommentsResponse is the paginated list of comments.
type CommentsResponse struct {
	Data  []Comment       `json:"data"`
	Extra PaginationExtra `json:"extra"`
}

// Comment is a post comment.
type Comment struct {
	ID        string         `json:"id"`
	IntID     int64          `json:"intId"` // the comment's own numeric id (NOT the author's — that is Author.ID)
	Author    CommentAuthor  `json:"author"`
	Data      []ContentBlock `json:"data"`
	CreatedAt int64          `json:"createdAt"`
	// IsDeleted marks a comment its author removed: the endpoint still returns
	// it as a stub (empty Data; author and timestamps intact) so reply threads
	// under it keep their parent. Post.Count.Comments and ReplyCount exclude
	// deleted comments, so counting code must skip stubs too.
	IsDeleted  bool              `json:"isDeleted"`
	ReplyCount int               `json:"replyCount"` // live (non-deleted) replies; Replies.Data includes stubs, so the two counts differ in either direction
	Replies    *CommentsResponse `json:"replies,omitempty"`
	// Raw is the JSON the comment was decoded from, replies included; nil
	// for a Comment built in code. comments.json is written from it for the
	// same reason as Post.Raw.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the comment and keeps a copy of data in Raw.
func (c *Comment) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	type plain Comment
	if err := json.Unmarshal(data, (*plain)(c)); err != nil {
		return err
	}
	c.Raw = bytes.Clone(data)
	return nil
}

// CommentAuthor is the author of a comment.
type CommentAuthor struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatarUrl"`
}

// SubscriptionsResponse is the list of user subscriptions.
type SubscriptionsResponse struct {
	Data   []Subscription `json:"data"`
	Total  int            `json:"total"`
	Offset int            `json:"offset"`
}

// Subscription is a user's subscription to a blog.
type Subscription struct {
	Name              string            `json:"name"`
	Price             float64           `json:"price"`
	OnTime            int64             `json:"onTime"`
	OffTime           int64             `json:"offTime"`
	IsPaused          bool              `json:"isPause"` // the API's name; isPaused never matches
	IsSuspended       bool              `json:"isSuspended"`
	Blog              SubscriptionBlog  `json:"blog"`
	SubscriptionLevel SubscriptionLevel `json:"subscriptionLevel"`
}

// SubscriptionBlog is the blog info within a subscription.
type SubscriptionBlog struct {
	BlogURL string    `json:"blogUrl"`
	Title   string    `json:"title"`
	Owner   BlogOwner `json:"owner"`
}

// BlogOwner is the owner of a blog.
type BlogOwner struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// SubscriptionLevel is a subscription tier.
type SubscriptionLevel struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Price      float64 `json:"price"`
	IsArchived bool    `json:"isArchived"`
}
