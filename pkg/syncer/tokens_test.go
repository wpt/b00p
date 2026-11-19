package syncer

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wpt/b00p/pkg/boosty"
)

// A refresh rotated the pair and saving it failed. The run may keep working
// on the in-memory pair, but it must not end in success: auth.json still
// holds the refresh token the server just killed, and only the error names
// auth.json.new. Here the refresh runs inside MaybeRefreshSignedURLs, which
// logs its GetJSON error and carries on with the list copy.
func TestEngine_TokenSaveFailureFailsRun(t *testing.T) {
	f := newFakeAPI(t)
	f.Media("video.mp4", []byte("VIDEO"))
	post := boosty.Post{
		ID: "p1", Title: "Hello", HasAccess: true, PublishTime: 1700000000, UpdatedAt: 100,
		Data: []boosty.ContentBlock{{
			ID: "vid-1", Type: "ok_video",
			PlayerURLs: []boosty.PlayerURL{{Type: "low", URL: f.MediaURL("video.mp4")}},
		}},
	}
	f.PostsList("myblog", post)
	var perPost atomic.Int32
	f.HandleFunc("GET", "/v1/blog/myblog/post/p1", func(w http.ResponseWriter, r *http.Request) {
		if perPost.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(post)
	})
	f.HandleFunc("POST", "/oauth/token/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600})
	})

	// A directory at the auth path fails the save on every OS; the
	// auth.json.new fallback beside it still works.
	authPath := filepath.Join(t.TempDir(), "auth.json")
	mustMkdir(t, filepath.Join(authPath, "blocker"))
	f.client.AuthPath = authPath
	f.client.Tokens.RefreshToken = "old-refresh"
	saved := boosty.RetryDelays
	boosty.RetryDelays = []time.Duration{time.Millisecond}
	defer func() { boosty.RetryDelays = saved }()

	cfg := Config{Blog: "myblog", OutputDir: t.TempDir(), AutoApply: true}
	err := New(f.client, cfg).DownloadAll()
	if !errors.Is(err, boosty.ErrTokenSaveFailed) {
		t.Fatalf("DownloadAll: err = %v, want ErrTokenSaveFailed", err)
	}
	if _, err := os.Stat(authPath + ".new"); err != nil {
		t.Errorf("auth.json.new: %v, want the rotated pair saved beside auth.json", err)
	}
}
