package boosty

import (
	"os"
	"path/filepath"
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
