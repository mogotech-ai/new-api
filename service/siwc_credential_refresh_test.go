package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSIWCCredentialImportValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"malformed", `{"client_id":"secret-do-not-log",`},
		{"empty OAuth fields", `{"client_id":"","access_token":"","refresh_token":""}`},
		{"missing refresh token", `{"client_id":"issued","access_token":"secret-do-not-log","expires_at":1800000000}`},
		{"placeholder client", `{"client_id":"dynamic_agent_client","access_token":"access","refresh_token":"refresh","expires_at":1800000000}`},
		{"Codex client", `{"client_id":"app_EMoamEEZ73f0CkXaXp7hrann","access_token":"access","refresh_token":"refresh","expires_at":1800000000}`},
		{"missing expiry", `{"client_id":"issued","access_token":"access","refresh_token":"refresh"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, err := common.ParseSIWCCredential(tc.raw)
			require.Error(t, err)
			assert.Nil(t, key)
			assert.NotContains(t, err.Error(), "secret-do-not-log")
		})
	}
	ordinary, err := common.ParseSIWCCredential("ordinary-api-key")
	require.NoError(t, err)
	assert.Nil(t, ordinary)
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1800000000,"sub":"account-subject"}`))
	raw, err := common.Marshal(map[string]any{"client_id": "issued", "access_token": "header." + claims + ".signature", "refresh_token": "refresh"})
	require.NoError(t, err)
	key, err := common.ParseSIWCCredential(string(raw))
	require.NoError(t, err)
	assert.EqualValues(t, 1800000000, key.ExpiresAt)
	assert.Equal(t, "issued\x00account-subject", key.SessionKey())
	key, err = common.ParseSIWCCredential(`{"client_id":"issued","access_token":"discarded","refresh_token":"discarded","reauth_required":true,"siwc_subject":"account-subject"}`)
	require.NoError(t, err)
	assert.Empty(t, key.AccessToken)
	assert.Empty(t, key.RefreshToken)
	assert.Equal(t, "issued\x00account-subject", key.SessionKey())
}

type siwcCredentialTransport func(*http.Request) (*http.Response, error)

func (f siwcCredentialTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func setupSIWCCredentialTest(t *testing.T) *http.Client {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SystemTaskLock{}, &model.AuditLog{}))
	previousDB, previousLog, previousCache := model.DB, model.LOG_DB, common.MemoryCacheEnabled
	previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	model.DB, model.LOG_DB, common.MemoryCacheEnabled = db, db, true
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	if GetHttpClient() == nil {
		InitHttpClient()
	}
	client := GetHttpClient()
	previousTransport := client.Transport
	client.Transport = siwcCredentialTransport(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected token request")
		return nil, errors.New("unexpected token request")
	})
	t.Cleanup(func() {
		client.Transport = previousTransport
		model.DB, model.LOG_DB, common.MemoryCacheEnabled = previousDB, previousLog, previousCache
		common.SetDatabaseTypes(previousMainType, previousLogType)
		require.NoError(t, sqlDB.Close())
	})
	return client
}

func createSIWCTestChannel(t *testing.T, id int) *model.Channel {
	t.Helper()
	key, err := common.Marshal(map[string]any{"client_id": fmt.Sprintf("issued-%d", id), "access_token": "old-access", "refresh_token": "old-refresh",
		"expires_at": time.Now().Add(time.Minute).Unix(), "ext_agent_host_id": "retained-host"})
	require.NoError(t, err)
	channel := &model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Name: "SIWC test", Key: string(key), Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(channel).Error)
	return channel
}

func TestSIWCCredentialsRefreshAllChannelsAndPreserveAPIKeys(t *testing.T) {
	client := setupSIWCCredentialTest(t)
	for _, id := range []int{17, 42} {
		createSIWCTestChannel(t, id)
	}
	ordinary := &model.Channel{Id: 99, Type: constant.ChannelTypeOpenAI, Key: "ordinary-api-key", Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(ordinary).Error)
	model.InitChannelCache()
	var calls atomic.Int32
	client.Transport = siwcCredentialTransport(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		assert.Equal(t, siwcTokenURL, request.URL.String())
		require.NoError(t, request.ParseForm())
		assert.Equal(t, "refresh_token", request.Form.Get("grant_type"))
		assert.Equal(t, "https://api.openai.com/v1", request.Form.Get("resource"))
		clientID := request.Form.Get("client_id")
		assert.Contains(t, []string{"issued-17", "issued-42"}, clientID)
		body, err := common.Marshal(map[string]any{"access_token": "new-access-" + clientID, "refresh_token": "new-refresh-" + clientID, "expires_in": 3600})
		require.NoError(t, err)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})
	RefreshSIWCCredentials(context.Background())
	RefreshSIWCCredentials(context.Background())
	assert.EqualValues(t, 2, calls.Load())
	for _, id := range []int{17, 42} {
		channel, err := model.CacheGetChannel(id)
		require.NoError(t, err)
		key, err := common.ParseSIWCCredential(channel.Key)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("new-refresh-issued-%d", id), key.RefreshToken)
		assert.Contains(t, channel.Key, `"ext_agent_host_id":"retained-host"`)
	}
	channel, err := model.GetChannelById(99, true)
	require.NoError(t, err)
	assert.Equal(t, "ordinary-api-key", channel.Key)
}

