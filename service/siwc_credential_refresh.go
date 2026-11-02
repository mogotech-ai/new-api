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
	client, err := getCodexOAuthHTTPClient(channel.GetSetting().Proxy)
	if err != nil {
		return errors.New("SIWC refresh proxy configuration is invalid")
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {key.ClientID}, "refresh_token": {key.RefreshToken}, "resource": {"https://api.openai.com/v1"}}
	request, err := http.NewRequestWithContext(refreshCtx, http.MethodPost, siwcTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("cannot create SIWC refresh request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("SIWC token refresh request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return errors.New("invalid SIWC token response")
	}
	var replacement struct {
		AccessToken       string            `json:"access_token"`
		RefreshToken      string            `json:"refresh_token"`
		ExpiresIn         int64             `json:"expires_in"`
		EarliestRefreshAt int64             `json:"earliest_refresh_at"`
		IDToken           string            `json:"id_token"`
		Scope             string            `json:"scope"`
		TokenType         string            `json:"token_type"`
		Error             common.RawMessage `json:"error"`
	}
	if common.Unmarshal(body, &replacement) != nil {
		return errors.New("invalid SIWC token response")
	}
	updates := map[string]any{}
	if response.StatusCode != http.StatusOK {
		code := ""
		if common.Unmarshal(replacement.Error, &code) != nil {
			var oauthError struct {
				Code string `json:"code"`
			}
			_ = common.Unmarshal(replacement.Error, &oauthError)
			code = oauthError.Code
		}
		switch code {
		case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
			updates = map[string]any{"access_token": "", "refresh_token": "", "id_token": "", "reauth_required": true, "siwc_subject": key.Subject}
		default:
			return fmt.Errorf("SIWC refresh failed: HTTP %d", response.StatusCode)
		}
	} else {
		if strings.TrimSpace(replacement.AccessToken) == "" || strings.TrimSpace(replacement.RefreshToken) == "" {
			return errors.New("SIWC token response is missing required fields")
		}
		// The old refresh token is already spent, so an unexpected expiry must not discard the new pair.
		// 0 lets ParseSIWCCredential fall back to the access token's exp claim; long lifetimes are capped.
		expiresAt := int64(0)
		if replacement.ExpiresIn > 0 {
			expiresAt = time.Now().Unix() + min(replacement.ExpiresIn, siwcMaxExpiresIn)
		}
		updates = map[string]any{"access_token": replacement.AccessToken, "refresh_token": replacement.RefreshToken,
			"expires_at": expiresAt, "earliest_refresh_at": max(replacement.EarliestRefreshAt, 0)}
		if replacement.IDToken != "" {
			updates["id_token"] = replacement.IDToken
		}
		if replacement.Scope != "" {
			updates["scope"] = replacement.Scope
		}
		if replacement.TokenType != "" {
			updates["token_type"] = replacement.TokenType
		}
	}
	updated := []byte(channel.Key)
	for field, value := range updates {
		updated, err = sjson.SetBytes(updated, field, value)
		if err != nil {
			return errors.New("cannot encode SIWC credential replacement")
		}
	}
	if stored, _ := common.ParseSIWCCredential(string(updated)); stored == nil {
		// Neither expires_in nor an exp claim: refresh on the next tick instead of storing an unusable key.
		if updated, err = sjson.SetBytes(updated, "expires_at", time.Now().Unix()); err != nil {
			return errors.New("cannot encode SIWC credential replacement")
		}
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
	if response.StatusCode != http.StatusOK {
		// Without a token every request fails before reaching upstream; stop routing until an administrator re-imports it.
		DisableChannel(*types.NewChannelError(channel.Id, channel.Type, channel.Name, false, "", channel.GetAutoBan()), "SIWC authorization requires a new sign-in")
	}
	model.RecordAuditLog(nil, model.AuditLog{Username: "system", ActorRole: common.RoleRootUser, AuthMethod: "system",
		Category: model.AuditCategorySecurity, Action: "channel.oauth_refresh", Status: response.StatusCode, Success: response.StatusCode == http.StatusOK,
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
