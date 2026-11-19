package boosty

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/wpt/b00p/pkg/fileutil"
)

// ErrRefreshRejected marks a token refresh the server rejected with a 4xx
// status: the refresh token itself is dead (expired or revoked), so retrying
// the identical request cannot succeed. GetJSON fails fast on it instead of
// burning the backoff schedule. The message doubles as the user-facing
// instruction.
//
//lint:ignore ST1005 the message renders mid-sentence after "(status %d)." — the capital starts the user-facing instruction sentence, not an error string.
var ErrRefreshRejected = errors.New("Get new tokens from browser cookies and update auth.json")

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
	data = decodeAuthText(data)

	var tokens Tokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		raw := strings.TrimSpace(string(data))
		// PathUnescape (not QueryUnescape) — both decode %XX identically, but
		// QueryUnescape also turns a literal '+' into a space, which would
		// silently corrupt a token containing '+' (common in base64/JWT values).
		if decoded, decErr := neturl.PathUnescape(raw); decErr == nil && decoded != raw {
			if retryErr := json.Unmarshal([]byte(decoded), &tokens); retryErr == nil {
				if tokens.AccessToken == "" {
					return nil, fmt.Errorf("accessToken is empty in %s", path)
				}
				return &tokens, nil
			}
		}
		return nil, fmt.Errorf("parse auth file: %w", err)
	}

	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("accessToken is empty in %s", path)
	}

	return &tokens, nil
}

// decodeAuthText returns data as UTF-8. Windows editors and PowerShell 5.1
// (Set-Content, Out-File, >) save auth.json with a UTF-8 BOM or as UTF-16
// with a BOM, which json.Unmarshal rejects as an invisible character.
func decodeAuthText(data []byte) []byte {
	if b, ok := bytes.CutPrefix(data, []byte("\xef\xbb\xbf")); ok {
		return b
	}
	if len(data) < 2 || !(data[0] == 0xff && data[1] == 0xfe || data[0] == 0xfe && data[1] == 0xff) {
		return data
	}
	order := binary.ByteOrder(binary.LittleEndian)
	if data[0] == 0xfe {
		order = binary.BigEndian
	}
	units := make([]uint16, (len(data)-2)/2)
	for i := range units {
		units[i] = order.Uint16(data[2+2*i:])
	}
	return []byte(string(utf16.Decode(units)))
}

// SaveTokens writes tokens to a JSON file atomically (see
// fileutil.WriteFileAtomic: temp + fsync + rename, Windows lock retries, a
// symlinked path keeps its link), so an interrupted write cannot truncate an
// existing auth.json. Permissions are restricted to 0600 since the file holds
// bearer credentials.
func (t *Tokens) SaveTokens(path string) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	return fileutil.WriteFileAtomic(path, data, 0600)
}

// refreshRequest is the POST body sent to /oauth/token/. A typed struct so
// json.Marshal handles escaping for arbitrary token contents (backslashes,
// quotes, newlines).
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
		if deterministic4xx(resp.StatusCode) {
			// Deterministic rejection: the refresh body (device_id +
			// refresh_token) cannot change between attempts, so a 4xx
			// verdict is permanent. Wrap the sentinel so callers fail fast.
			return fmt.Errorf("token refresh failed (status %d). %w", resp.StatusCode, ErrRefreshRejected)
		}
		// 5xx / 429: transient edge failure — the retry loop may recover,
		// and the tokens are fine, so don't send the user to re-extract them.
		return fmt.Errorf("token refresh failed (status %d): Boosty server error, try again later", resp.StatusCode)
	}

	var result refreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode refresh response: %w", err)
	}
	if result.AccessToken == "" {
		return fmt.Errorf("token refresh failed: response missing access_token")
	}

	t.AccessToken = result.AccessToken
	if result.RefreshToken != "" {
		t.RefreshToken = result.RefreshToken
	}
	// Clamp ExpiresIn to a sensible floor — a degenerate server response of
	// 0 / negative would mark the freshly-issued token as already expired,
	// so the next API call would do one redundant reactive refresh before
	// proceeding (not a loop — the second refresh succeeds and IsExpired is
	// then false). Real Boosty tokens last hours; an hour floor avoids that
	// wasted round-trip without affecting normal behavior.
	expiresIn := result.ExpiresIn
	if expiresIn < 60 {
		expiresIn = 3600
	}
	t.ExpiresAt = time.Now().UnixMilli() + expiresIn*1000

	return nil
}