func TestSIWCCredentialWritesExcludeSecretsFromDebugSQL(t *testing.T) {
	setupSIWCCredentialTest(t)
	var logs bytes.Buffer
	model.DB = model.DB.Session(&gorm.Session{Logger: logger.New(log.New(&logs, "", 0), logger.Config{LogLevel: logger.Info})}).Debug()
	key := `{"client_id":"issued","access_token":"private-access","refresh_token":"private-refresh","expires_at":1800000000}`
	channel := model.Channel{Id: 42, Type: constant.ChannelTypeOpenAI, Name: "test", Key: key, Models: "model", Group: "default"}
	require.NoError(t, model.BatchInsertChannels([]model.Channel{channel}))
	channel.Name = "updated"
	require.NoError(t, channel.Update())
	channel.Id = 43
	require.NoError(t, channel.Insert())
	lease, acquired, err := model.AcquireOAuthCredentialLease("session", "owner", time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, model.SaveChannelCredentialWithLease(context.Background(), 42, key, strings.ReplaceAll(key, "private-", "rotated-private-"), lease, "owner"))
	assert.NotContains(t, logs.String(), "private-access")
	assert.NotContains(t, logs.String(), "private-refresh")
}

func TestSIWCCredentialRefreshFailureRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		reauth bool
	}{
		{"temporary", 503, `{"error":"unavailable"}`, false},
		{"malformed", 200, `{"access_token":"incomplete"}`, false},
		{"invalid client", 400, `{"error":"invalid_client"}`, false},
		{"revoked", 400, `{"error":"invalid_grant"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := setupSIWCCredentialTest(t)
			channel := createSIWCTestChannel(t, 42)
			client.Transport = siwcCredentialTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})
			err := RefreshSIWCChannelCredential(context.Background(), 42)
			if !tc.reauth {
				require.Error(t, err)
			}
			stored, err := model.GetChannelById(42, true)
			require.NoError(t, err)
			if tc.reauth {
				key, err := common.ParseSIWCCredential(stored.Key)
				require.NoError(t, err)
				assert.True(t, key.ReauthRequired)
				assert.Empty(t, key.AccessToken)
				assert.Empty(t, key.RefreshToken)
				assert.Equal(t, "issued-42", key.ClientID)
			} else {
				assert.Equal(t, channel.Key, stored.Key)
			}
		})
	}
}

func TestSIWCRefreshLeaseSerializesOneSession(t *testing.T) {
	client := setupSIWCCredentialTest(t)
	createSIWCTestChannel(t, 42)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	client.Transport = siwcCredentialTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		close(entered)
		<-release
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)), Header: make(http.Header)}, nil
	})
	done := make(chan error, 1)
	go func() { done <- RefreshSIWCChannelCredential(context.Background(), 42); close(done) }()
	t.Cleanup(func() { once.Do(func() { close(release) }); <-done })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach token endpoint")
	}
	require.NoError(t, RefreshSIWCChannelCredential(context.Background(), 42))
	assert.EqualValues(t, 1, calls.Load())
	once.Do(func() { close(release) })
	require.NoError(t, <-done)
}

func TestSIWCDuplicateSessionCannotRotateTwoChannelCopies(t *testing.T) {
	setupSIWCCredentialTest(t)
	first := createSIWCTestChannel(t, 17)
	second := *first
	second.Id = 42
	require.NoError(t, model.DB.Create(&second).Error)
	require.ErrorContains(t, ValidateSIWCChannelSession(&second), "already managed")
	require.ErrorContains(t, RefreshSIWCChannelCredential(context.Background(), 17), "already managed")
}
