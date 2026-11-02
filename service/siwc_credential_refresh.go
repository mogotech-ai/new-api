package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/tidwall/sjson"
)

const (
	siwcTokenURL     = "https://auth.openai.com/api/accounts/oauth/token"
	siwcResource     = "https://api.openai.com/v1"
	siwcMaxExpiresIn = 30 * 24 * 60 * 60
	// Half the documented one-hour access-token lifetime, so a freshly issued token is never due again.
	siwcMaxRefreshLead = 30 * time.Minute
)

var siwcRefreshOnce sync.Once

// Non-master nodes only see a rotated key after SYNC_FREQUENCY, so refresh two sync periods early, within the cap.
func siwcRefreshDue(key *common.SIWCCredential) bool {
	now := time.Now()
	lead := min(max(5*time.Minute, 2*time.Duration(common.SyncFrequency)*time.Second), siwcMaxRefreshLead)
	return !key.ReauthRequired && key.ExpiresAt <= now.Add(lead).Unix() && key.EarliestRefreshAt <= now.Unix()
}

func StartSIWCCredentialAutoRefreshTask() {
	siwcRefreshOnce.Do(func() {
		if !common.IsMasterNode {
			return
		}
		// Upstream earliest_refresh_at only allows refresh about six minutes before expiry (observed live).
		if 2*time.Duration(common.SyncFrequency)*time.Second > 5*time.Minute {
			common.SysError(common.LogText("SYNC_FREQUENCY=%ds is above 150s; SIWC tokens can only be refreshed minutes before expiry, so non-master nodes may send expired SIWC access tokens", common.SyncFrequency))
		}
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				RefreshSIWCCredentials(context.Background())
				<-ticker.C
			}
		}()
	})
}

// RefreshSIWCCredentials handles every eligible channel, independently of its ID.
func RefreshSIWCCredentials(ctx context.Context) {
	for offset := 0; ; offset += 200 {
		var channels []*model.Channel
		if err := model.DB.WithContext(ctx).Select("id", "key").
			Where("type = ? AND status IN ?", constant.ChannelTypeOpenAI, []int{common.ChannelStatusEnabled, common.ChannelStatusAutoDisabled}).
			Order("id").Offset(offset).Limit(200).Find(&channels).Error; err != nil {
			logger.LogError(ctx, "SIWC credential refresh: channel query failed")
			return
		}
		for _, channel := range channels {
			key, err := common.ParseSIWCCredential(channel.Key)
			if err != nil || key == nil || !siwcRefreshDue(key) {
				continue
			}
			if err := RefreshSIWCChannelCredential(ctx, channel.Id); err != nil {
				logger.LogWarn(ctx, common.LogText("SIWC credential refresh failed: channel_id=%d error=%v", channel.Id, err))
			}
		}
		if len(channels) < 200 {
			return
		}
	}
}

func RefreshSIWCChannelCredential(ctx context.Context, channelID int) error {
	channel, err := model.GetChannelById(channelID, true)
	if err != nil {
		return err
	}
	initial, err := common.ParseSIWCCredential(channel.Key)
	if err != nil || initial == nil {
		return errors.New("channel does not contain valid SIWC credentials")
	}
	owner := common.GetRandomString(32)
	lease, acquired, err := model.AcquireOAuthCredentialLease(initial.SessionKey(), owner, time.Now().Add(time.Minute).Unix())
	if err != nil || !acquired {
		return err
	}
	defer model.ReleaseSystemTaskLock(lease, owner)
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	channel, err = model.GetChannelById(channelID, true)
	if err != nil {
		return err
	}
	if channel.Type != constant.ChannelTypeOpenAI || channel.ChannelInfo.IsMultiKey {
		return errors.New("SIWC credentials require a single-key OpenAI channel")
	}
	key, err := common.ParseSIWCCredential(channel.Key)
	if err != nil || key == nil {
		return errors.New("channel does not contain valid SIWC credentials")
	}
	if key.SessionKey() != initial.SessionKey() {
		return nil
	}
	if !siwcRefreshDue(key) {
		return nil
	}
	if err := ValidateSIWCChannelSession(channel); err != nil {
		return err
	}
	status, token, err := postSIWCToken(refreshCtx, channel.GetSetting().Proxy,
		url.Values{"grant_type": {"refresh_token"}, "client_id": {key.ClientID}, "refresh_token": {key.RefreshToken}, "resource": {siwcResource}})
	if err != nil {
		return err
	}
	updated := []byte(channel.Key)
	if status != http.StatusOK {
		switch token.errorCode() {
		case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
			updated, err = setSIWCFields(updated, map[string]any{"access_token": "", "refresh_token": "", "id_token": "", "reauth_required": true, "siwc_subject": key.Subject})
		default:
			return fmt.Errorf("SIWC refresh failed: HTTP %d", status)
		}
	} else {
		updated, err = applySIWCTokens(updated, token)
	}
	if err != nil {
		return err
	}
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer persistCancel()
	if err := model.SaveChannelCredentialWithLease(persistCtx, channelID, channel.Key, string(updated), lease, owner); err != nil {
		return errors.New("cannot persist SIWC credential replacement")
	}
	// Reload routing/settings that an administrator may have edited during the HTTP call.
	channel, err = model.GetChannelById(channelID, true)
	if err != nil {
		return errors.New("SIWC credentials were saved but channel cache reload failed")
	}
	model.CacheUpdateChannel(channel)
	if status != http.StatusOK {
		// Without a token every request fails before reaching upstream; stop routing until an administrator re-imports it.
		DisableChannel(*types.NewChannelError(channel.Id, channel.Type, channel.Name, false, "", channel.GetAutoBan()), "SIWC authorization requires a new sign-in")
	}
	model.RecordAuditLog(nil, model.AuditLog{Username: "system", ActorRole: common.RoleRootUser, AuthMethod: "system",
		Category: model.AuditCategorySecurity, Action: "channel.oauth_refresh", Status: status, Success: status == http.StatusOK,
		Content: fmt.Sprintf("SIWC credential refresh for channel %d", channelID)})
	return nil
}

