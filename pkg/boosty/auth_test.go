package boosty

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTokens_IsExpired(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()
	past := time.Now().Add(-time.Hour).UnixMilli()

	tok := &Tokens{ExpiresAt: future}
	if tok.IsExpired() {
		t.Error("IsExpired() = true for future token")
	}

	tok.ExpiresAt = past
	if !tok.IsExpired() {
		t.Error("IsExpired() = false for past token")
	}
}

func TestTokens_IsExpired_Zero(t *testing.T) {
	tok := &Tokens{ExpiresAt: 0}
	if tok.IsExpired() {
		t.Error("IsExpired() = true for zero ExpiresAt, should be false (rely on 401 retry)")
	}
}

func TestLoadTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	content := `{"accessToken":"abc123","refreshToken":"ref456","expiresAt":9999999999999}`
	os.WriteFile(path, []byte(content), 0600)

	tok, err := LoadTokens(path)
	if err != nil {
		t.Fatalf("LoadTokens error: %v", err)
	}
	if tok.AccessToken != "abc123" {
		t.Errorf("AccessToken = %q, want 'abc123'", tok.AccessToken)
	}
	if tok.RefreshToken != "ref456" {
		t.Errorf("RefreshToken = %q, want 'ref456'", tok.RefreshToken)
	}
}

func TestLoadTokens_URLEncodedCookieValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	content := `{%22accessToken%22:%22abc123%22,%22refreshToken%22:%22ref456%22,%22expiresAt%22:9999999999999}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tok, err := LoadTokens(path)
	if err != nil {
		t.Fatalf("LoadTokens error: %v", err)
	}
	if tok.AccessToken != "abc123" || tok.RefreshToken != "ref456" {
		t.Errorf("decoded tokens = %+v", tok)
	}
}

func TestLoadTokens_EmptyAccessToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	os.WriteFile(path, []byte(`{"accessToken":"","refreshToken":"ref"}`), 0600)

	_, err := LoadTokens(path)
	if err == nil {
		t.Error("LoadTokens with empty accessToken should return error")
	}
}

func TestLoadTokens_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	os.WriteFile(path, []byte("not json"), 0600)

	_, err := LoadTokens(path)
	if err == nil {
		t.Error("LoadTokens with invalid JSON should return error")
	}
}

func TestLoadTokens_MissingFile(t *testing.T) {
	_, err := LoadTokens("/nonexistent/auth.json")
	if err == nil {
		t.Error("LoadTokens with missing file should return error")
	}
}

func TestTokens_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	tok := &Tokens{
		AccessToken:  "access",
		RefreshToken: "refresh",
		DeviceID:     "device",
		ExpiresAt:    1234567890,
	}

	if err := tok.SaveTokens(path); err != nil {
		t.Fatalf("SaveTokens error: %v", err)
	}

	loaded, err := LoadTokens(path)
	if err != nil {
		t.Fatalf("LoadTokens error: %v", err)
	}

	if loaded.AccessToken != "access" || loaded.RefreshToken != "refresh" || loaded.DeviceID != "device" {
		t.Errorf("Round-trip failed: %+v", loaded)
	}
}

// The refresh request as Boosty sees it: a JSON POST to /oauth/token/ with
// the browser UA and the field names the server expects. The token values
// carry a backslash, a quote and a newline — the previous fmt.Sprintf body
// produced invalid JSON for them.
func TestRefresh_WireRequest(t *testing.T) {
	const deviceID, refreshToken = "dev\\\"\nid", "ref\\\"\ntoken"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token/" {
			t.Errorf("request = %s %s, want POST /oauth/token/", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := r.Header.Get("User-Agent"); got != UserAgent {
			t.Errorf("User-Agent = %q, want the browser UA", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode wire body: %v", err)
		}
		want := map[string]string{"device_id": deviceID, "refresh_token": refreshToken}
		if !maps.Equal(body, want) {
			t.Errorf("wire body = %q, want %q", body, want)
		}
		w.Write([]byte(`{"access_token":"a2","refresh_token":"r2","expires_in":3600}`))
	}))
	defer server.Close()

	tok := &Tokens{AccessToken: "a1", RefreshToken: refreshToken, DeviceID: deviceID}
	if err := tok.Refresh(rewriteClient(t, server)); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok.AccessToken != "a2" || tok.RefreshToken != "r2" || tok.DeviceID != deviceID || tok.ExpiresAt == 0 {
		t.Errorf("tokens after refresh = %+v, want a2/r2, same device, expiresAt set", tok)
	}
}

// An antivirus, indexer or backup handle on auth.json blocks the rename on
// Windows until it closes; SaveTokens must wait it out instead of failing.
// Go opens files without FILE_SHARE_DELETE, so os.Open stands in for it.
func TestSaveTokens_RetriesWhileFileLockedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("an open handle blocks a rename only on Windows")
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := (&Tokens{AccessToken: "a1"}).SaveTokens(path); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	saved := lockRetryWindow
	lockRetryWindow = 0
	err = (&Tokens{AccessToken: "a2"}).SaveTokens(path)
	lockRetryWindow = saved
	if err == nil {
		f.Close()
		t.Fatal("SaveTokens without retries succeeded under an open handle; the test no longer models the lock")
	}

	time.AfterFunc(200*time.Millisecond, func() { f.Close() })
	if err := (&Tokens{AccessToken: "a2"}).SaveTokens(path); err != nil {
		t.Fatalf("SaveTokens while locked: %v", err)
	}
	if loaded, err := LoadTokens(path); err != nil || loaded.AccessToken != "a2" {
		t.Errorf("auth.json = %+v, %v; want a2", loaded, err)
	}
}

func TestRefresh_MissingAccessTokenDoesNotMutate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer server.Close()

	tok := &Tokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		DeviceID:     "device",
		ExpiresAt:    123,
	}

	err := tok.Refresh(rewriteClient(t, server))
	if err == nil {
		t.Fatal("Refresh should reject a 200 response without access_token")
	}
	if !strings.Contains(err.Error(), "missing access_token") {
		t.Fatalf("Refresh error = %q, want missing access_token", err)
	}
	if tok.AccessToken != "old-access" {
		t.Errorf("AccessToken mutated to %q, want old-access", tok.AccessToken)
	}
	if tok.RefreshToken != "old-refresh" {
		t.Errorf("RefreshToken mutated to %q, want old-refresh", tok.RefreshToken)
	}
	if tok.ExpiresAt != 123 {
		t.Errorf("ExpiresAt mutated to %d, want 123", tok.ExpiresAt)
	}
}

// Regression: the previous SaveTokens passed dir="" to os.CreateTemp when
// path was a bare filename like "auth.json", which silently used os.TempDir
// and made the "atomic rename" cross-filesystem. This test runs the save
// from inside a temp working directory using a bare filename to ensure the
// temp file lands in the same directory.
func TestTokens_SaveTokens_BareFilename(t *testing.T) {
	dir := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prevWD); err != nil {
			t.Logf("restore cwd: %v", err)
		}
	})

	tok := &Tokens{AccessToken: "a", RefreshToken: "r"}
	if err := tok.SaveTokens("auth.json"); err != nil {
		t.Fatalf("SaveTokens(bare): %v", err)
	}

	// File must exist in cwd.
	if _, err := os.Stat(filepath.Join(dir, "auth.json")); err != nil {
		t.Fatalf("expected auth.json in cwd: %v", err)
	}

	loaded, err := LoadTokens(filepath.Join(dir, "auth.json"))
	if err != nil || loaded.AccessToken != "a" {
		t.Errorf("round-trip mismatch: %+v err=%v", loaded, err)
	}
}
