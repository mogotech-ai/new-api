package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
)

// First-time registration per https://developers.openai.com/siwc/token-sharing-open-source/sign-in.
// SIWC only accepts a 127.0.0.1 loopback redirect, which a server cannot receive, so the
// administrator pastes the URL the browser lands on back into the console.
const (
	siwcAuthorizeURL  = "https://auth.openai.com/api/accounts/authorize"
	siwcRedirectURI   = "http://127.0.0.1:1455/auth/callback"
	siwcLoginScope    = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	siwcLoginLifetime = 10 * time.Minute
)

type siwcLoginFlow struct {
	Verifier string `json:"verifier"`
	HostID   string `json:"host_id"`
}

// StartSIWCLogin returns the authorization URL for a new SIWC registration. The OAuth state is an
// AuthFlow token: stored hashed, single use, bound to the starting administrator, valid 10 minutes.
func StartSIWCLogin(userID int) (string, error) {
	secret := make([]byte, 48)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	// Each sign-in registers its own agent, so its host ID is stored with that credential.
	flow := siwcLoginFlow{Verifier: base64.RawURLEncoding.EncodeToString(secret), HostID: "urn:uuid:" + uuid.NewString()}
	payload, err := common.Marshal(flow)
	if err != nil {
		return "", err
	}
	state, _, err := model.CreateAuthFlow(model.AuthFlowCreate{Purpose: model.AuthFlowPurposeSIWCLogin, UserId: userID,
		Payload: string(payload), ExpiresAt: time.Now().Add(siwcLoginLifetime)})
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(flow.Verifier))
	query := url.Values{
		"client_id":             {"dynamic_agent_client"},
		"agent_name_hint":       {"New API"},
		"ext_agent_host_id":     {flow.HostID},
		"response_type":         {"code"},
		"redirect_uri":          {siwcRedirectURI},
		"scope":                 {siwcLoginScope},
		"resource":              {siwcResource},
		"state":                 {state},
		"nonce":                 {rand.Text()},
		"code_challenge_method": {"S256"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
	}
	return siwcAuthorizeURL + "?" + strings.ReplaceAll(query.Encode(), "+", "%20"), nil
}

// CompleteSIWCLogin exchanges the pasted callback URL for the credential JSON an OpenAI channel imports.
// The ID token is not consumed: identity comes from the access token, and PKCE binds the code to this flow.
func CompleteSIWCLogin(ctx context.Context, userID int, callbackURL string) (string, error) {
	callback, err := url.Parse(strings.TrimSpace(callbackURL))
	if err != nil {
		return "", errors.New("invalid SIWC callback URL")
	}
	query := callback.Query()
	flow, err := model.ConsumeAuthFlow(query.Get("state"), model.AuthFlowMatch{Purpose: model.AuthFlowPurposeSIWCLogin, UserId: userID})
	if err != nil {
		return "", errors.New("SIWC sign-in expired or was already used; start again")
	}
	if query.Get("error") != "" {
		return "", errors.New("SIWC authorization was not granted")
	}
	clientID := query.Get("client_id")
	if query.Get("code") == "" || clientID == "" || clientID == "dynamic_agent_client" {
		return "", errors.New("SIWC callback is missing the code or issued client_id")
	}
	var pending siwcLoginFlow
	if err := common.UnmarshalJsonStr(flow.Payload, &pending); err != nil {
		return "", err
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, token, err := postSIWCToken(exchangeCtx, "", url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID},
		"code": {query.Get("code")}, "code_verifier": {pending.Verifier}, "redirect_uri": {siwcRedirectURI}, "resource": {siwcResource}})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("SIWC code exchange failed: HTTP %d %s", status, token.errorCode())
	}
	granted := token.Scope
	if granted == "" {
		granted = query.Get("scope")
	}
	if !slices.Contains(strings.Fields(granted), "chatgpt.tokens.use.direct") {
		return "", errors.New("SIWC sign-in did not grant ChatGPT plan usage")
	}
	base, err := common.Marshal(map[string]string{"client_id": clientID, "ext_agent_host_id": pending.HostID})
	if err != nil {
		return "", err
	}
	credential, err := applySIWCTokens(base, token)
	if err != nil {
		return "", err
	}
	if parsed, err := common.ParseSIWCCredential(string(credential)); err != nil || parsed == nil {
		return "", errors.New("SIWC sign-in returned an unusable credential")
	}
	return string(credential), nil
}
