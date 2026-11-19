package boosty

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A server that sends headers and a few bytes and then goes silent must not
// hold the download forever: DownloadHTTP has no Client.Timeout, so only the
// idle-read watchdog ends it.
func TestDownload_IdleWatchdogCancelsStalledBody(t *testing.T) {
	savedDelays, savedIdle := RetryDelays, downloadIdleTimeout
	RetryDelays, downloadIdleTimeout = []time.Duration{time.Millisecond}, 100*time.Millisecond
	defer func() { RetryDelays, downloadIdleTimeout = savedDelays, savedIdle }()

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, "0123456789")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	c := &Client{
		Tokens:       &Tokens{AccessToken: "t"},
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}

	start := time.Now()
	err := c.Download(DownloadRequest{URL: server.URL + "/v.mp4", Path: filepath.Join(t.TempDir(), "video_001.mp4")})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Download of a stalled body succeeded")
	}
	if !strings.Contains(err.Error(), errIdleTimeout.Error()) {
		t.Errorf("error = %v, want it to name the idle timeout", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Download took %s, want about two 100ms idle timeouts", elapsed)
	}
}

// A transport error renders as `Get "<URL>": ...`; the signed query in that
// URL is a credential and must not reach the returned error.
func TestDownload_TransportErrorOmitsSignedURL(t *testing.T) {
	saved := RetryDelays
	RetryDelays = []time.Duration{time.Millisecond}
	defer func() { RetryDelays = saved }()

	closed := httptest.NewServer(http.NotFoundHandler())
	dead := closed.URL
	closed.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "t"},
		DownloadHTTP: &http.Client{},
		Log:          discardLogger{},
	}
	err := c.Download(DownloadRequest{
		URL:  dead + "/v.mp4?sig=SECRET&srcIp=1.2.3.4",
		Path: filepath.Join(t.TempDir(), "video_001.mp4"),
	})
	if err == nil {
		t.Fatal("Download from a closed server succeeded")
	}
	for _, leak := range []string{"SECRET", "1.2.3.4", dead} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks %q", err, leak)
		}
	}
}
