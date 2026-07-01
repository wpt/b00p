package boosty

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// Regression: the previous Refresh body was built by fmt.Sprintf and would
// produce invalid JSON if RefreshToken or DeviceID contained backslashes,
// quotes, or newlines. The typed refreshRequest must round-trip cleanly
// through json.Marshal regardless of token contents.
func TestRefreshRequest_EscapesSpecialChars(t *testing.T) {
	cases := []struct {
		name         string
		deviceID     string
		refreshToken string
	}{
		{"backslash", `dev\id`, `ref\token`},
		{"quote", `dev"id`, `ref"token`},
		{"newline", "dev\nid", "ref\ntoken"},
		{"all", "dev\\\"\nid", "ref\\\"\ntoken"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(refreshRequest{
				DeviceID:     tc.deviceID,
				RefreshToken: tc.refreshToken,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got refreshRequest
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal %q: %v", body, err)
			}
			if got.DeviceID != tc.deviceID || got.RefreshToken != tc.refreshToken {
				t.Errorf("round-trip mismatch: got %+v, want device=%q refresh=%q",
					got, tc.deviceID, tc.refreshToken)
			}
		})
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
