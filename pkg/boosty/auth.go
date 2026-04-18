package boosty

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// SaveTokens writes tokens to a JSON file atomically (temp + rename), so an
// interrupted write cannot truncate an existing auth.json.
func (t *Tokens) SaveTokens(path string) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}

	dir, name := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// Refresh obtains a new access token using the refresh token.
func (t *Tokens) Refresh(httpClient *http.Client) error {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	body := fmt.Sprintf(`{"device_id":"%s","refresh_token":"%s"}`, t.DeviceID, t.RefreshToken)
	req, err := http.NewRequest("POST", BaseURL+"/oauth/token/", strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token refresh failed (status %d). Get new tokens from browser cookies and update auth.json", resp.StatusCode)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
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
