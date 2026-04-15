package boosty

import "encoding/json"

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
	Price             int                `json:"price"`
	CurrencyPrices    map[string]float64 `json:"currencyPrices,omitempty"`
	SubscriptionLevel *PostSubLevel      `json:"subscriptionLevel,omitempty"`
	User              PostUser           `json:"user"`
	Data              []ContentBlock     `json:"data"`
	Tags              []Tag              `json:"tags"`
	Count             PostCount          `json:"count"`
	SignedQuery       string             `json:"signedQuery"`
}

// PostSubLevel is the minimum subscription tier required to access a post.
type PostSubLevel struct {
	ID             int64              `json:"id"`
	Name           string             `json:"name"`
	Price          int                `json:"price"`
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

// ContentBlock is a piece of post content (text, image, video, link).
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
	ID         string            `json:"id"`
	AuthorID   int64             `json:"intId"`
	Author     CommentAuthor     `json:"author"`
	Data       []ContentBlock    `json:"data"`
	CreatedAt  int64             `json:"createdAt"`
	ReplyCount int               `json:"replyCount"`
	Replies    *CommentsResponse `json:"replies,omitempty"`
}

// CommentAuthor is the author of a comment.
type CommentAuthor struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatarUrl"`
}

// SubscriptionsResponse is the list of user subscriptions.
type SubscriptionsResponse struct {
	Data  []Subscription `json:"data"`
	Total int            `json:"total"`
}

// Subscription is a user's subscription to a blog.
type Subscription struct {
	Name              string            `json:"name"`
	Price             int               `json:"price"`
	OnTime            int64             `json:"onTime"`
	OffTime           int64             `json:"offTime"`
	IsPaused          bool              `json:"isPaused"`
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
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Price      int    `json:"price"`
	IsArchived bool   `json:"isArchived"`
}