// One registration/account has one channel owner; duplicated rotating sessions
// would otherwise leave one channel holding an invalidated refresh token.
func ValidateSIWCChannelSession(channel *model.Channel) error {
	if channel.Type != constant.ChannelTypeOpenAI {
		return nil
	}
	credential, err := common.ParseSIWCCredential(channel.Key)
	if err != nil || credential == nil {
		return err
	}
	var others []*model.Channel
	if err := model.DB.Select("id", "key").Where("type = ? AND id <> ?", constant.ChannelTypeOpenAI, channel.Id).Find(&others).Error; err != nil {
		return err
	}
	for _, other := range others {
		key, err := common.ParseSIWCCredential(other.Key)
		if err == nil && key != nil && key.SessionKey() == credential.SessionKey() {
			return errors.New("this SIWC session is already managed by another channel")
		}
	}
	return nil
}

type siwcTokenResponse struct {
	AccessToken       string            `json:"access_token"`
	RefreshToken      string            `json:"refresh_token"`
	ExpiresIn         int64             `json:"expires_in"`
	EarliestRefreshAt int64             `json:"earliest_refresh_at"`
	IDToken           string            `json:"id_token"`
	Scope             string            `json:"scope"`
	TokenType         string            `json:"token_type"`
	Error             common.RawMessage `json:"error"`
}

// errorCode reads both the RFC 6749 string form and OpenAI's {"code": ...} object form.
func (token *siwcTokenResponse) errorCode() string {
	code := ""
	if common.Unmarshal(token.Error, &code) == nil {
		return code
	}
	var oauthError struct {
		Code string `json:"code"`
	}
	_ = common.Unmarshal(token.Error, &oauthError)
	return oauthError.Code
}

// postSIWCToken sends one form-encoded grant to the SIWC token endpoint and decodes the reply.
func postSIWCToken(ctx context.Context, proxy string, form url.Values) (int, *siwcTokenResponse, error) {
	client, err := getCodexOAuthHTTPClient(proxy)
	if err != nil {
		return 0, nil, errors.New("SIWC proxy configuration is invalid")
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, siwcTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, errors.New("cannot create SIWC token request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, errors.New("SIWC token request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return 0, nil, errors.New("invalid SIWC token response")
	}
	var token siwcTokenResponse
	if common.Unmarshal(body, &token) != nil {
		return 0, nil, errors.New("invalid SIWC token response")
	}
	return response.StatusCode, &token, nil
}

// applySIWCTokens writes a successful token response into a credential JSON object.
// A refresh has already spent the old refresh token, so an unexpected expiry must not discard the new pair.
func applySIWCTokens(credential []byte, token *siwcTokenResponse) ([]byte, error) {
	if strings.TrimSpace(token.AccessToken) == "" || strings.TrimSpace(token.RefreshToken) == "" {
		return nil, errors.New("SIWC token response is missing required fields")
	}
	// 0 lets ParseSIWCCredential fall back to the access token's exp claim; long lifetimes are capped.
	expiresAt := int64(0)
	if token.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + min(token.ExpiresIn, siwcMaxExpiresIn)
	}
	fields := map[string]any{"access_token": token.AccessToken, "refresh_token": token.RefreshToken,
		"expires_at": expiresAt, "earliest_refresh_at": max(token.EarliestRefreshAt, 0)}
	if token.IDToken != "" {
		fields["id_token"] = token.IDToken
	}
	if token.Scope != "" {
		fields["scope"] = token.Scope
	}
	if token.TokenType != "" {
		fields["token_type"] = token.TokenType
	}
	credential, err := setSIWCFields(credential, fields)
	if err != nil {
		return nil, err
	}
	if stored, _ := common.ParseSIWCCredential(string(credential)); stored == nil {
		// Neither expires_in nor an exp claim: refresh on the next tick instead of storing an unusable key.
		return setSIWCFields(credential, map[string]any{"expires_at": time.Now().Unix()})
	}
	return credential, nil
}

func setSIWCFields(credential []byte, fields map[string]any) ([]byte, error) {
	var err error
	for field, value := range fields {
		if credential, err = sjson.SetBytes(credential, field, value); err != nil {
			return nil, errors.New("cannot encode SIWC credential")
		}
	}
	return credential, nil
}
