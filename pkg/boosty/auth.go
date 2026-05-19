package boosty

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/wpt/b00p/pkg/fileutil"
)

// Tokens holds authentication credentials for the Boosty API.
type Tokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	DeviceID     string `json:"deviceId,omitempty"`
	ExpiresAt    int64  `json:"expiresAt"`
}

// IsExpired reports whether the access token has expired.
// Returns false if ExpiresAt is not set (0), relying on 401 retry instead.
func (t *Tokens) IsExpired() bool {
	if t.ExpiresAt == 0 {
		return false
	}
	return time.Now().UnixMilli() >= t.ExpiresAt
}

// LoadTokens reads authentication tokens from a JSON file.
func LoadTokens(path string) (*Tokens, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read auth file: %w", err)
	}

	var tokens Tokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, fmt.Errorf("parse auth file: %w", err)
	}

	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("accessToken is empty in %s", path)
	}

	return &tokens, nil
}

// SaveTokens writes tokens to a JSON file atomically (temp + fsync + rename),
// so an interrupted write cannot truncate an existing auth.json. Permissions
// are restricted to 0600 since the file holds bearer credentials.
func (t *Tokens) SaveTokens(path string) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	return fileutil.WriteFileAtomic(path, data, 0600)
}

// refreshRequest is the POST body sent to /oauth/token/.
// Modeled as a typed struct so json.Marshal handles escaping for arbitrary
// token contents (backslashes, quotes, newlines) — earlier code stitched the
// body with fmt.Sprintf and would have produced invalid JSON on such tokens.
type refreshRequest struct {
	DeviceID     string `json:"device_id"`
	RefreshToken string `json:"refresh_token"`
}

// refreshResponse is the /oauth/token/ success payload.
type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// Refresh obtains a new access token using the refresh token.
func (t *Tokens) Refresh(httpClient *http.Client) error {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	body, err := json.Marshal(refreshRequest{
		DeviceID:     t.DeviceID,
		RefreshToken: t.RefreshToken,
	})
	if err != nil {
		return fmt.Errorf("marshal refresh body: %w", err)
	}
	req, err := http.NewRequest("POST", BaseURL+"/oauth/token/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// /oauth/token/ sits behind the same edge as the API and rejects naked
	// (no UA) requests as bots; mirror what rawRequest sends on every other
	// call so refresh does not become the one bot-flagged endpoint.
	req.Header.Set("User-Agent", UserAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token refresh failed (status %d). Get new tokens from browser cookies and update auth.json", resp.StatusCode)
	}

	var result refreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode refresh response: %w", err)
	}

	t.AccessToken = result.AccessToken
	if result.RefreshToken != "" {
		t.RefreshToken = result.RefreshToken
	}
	t.ExpiresAt = time.Now().UnixMilli() + result.ExpiresIn*1000

	return nil
}
