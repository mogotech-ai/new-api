package common

import (
	"encoding/base64"
	"errors"
	"strings"
)

type SIWCCredential struct {
	ClientID          string `json:"client_id"`
	AccessToken       string `json:"access_token"`
	RefreshToken      string `json:"refresh_token"`
	ExpiresAt         int64  `json:"expires_at"`
	EarliestRefreshAt int64  `json:"earliest_refresh_at"`
	ReauthRequired    bool   `json:"reauth_required,omitempty"`
	Subject           string `json:"siwc_subject,omitempty"`
}

// ParseSIWCCredential distinguishes imported OAuth sessions from ordinary API keys.
// JWT claims are scheduling/correlation metadata; OpenAI validates the bearer.
func ParseSIWCCredential(raw string) (*SIWCCredential, error) {
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return nil, nil
	}
	var fields map[string]RawMessage
	if err := UnmarshalJsonStr(raw, &fields); err != nil {
		return nil, errors.New("invalid SIWC credential JSON")
	}
	_, clientPresent := fields["client_id"]
	_, accessPresent := fields["access_token"]
	_, refreshPresent := fields["refresh_token"]
	if !clientPresent && !accessPresent && !refreshPresent {
		return nil, nil
	}
	var key SIWCCredential
	if err := UnmarshalJsonStr(raw, &key); err != nil {
		return nil, errors.New("invalid SIWC credential JSON")
	}
	if strings.TrimSpace(key.ClientID) == "" || (!key.ReauthRequired && (strings.TrimSpace(key.AccessToken) == "" || strings.TrimSpace(key.RefreshToken) == "")) {
		return nil, errors.New("SIWC credentials require client_id, access_token and refresh_token")
	}
	if key.ClientID == "dynamic_agent_client" || key.ClientID == "app_EMoamEEZ73f0CkXaXp7hrann" {
		return nil, errors.New("SIWC credentials require the client_id issued for this registration")
	}
	if key.ReauthRequired {
		key.AccessToken, key.RefreshToken = "", ""
		return &key, nil
	}
	parts := strings.Split(key.AccessToken, ".")
	if len(parts) == 3 {
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err == nil {
			var claims struct {
				ExpiresAt int64  `json:"exp"`
				Subject   string `json:"sub"`
			}
			if Unmarshal(payload, &claims) == nil {
				key.Subject = claims.Subject
				if key.ExpiresAt == 0 {
					key.ExpiresAt = claims.ExpiresAt
				}
			}
		}
	}
	if key.ExpiresAt <= 0 || key.EarliestRefreshAt < 0 {
		return nil, errors.New("SIWC credentials require a valid expires_at or access-token expiry")
	}
	return &key, nil
}

func (key *SIWCCredential) SessionKey() string {
	return key.ClientID + "\x00" + key.Subject
}

func IsSIWCCredential(raw string) bool {
	key, _ := ParseSIWCCredential(raw)
	return key != nil
}
