package relay

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func upstreamPricingFixture(t *testing.T, requested string) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	db := setupRelayChannelDB(t)
	previousLogDB := model.LOG_DB
	model.LOG_DB = db
	previousRedis, previousBatch, previousLog := common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled
	previousCount, previousSensitive := constant.CountToken, setting.CheckSensitiveEnabled
	previousExport := common.DataExportEnabled
	common.DataExportEnabled = false
	common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = false, false, true
	constant.CountToken, setting.CheckSensitiveEnabled = true, false
	previousConfig := config.GlobalConfig.ExportAllConfigs()
	t.Cleanup(func() {
		model.LOG_DB = previousLogDB
		common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = previousRedis, previousBatch, previousLog
		constant.CountToken, setting.CheckSensitiveEnabled = previousCount, previousSensitive
		common.DataExportEnabled = previousExport
		require.NoError(t, config.GlobalConfig.LoadFromDB(previousConfig))
	})
	for _, prices := range []struct {
		get    func() string
		set    func(string) error
		values string
	}{
		{ratio_setting.ModelRatio2JSONString, ratio_setting.UpdateModelRatioByJSONString, `{"pricing-requested":1,"pricing-upstream":4,"pricing-free":0}`},
		{ratio_setting.ModelPrice2JSONString, ratio_setting.UpdateModelPriceByJSONString, `{"pricing-fixed":0.01}`},
		{ratio_setting.CompletionRatio2JSONString, ratio_setting.UpdateCompletionRatioByJSONString, `{"pricing-requested":1,"pricing-upstream":3}`},
		{ratio_setting.CacheRatio2JSONString, ratio_setting.UpdateCacheRatioByJSONString, `{"pricing-requested":1,"pricing-upstream":0.25}`},
		{ratio_setting.CreateCacheRatio2JSONString, ratio_setting.UpdateCreateCacheRatioByJSONString, `{"pricing-requested":1,"pricing-upstream":1.5}`},
	} {
		previous := prices.get()
		t.Cleanup(func() { require.NoError(t, prices.set(previous)) })
		require.NoError(t, prices.set(prices.values))
	}
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode":    `{"pricing-tiered":"tiered_expr"}`,
		"billing_setting.billing_expr":    `{"pricing-tiered":"param(\"fast\") == true ? tier(\"fast\", p * 8 + c * 24 + cr * 2 + cc * 12) : tier(\"normal\", p)"}`,
		"group_ratio_setting.group_ratio": `{"default":1,"free":0,"expensive":2}`,
	}))
	*operation_setting.GetQuotaSetting() = operation_setting.QuotaSetting{PreConsumeMultiplier: 1}
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}))
	user := model.User{Username: "upstream-pricing", Role: common.RoleCommonUser, Quota: 2_000_000, Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "upstream-pricing", RemainQuota: 2_000_000, Status: common.TokenStatusEnabled}
	require.NoError(t, db.Create(&token).Error)
	channel := model.Channel{Name: "upstream-pricing", Key: "unused", Type: constant.ChannelTypeOpenAI}
	require.NoError(t, db.Create(&channel).Error)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("token_quota", 2_000_000)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, requested)
	common.SetContextKey(c, constant.ContextKeyChannelId, channel.Id)
	common.SetContextKey(c, constant.ContextKeyChannelType, channel.Type)
	info := &relaycommon.RelayInfo{
		OriginModelName: requested, UserId: user.Id, TokenId: token.Id, TokenKey: token.Key,
		UsingGroup: "default", UserGroup: "default", StartTime: time.Now(), FirstResponseTime: time.Now(),
		UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}, RelayFormat: types.RelayFormatOpenAI,
		Request:             &dto.GeneralOpenAIRequest{Model: requested, Messages: []dto.Message{{Role: "user", Content: strings.Repeat("hello ", 100)}}},
		BillingRequestInput: &billingexpr.RequestInput{Body: []byte(`{"fast":true}`)},
	}
	return c, info
}

func TestSelectedChannelPricingAndSettlement(t *testing.T) {
	for _, tc := range []struct {
		name, requested, upstream, billing, source string
		fallback                                   bool
		wantQuota                                  int
	}{
		{"ratio", "pricing-requested", "pricing-upstream", "pricing-upstream", "upstream", false, 5000},
		{"tiered expression", "pricing-requested", "pricing-tiered", "pricing-tiered", "upstream", false, 5000},
		{"fixed model price", "pricing-requested", "pricing-fixed", "pricing-fixed", "upstream", false, 5000},
		{"unpriced upstream", "pricing-requested", "pricing-missing", "pricing-requested", "requested", true, 1100},
		{"unpriced request", "pricing-missing", "pricing-upstream", "pricing-upstream", "upstream", false, 5000},
		{"unpriced request to tiered", "pricing-missing", "pricing-tiered", "pricing-tiered", "upstream", false, 5000},
		{"unpriced request to fixed", "pricing-missing", "pricing-fixed", "pricing-fixed", "upstream", false, 5000},
		{"unmapped", "pricing-requested", "", "pricing-requested", "requested", false, 1100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, info := upstreamPricingFixture(t, tc.requested)
			require.Nil(t, PrepareRequestBilling(c, info))
			estimate, meta := info.GetEstimatePromptTokens(), info.BillingTokenCountMeta
			require.Positive(t, estimate)
			initialReservation := info.FinalPreConsumedQuota
			assert.Nil(t, info.Billing, "funding waits for selected-channel pricing")
			if tc.upstream != "" {
				mapping, err := common.Marshal(map[string]string{tc.requested: tc.upstream})
				require.NoError(t, err)
				c.Set("model_mapping", string(mapping))
			}
			require.Nil(t, PrepareSelectedChannelBilling(c, info))
			assert.Equal(t, tc.billing, info.GetBillingModelName())
			assert.Same(t, meta, info.BillingTokenCountMeta)
			assert.Equal(t, estimate, info.GetEstimatePromptTokens())
			wantEstimate := estimate
			switch tc.billing {
			case "pricing-upstream", "pricing-tiered":
				wantEstimate *= 4
			case "pricing-fixed":
				wantEstimate = 5000
			}
			assert.Equal(t, wantEstimate, info.PriceData.QuotaToPreConsume)
			assert.Equal(t, max(initialReservation, wantEstimate), info.FinalPreConsumedQuota)
			if tc.billing == "pricing-tiered" {
				require.NotNil(t, info.TieredBillingSnapshot)
				assert.Equal(t, tc.billing, info.TieredBillingSnapshot.ModelName)
				assert.Equal(t, "fast", info.TieredBillingSnapshot.EstimatedTier)
			}
			// Handlers initialize channel metadata again; routing must agree.
			info.InitChannelMeta(c)
			outbound := &dto.GeneralOpenAIRequest{}
			require.NoError(t, helper.ModelMappedHelper(c, info, outbound))
			assert.Equal(t, info.UpstreamModelName, outbound.Model)
			service.PostTextConsumeQuota(c, info, &dto.Usage{
				PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100,
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100, CachedCreationTokens: 50},
			}, nil)
			var log model.Log
			require.NoError(t, model.LOG_DB.First(&log).Error)
			assert.Equal(t, tc.wantQuota, log.Quota)
			assert.Equal(t, tc.requested, log.ModelName)
			var other map[string]any
			require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
			admin, ok := other["admin_info"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tc.billing, admin["pricing_model"])
			assert.Equal(t, tc.source, admin["pricing_source"])
			if tc.fallback {
				assert.Equal(t, true, admin["pricing_fallback"])
			} else {
				assert.NotContains(t, admin, "pricing_fallback")
			}
			var user model.User
			require.NoError(t, model.DB.First(&user, info.UserId).Error)
			assert.Equal(t, 2_000_000-tc.wantQuota, user.Quota)
			var token model.Token
			require.NoError(t, model.DB.First(&token, info.TokenId).Error)
			assert.Equal(t, 2_000_000-tc.wantQuota, token.RemainQuota)
		})
	}
}

func TestSelectedChannelMissingPricePreservesError(t *testing.T) {
	c, info := upstreamPricingFixture(t, "pricing-missing")
	_, original := helper.ModelPriceHelper(c, info, 100, &types.TokenCountMeta{})
	require.Error(t, original)
	require.Nil(t, PrepareRequestBilling(c, info))
	c.Set("model_mapping", `{"pricing-missing":"also-missing"}`)
	apiErr := PrepareSelectedChannelBilling(c, info)
	require.NotNil(t, apiErr)
	assert.Equal(t, original.Error(), apiErr.Error())
	assert.Equal(t, types.ErrorCodeModelPriceError, apiErr.GetErrorCode())
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.True(t, types.IsSkipRetryError(apiErr))
	assert.Nil(t, info.Billing)
}

func TestSelectedChannelRetryReservationAndRefund(t *testing.T) {
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previousTransport := client.Transport
	t.Cleanup(func() { client.Transport = previousTransport })
	for _, tc := range []struct {
		name                                               string
		image, trusted, tokenFailure, insufficient, settle bool
	}{
		{name: "wallet cannot afford retry", insufficient: true},
		{name: "trusted wallet cannot afford retry", trusted: true, insufficient: true},
		{name: "trusted limited token cannot afford retry", trusted: true, tokenFailure: true},
		{name: "wallet retry refund"},
		{name: "trusted retry refund", trusted: true},
		{name: "wallet increase rolled back on token failure", tokenFailure: true},
		{name: "image retry refund", image: true},
		{name: "image increase rolled back on token failure", image: true, tokenFailure: true},
		{name: "image retry settlement", image: true, settle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, info := upstreamPricingFixture(t, "pricing-requested")
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://policy.invalid")
			info.RequestURLPath = "/v1/chat/completions"
			info.RelayMode = relayconstant.RelayModeChatCompletions
			handler := TextHelper
			transport := &policyTestTransport{}
			client.Transport = transport
			walletQuota, tokenQuota := 2_000_000, 2_000_000
			if tc.insufficient {
				walletQuota = 2000
			}
			if tc.tokenFailure {
				tokenQuota = 1000
			}
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", info.UserId).Update("quota", walletQuota).Error)
			require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", info.TokenId).Update("remain_quota", tokenQuota).Error)
			c.Set("token_quota", tokenQuota)
			if tc.trusted {
				operation_setting.GetQuotaSetting().TrustQuotaUSD = 0.001 // 500 quota
			}
			if tc.image {
				handler = ImageHelper
				info.RequestURLPath = "/v1/images/generations"
				info.RelayMode, info.RelayFormat = relayconstant.RelayModeImagesGenerations, types.RelayFormatOpenAIImage
				info.Request = &dto.ImageRequest{Model: info.OriginModelName, N: common.GetPointer(uint(1))}
				require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"pricing-requested":0.001,"pricing-fixed":0.01}`))
			}
			require.Nil(t, PrepareRequestBilling(c, info))
			require.Nil(t, PrepareSelectedChannelBilling(c, info))
			session, initial := info.Billing, info.FinalPreConsumedQuota
			require.NotNil(t, session)
			if tc.trusted {
				assert.Zero(t, initial)
			} else if tc.image {
				assert.Equal(t, 500, initial)
			}
			require.NotNil(t, handler(c, info)) // The in-memory upstream rejects the first attempt.
			require.Equal(t, 1, transport.calls)
			c.Set("model_mapping", `{"pricing-requested":"pricing-fixed"}`)
			apiErr := PrepareSelectedChannelBilling(c, info)
			assert.Same(t, session, info.Billing)
			if tc.insufficient || tc.tokenFailure {
				require.NotNil(t, apiErr)
				wantCode := types.ErrorCodeInsufficientUserQuota
				if tc.tokenFailure {
					wantCode = types.ErrorCodePreConsumeTokenQuotaFailed
				}
				assert.Equal(t, wantCode, apiErr.GetErrorCode())
				assert.Equal(t, http.StatusForbidden, apiErr.StatusCode)
				assert.True(t, types.IsSkipRetryError(apiErr))
				assert.Equal(t, initial, info.FinalPreConsumedQuota)
				assert.Equal(t, 1, transport.calls, "unaffordable retries never reach upstream")
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, 5000, info.FinalPreConsumedQuota)
				require.NotNil(t, handler(c, info))
				assert.Equal(t, 2, transport.calls)
			}
			var user model.User
			var token model.Token
			require.NoError(t, model.DB.First(&user, info.UserId).Error)
			require.NoError(t, model.DB.First(&token, info.TokenId).Error)
			assert.Equal(t, walletQuota-info.FinalPreConsumedQuota, user.Quota, "a rejected token increase rolls back funding")
			assert.Equal(t, tokenQuota-info.FinalPreConsumedQuota, token.RemainQuota)
			actual := 0
			if tc.settle {
				actual = 3000
				require.NoError(t, session.Settle(actual))
			}
			session.Refund(c)
			session.Refund(c)
			require.Eventually(t, func() bool {
				var refunded model.Token
				return model.DB.First(&refunded, info.TokenId).Error == nil && refunded.RemainQuota == tokenQuota-actual
			}, time.Second, time.Millisecond)
			require.NoError(t, model.DB.First(&user, info.UserId).Error)
			assert.Equal(t, walletQuota-actual, user.Quota)
			assert.False(t, session.NeedsRefund())
		})
	}
}

func TestSelectedChannelLogAndStatisticsKeepRequestedBillingAlias(t *testing.T) {
	c, info := upstreamPricingFixture(t, "pricing-requested@temperature:0.7")
	model.CacheQuotaDataLock.Lock()
	previousCache := model.CacheQuotaData
	model.CacheQuotaData = make(map[string]*model.QuotaData)
	model.CacheQuotaDataLock.Unlock()
	common.DataExportEnabled = true
	t.Cleanup(func() {
		model.CacheQuotaDataLock.Lock()
		model.CacheQuotaData = previousCache
		model.CacheQuotaDataLock.Unlock()
	})
	c.Set("model_mapping", `{"pricing-requested@temperature:0.7":"pricing-upstream"}`)
	require.Nil(t, PrepareRequestBilling(c, info))
	require.Nil(t, PrepareSelectedChannelBilling(c, info))
	service.PostTextConsumeQuota(c, info, &dto.Usage{PromptTokens: 1000, TotalTokens: 1000}, nil)
	var log model.Log
	require.NoError(t, model.LOG_DB.First(&log).Error)
	assert.Equal(t, "pricing-requested", log.ModelName)
	assert.Equal(t, 4000, log.Quota)
	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
	admin, ok := other["admin_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "pricing-upstream", admin["pricing_model"])
	assert.Equal(t, "upstream", admin["pricing_source"])
	assert.Equal(t, "pricing-requested", admin["billing_model"])
	model.CacheQuotaDataLock.Lock()
	defer model.CacheQuotaDataLock.Unlock()
	require.Len(t, model.CacheQuotaData, 1)
	for _, data := range model.CacheQuotaData {
		assert.Equal(t, "pricing-requested", data.ModelName)
		assert.Equal(t, log.Quota, data.Quota)
	}
}

func TestSelectedChannelRetryRefreshesModelModeAndGroup(t *testing.T) {
	c, info := upstreamPricingFixture(t, "pricing-requested")
	require.Nil(t, PrepareRequestBilling(c, info))
	estimate := info.GetEstimatePromptTokens()
	for _, tc := range []struct {
		mapping, group, billing string
		want, held              int
		tiered, fallback        bool
	}{
		{`{"pricing-requested":"pricing-tiered"}`, "default", "pricing-tiered", estimate * 4, estimate * 4, true, false},
		{`{"pricing-requested":"pricing-upstream"}`, "expensive", "pricing-upstream", estimate * 8, estimate * 8, false, false},
		{`{}`, "default", "pricing-requested", estimate, estimate * 8, false, false},
		{`{"pricing-requested":"missing"}`, "default", "pricing-requested", estimate, estimate * 8, false, true},
	} {
		c.Set("model_mapping", tc.mapping)
		c.Set("auto_group", tc.group)
		require.Nil(t, PrepareSelectedChannelBilling(c, info))
		assert.Equal(t, tc.billing, info.GetBillingModelName())
		assert.Equal(t, tc.want, info.PriceData.QuotaToPreConsume)
		assert.Equal(t, tc.held, info.FinalPreConsumedQuota)
		assert.Equal(t, tc.tiered, info.TieredBillingSnapshot != nil)
		assert.Equal(t, tc.fallback, info.PricingFallback)
	}
	info.InitChannelMeta(c)
	require.NoError(t, helper.ModelMappedHelper(c, info, nil))
	service.PostTextConsumeQuota(c, info, &dto.Usage{PromptTokens: 1000, TotalTokens: 1000}, nil)
	var user model.User
	require.NoError(t, model.DB.First(&user, info.UserId).Error)
	assert.Equal(t, 2_000_000-1000, user.Quota, "settle the last attempt and refund excess reservations")
}

func TestSelectedChannelFreeAndTrustedReservations(t *testing.T) {
	for _, tc := range []struct {
		name, requested, upstream, initialGroup, selectedGroup string
		trusted, playground, freePreConsume                    bool
		wantFree                                               bool
	}{
		{name: "free model to paid", requested: "pricing-free", upstream: "pricing-upstream", initialGroup: "default", selectedGroup: "default"},
		{name: "paid model to free", requested: "pricing-requested", upstream: "pricing-free", initialGroup: "default", selectedGroup: "default", wantFree: true},
		{name: "free group to paid playground", requested: "pricing-requested", upstream: "pricing-tiered", initialGroup: "free", selectedGroup: "default", playground: true},
		{name: "paid to free group", requested: "pricing-requested", upstream: "pricing-tiered", initialGroup: "default", selectedGroup: "free", wantFree: true},
		{name: "free stays free", requested: "pricing-free", upstream: "pricing-free", initialGroup: "default", selectedGroup: "default", wantFree: true},
		{name: "free preconsume enabled", requested: "pricing-free", upstream: "pricing-free", initialGroup: "default", selectedGroup: "default", freePreConsume: true},
		{name: "trusted wallet", requested: "pricing-requested", upstream: "pricing-upstream", initialGroup: "default", selectedGroup: "default", trusted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, info := upstreamPricingFixture(t, tc.requested)
			info.IsPlayground = tc.playground
			operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = tc.freePreConsume
			if tc.trusted {
				operation_setting.GetQuotaSetting().TrustQuotaUSD = 1
			}
			c.Set("auto_group", tc.initialGroup)
			require.Nil(t, PrepareRequestBilling(c, info))
			c.Set("auto_group", tc.selectedGroup)
			mapping, err := common.Marshal(map[string]string{tc.requested: tc.upstream})
			require.NoError(t, err)
			c.Set("model_mapping", string(mapping))
			require.Nil(t, PrepareSelectedChannelBilling(c, info))
			assert.Equal(t, tc.wantFree, info.PriceData.FreeModel)
			if tc.wantFree {
				assert.Nil(t, info.Billing)
				return
			}
			require.NotNil(t, info.Billing)
			if tc.trusted {
				assert.Zero(t, info.FinalPreConsumedQuota)
			} else if tc.upstream == "pricing-upstream" || tc.selectedGroup != "free" && tc.upstream == "pricing-tiered" {
				assert.Equal(t, info.PriceData.QuotaToPreConsume, info.FinalPreConsumedQuota)
			}
		})
	}
}

func TestSelectedChannelAuthorizesEffectiveReservation(t *testing.T) {
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previousTransport := client.Transport
	t.Cleanup(func() { client.Transport = previousTransport })
	for _, tc := range []struct {
		name, requested, upstream string
		quota                     int
		trusted, insufficient     bool
	}{
		{"unaffordable mapped price", "pricing-requested", "pricing-fixed", 2000, false, true},
		{"trust cannot admit unaffordable mapped price", "pricing-requested", "pricing-fixed", 2000, true, true},
		{"expensive request maps to affordable price", "pricing-fixed", "pricing-requested", 1000, false, false},
		{"expensive request maps to free price", "pricing-fixed", "pricing-free", 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, info := upstreamPricingFixture(t, tc.requested)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", info.UserId).Update("quota", tc.quota).Error)
			if tc.trusted {
				operation_setting.GetQuotaSetting().TrustQuotaUSD = 0.001 // 500 quota
			}
			transport := &policyTestTransport{}
			client.Transport = transport
			require.Nil(t, PrepareRequestBilling(c, info))
			assert.Nil(t, info.Billing)
			mapping, err := common.Marshal(map[string]string{tc.requested: tc.upstream})
			require.NoError(t, err)
			c.Set("model_mapping", string(mapping))
			apiErr := PrepareSelectedChannelBilling(c, info)
			if apiErr == nil {
				common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://policy.invalid")
				info.RequestURLPath = "/v1/chat/completions"
				require.NotNil(t, TextHelper(c, info)) // In-memory transport stops at the upstream error.
			}
			var user model.User
			require.NoError(t, model.DB.First(&user, info.UserId).Error)
			if tc.insufficient {
				require.NotNil(t, apiErr)
				assert.Equal(t, types.ErrorCodeInsufficientUserQuota, apiErr.GetErrorCode())
				assert.Equal(t, http.StatusForbidden, apiErr.StatusCode)
				assert.True(t, types.IsSkipRetryError(apiErr))
				assert.Empty(t, transport.path, "unaffordable work never reaches upstream")
				assert.Nil(t, info.Billing)
				assert.Equal(t, tc.quota, user.Quota)
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, "/v1/chat/completions", transport.path)
				assert.Equal(t, tc.quota-info.FinalPreConsumedQuota, user.Quota)
				if info.Billing != nil {
					require.NoError(t, info.Billing.Settle(0))
				}
			}
			var token model.Token
			require.NoError(t, model.DB.First(&token, info.TokenId).Error)
			assert.Equal(t, 2_000_000, token.RemainQuota)
		})
	}
}

func TestSelectedChannelToolSurchargeUsesPricingModel(t *testing.T) {
	c, info := upstreamPricingFixture(t, "pricing-requested")
	operation_setting.SetToolPriceForTest("mapped_tool:pricing-upstream", 4)
	t.Cleanup(func() { operation_setting.DeleteToolPriceForTest("mapped_tool:pricing-upstream") })
	require.Nil(t, PrepareRequestBilling(c, info))
	c.Set("model_mapping", `{"pricing-requested":"pricing-upstream"}`)
	require.Nil(t, PrepareSelectedChannelBilling(c, info))
	info.InitChannelMeta(c)
	require.NoError(t, helper.ModelMappedHelper(c, info, nil))
	info.CountBillableToolCall(dto.BuildInCallFunctionCall, "mapped_tool")
	require.NotNil(t, info.ResponsesUsageInfo)
	require.NotNil(t, info.BuiltInTools["mapped_tool"])
	assert.Equal(t, 1, info.BuiltInTools["mapped_tool"].CallCount)
	service.PostTextConsumeQuota(c, info, &dto.Usage{PromptTokens: 1000, TotalTokens: 1000}, nil)
	var log model.Log
	require.NoError(t, model.LOG_DB.First(&log).Error)
	assert.Equal(t, 6000, log.Quota, "4000 input quota plus 2000 for the upstream-priced tool")
}

func TestSelectedChannelPreservesImagePricingMetadata(t *testing.T) {
	c, info := upstreamPricingFixture(t, "pricing-requested")
	info.Request = &dto.ImageRequest{Model: info.OriginModelName, N: common.GetPointer(uint(3))}
	require.Nil(t, PrepareRequestBilling(c, info))
	assert.Equal(t, float64(3), info.BillingTokenCountMeta.BillingRatios["n"])
	c.Set("model_mapping", `{"pricing-requested":"pricing-fixed"}`)
	require.Nil(t, PrepareSelectedChannelBilling(c, info))
	assert.Equal(t, 15_000, info.PriceData.QuotaToPreConsume)
	assert.Equal(t, 15_000, info.FinalPreConsumedQuota)
}

func TestSelectedChannelResponsesWebSocketPayload(t *testing.T) {
	c, info := upstreamPricingFixture(t, "pricing-requested")
	request := dto.OpenAIResponsesRequest{Model: info.OriginModelName, Input: []byte(`"hello"`)}
	info.Request, info.RelayFormat = &request, types.RelayFormatOpenAIResponses
	require.Nil(t, PrepareRequestBilling(c, info))
	c.Set("model_mapping", `{"pricing-requested":"pricing-upstream"}`)
	require.Nil(t, PrepareSelectedChannelBilling(c, info))
	payload, apiErr := buildResponsesWSCreatePayload(c, info, request, nil, "")
	require.Nil(t, apiErr)
	var body map[string]any
	require.NoError(t, common.Unmarshal(payload, &body))
	assert.Equal(t, "pricing-upstream", body["model"])
	assert.Equal(t, "pricing-upstream", info.GetBillingModelName())
	assert.Equal(t, 4*info.GetEstimatePromptTokens(), info.FinalPreConsumedQuota)
}

func TestSelectedChannelRealtimeSettlementUsesPricingModel(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeAzure} {
		t.Run(constant.GetChannelTypeName(channelType), func(t *testing.T) {
			c, info := upstreamPricingFixture(t, "pricing-requested")
			common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://policy.invalid")
			info.RelayMode, info.RelayFormat = relayconstant.RelayModeRealtime, types.RelayFormatOpenAIRealtime
			info.RequestURLPath = "/v1/realtime?model=pricing-requested&voice=alloy"
			require.Nil(t, PrepareRequestBilling(c, info))
			c.Set("model_mapping", `{"pricing-requested":"pricing-upstream"}`)
			require.Nil(t, PrepareSelectedChannelBilling(c, info))
			adaptor, apiErr := prepareRealtimeRequest(c, info)
			require.Nil(t, apiErr)
			requestURL, err := adaptor.GetRequestURL(info)
			require.NoError(t, err)
			parsed, err := url.Parse(requestURL)
			require.NoError(t, err)
			if channelType == constant.ChannelTypeAzure {
				assert.Equal(t, "pricing-upstream", parsed.Query().Get("deployment"))
			} else {
				assert.Equal(t, "pricing-upstream", parsed.Query().Get("model"))
				assert.Equal(t, "alloy", parsed.Query().Get("voice"))
			}
			assert.Equal(t, info.GetBillingModelName(), info.UpstreamModelName)
			service.PostWssConsumeQuota(c, info, info.OriginModelName, &dto.RealtimeUsage{
				InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100,
				InputTokenDetails:  dto.InputTokenDetails{TextTokens: 1000},
				OutputTokenDetails: dto.OutputTokenDetails{TextTokens: 100},
			}, nil)
			var log model.Log
			require.NoError(t, model.LOG_DB.First(&log).Error)
			assert.Equal(t, "pricing-requested", log.ModelName)
			assert.Equal(t, 5200, log.Quota, "upstream input ratio 4 and completion ratio 3")
		})
	}
}

func TestSelectedChannelRealtimeRejectsUnsupportedModelControls(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeAzure} {
		for _, suffix := range []string{"@temperature:0.7", "@topp:0.9", "@effort:high", "@thinking:on", "@unknown:1"} {
			t.Run(constant.GetChannelTypeName(channelType)+suffix, func(t *testing.T) {
				c, info := upstreamPricingFixture(t, "pricing-requested")
				common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
				common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://policy.invalid")
				info.RelayMode, info.RelayFormat = relayconstant.RelayModeRealtime, types.RelayFormatOpenAIRealtime
				info.RequestURLPath = "/v1/realtime?model=pricing-requested&voice=alloy"
				mapping, err := common.Marshal(map[string]string{info.OriginModelName: "pricing-upstream" + suffix})
				require.NoError(t, err)
				c.Set("model_mapping", string(mapping))
				adaptor, apiErr := prepareRealtimeRequest(c, info)
				require.NotNil(t, apiErr)
				assert.Nil(t, adaptor, "reject controls before a transport can dial")
				assert.Equal(t, types.ErrorCodeConvertRequestFailed, apiErr.GetErrorCode())
				assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
				assert.True(t, types.IsSkipRetryError(apiErr))
			})
		}
	}
}

func TestSelectedChannelRealtimePreservesExemptModelNames(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeAzure} {
		t.Run(constant.GetChannelTypeName(channelType), func(t *testing.T) {
			c, info := upstreamPricingFixture(t, "pricing-requested")
			common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://policy.invalid")
			info.RelayMode, info.RelayFormat = relayconstant.RelayModeRealtime, types.RelayFormatOpenAIRealtime
			info.RequestURLPath = "/v1/realtime?model=pricing-requested&voice=alloy"
			const upstream = "opaque@temperature:0.7"
			c.Set("channel_create_time", int64(constant.AzureNoRemoveDotTime+1))
			model_setting.GetGlobalSettings().ThinkingModelBlacklist = []string{upstream}
			c.Set("model_mapping", `{"pricing-requested":"opaque@temperature:0.7"}`)
			adaptor, apiErr := prepareRealtimeRequest(c, info)
			require.Nil(t, apiErr)
			requestURL, err := adaptor.GetRequestURL(info)
			require.NoError(t, err)
			parsed, err := url.Parse(requestURL)
			require.NoError(t, err)
			key := "model"
			if channelType == constant.ChannelTypeAzure {
				key = "deployment"
			}
			assert.Equal(t, upstream, parsed.Query().Get(key))
		})
	}
}

func TestSelectedChannelPassthroughPricesOutboundModel(t *testing.T) {
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previousTransport := client.Transport
	t.Cleanup(func() { client.Transport = previousTransport })
	for _, tc := range []struct {
		name, path  string
		format      types.RelayFormat
		channelType int
		request     dto.Request
		handler     func(*gin.Context, *relaycommon.RelayInfo) *types.NewAPIError
		urlModel    bool
	}{
		{"chat", "/v1/chat/completions", types.RelayFormatOpenAI, constant.ChannelTypeOpenAI, &dto.GeneralOpenAIRequest{Model: "pricing-requested"}, TextHelper, false},
		{"Claude", "/v1/messages", types.RelayFormatClaude, constant.ChannelTypeAnthropic, &dto.ClaudeRequest{Model: "pricing-requested"}, ClaudeHelper, false},
		{"Responses", "/v1/responses", types.RelayFormatOpenAIResponses, constant.ChannelTypeOpenAI, &dto.OpenAIResponsesRequest{Model: "pricing-requested"}, ResponsesHelper, false},
		{"compact", "/v1/responses/compact", types.RelayFormatOpenAIResponses, constant.ChannelTypeOpenAI, &dto.OpenAIResponsesCompactionRequest{Model: "pricing-requested", Input: []byte(`"hello"`)}, ResponsesHelper, false},
		{"image", "/v1/images/generations", types.RelayFormatOpenAIImage, constant.ChannelTypeOpenAI, &dto.ImageRequest{Model: "pricing-requested"}, ImageHelper, false},
		{"rerank", "/v1/rerank", types.RelayFormatRerank, constant.ChannelTypeJina, &dto.RerankRequest{Model: "pricing-requested"}, RerankHelper, false},
		{"Gemini URL", "/v1beta/models/pricing-requested:generateContent", types.RelayFormatGemini, constant.ChannelTypeGemini, &dto.GeminiChatRequest{}, GeminiHelper, true},
		{"Azure deployment", "/v1/chat/completions", types.RelayFormatOpenAI, constant.ChannelTypeAzure, &dto.GeneralOpenAIRequest{Model: "pricing-requested"}, TextHelper, true},
		{"Cloudflare chat", "/v1/chat/completions", types.RelayFormatOpenAI, constant.ChannelCloudflare, &dto.GeneralOpenAIRequest{Model: "pricing-requested"}, TextHelper, false},
		{"Cloudflare Responses", "/v1/responses", types.RelayFormatOpenAIResponses, constant.ChannelCloudflare, &dto.OpenAIResponsesRequest{Model: "pricing-requested"}, ResponsesHelper, false},
		{"Cloudflare run", "/v1/completions", types.RelayFormatOpenAI, constant.ChannelCloudflare, &dto.GeneralOpenAIRequest{Model: "pricing-requested"}, TextHelper, true},
		{"advanced route body", "/v1/chat/completions", types.RelayFormatOpenAI, constant.ChannelTypeAdvancedCustom, &dto.GeneralOpenAIRequest{Model: "pricing-requested"}, TextHelper, false},
		{"advanced route URL", "/v1/chat/completions", types.RelayFormatOpenAI, constant.ChannelTypeAdvancedCustom, &dto.GeneralOpenAIRequest{Model: "pricing-requested"}, TextHelper, true},
	} {
		for _, global := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/channel", true: "/global"}[global], func(t *testing.T) {
				c, info := upstreamPricingFixture(t, "pricing-requested")
				model_setting.GetGlobalSettings().PassThroughRequestEnabled = global
				body := `{"model":"pricing-requested","n":1}`
				if tc.path == "/v1/responses/compact" {
					body = `{"model":"pricing-requested","input":"hello"}`
				}
				c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				info.Request, info.RelayFormat, info.RequestURLPath = tc.request, tc.format, tc.path
				info.RelayMode = relayconstant.Path2RelayMode(tc.path)
				common.SetContextKey(c, constant.ContextKeyChannelType, tc.channelType)
				common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://policy.invalid")
				common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: !global && tc.channelType != constant.ChannelTypeAdvancedCustom})
				if tc.channelType == constant.ChannelTypeAdvancedCustom {
					path := "/v1/chat/completions"
					if tc.urlModel {
						path = "/models/{model}"
					}
					common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: tc.path, UpstreamPath: path, PassThroughBodyEnabled: !global}}}})
				}
				require.Nil(t, PrepareRequestBilling(c, info))
				c.Set("model_mapping", `{"pricing-requested":"pricing-upstream"}`)
				require.Nil(t, PrepareSelectedChannelBilling(c, info))
				wantModel, wantSource := "pricing-requested", "requested"
				if tc.urlModel {
					wantModel, wantSource = "pricing-upstream", "upstream"
				}
				assert.Equal(t, wantModel, info.GetBillingModelName())
				assert.Equal(t, wantSource, info.PricingSource)
				assert.False(t, info.PricingFallback)
				wantReservation := info.GetEstimatePromptTokens()
				if tc.urlModel {
					wantReservation *= 4
				}
				assert.Equal(t, wantReservation, info.FinalPreConsumedQuota)
				if tc.path == "/v1/responses/compact" {
					assert.Positive(t, wantReservation)
				}
				transport := &policyTestTransport{}
				client.Transport = transport
				require.NotNil(t, tc.handler(c, info))
				require.NotEmpty(t, transport.path)
				assert.JSONEq(t, body, string(transport.body))
				if tc.urlModel {
					assert.Contains(t, transport.path, "pricing-upstream")
				}
				service.PostTextConsumeQuota(c, info, &dto.Usage{PromptTokens: 1000, TotalTokens: 1000}, nil)
				var log model.Log
				require.NoError(t, model.LOG_DB.First(&log).Error)
				wantQuota := 1000
				if tc.urlModel {
					wantQuota = 4000
				}
				assert.Equal(t, wantQuota, log.Quota)
			})
		}
	}
}

func TestSelectedChannelAudioSettlementUsesPricingModel(t *testing.T) {
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	oldTransport := client.Transport
	t.Cleanup(func() { client.Transport = oldTransport })
	for _, route := range []string{"chat", "chat through Responses", "Responses"} {
		for _, tc := range []struct {
			name, requested, upstream string
			quota                     int
		}{
			{"mapped audio", "pricing-requested", "gpt-4o-audio-priced", 11200},
			{"unmapped annotated", "pricing-requested@temperature:0.7", "", 1100},
			{"requested fallback", "pricing-requested@temperature:0.7", "missing", 1100},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				c, info := upstreamPricingFixture(t, tc.requested)
				oldInput, oldOutput := ratio_setting.AudioRatio2JSONString(), ratio_setting.AudioCompletionRatio2JSONString()
				t.Cleanup(func() {
					require.NoError(t, ratio_setting.UpdateAudioRatioByJSONString(oldInput))
					require.NoError(t, ratio_setting.UpdateAudioCompletionRatioByJSONString(oldOutput))
				})
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"pricing-requested":1,"gpt-4o-audio-priced":4}`))
				require.NoError(t, ratio_setting.UpdateCompletionRatioByJSONString(`{"pricing-requested":1,"gpt-4o-audio-priced":1}`))
				require.NoError(t, ratio_setting.UpdateAudioRatioByJSONString(`{"pricing-requested":10,"gpt-4o-audio-priced":10}`))
				require.NoError(t, ratio_setting.UpdateAudioCompletionRatioByJSONString(`{"pricing-requested":2,"gpt-4o-audio-priced":2}`))
				common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://policy.invalid")
				info.RequestURLPath, info.RelayMode = "/v1/chat/completions", relayconstant.RelayModeChatCompletions
				transport := &policyTestTransport{response: `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100,"prompt_tokens_details":{"audio_tokens":200,"text_tokens":800}}}`}
				handler := TextHelper
				if route != "chat" {
					transport.response = `{"id":"resp-test","object":"response","status":"completed","output":[],"usage":{"input_tokens":1000,"output_tokens":100,"total_tokens":1100,"input_tokens_details":{"audio_tokens":200,"text_tokens":800}}}`
					if route == "Responses" {
						handler = ResponsesHelper
						info.RequestURLPath, info.RelayMode, info.RelayFormat = "/v1/responses", relayconstant.RelayModeResponses, types.RelayFormatOpenAIResponses
						info.Request = &dto.OpenAIResponsesRequest{Model: tc.requested}
					} else {
						model_setting.GetGlobalSettings().ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{Enabled: true, AllChannels: true, ModelPatterns: []string{".*"}}
					}
				}
				client.Transport = transport
				if tc.upstream != "" {
					mapping, err := common.Marshal(map[string]string{tc.requested: tc.upstream})
					require.NoError(t, err)
					c.Set("model_mapping", string(mapping))
				}
				require.Nil(t, PrepareRequestBilling(c, info))
				require.Nil(t, PrepareSelectedChannelBilling(c, info))
				require.Nil(t, handler(c, info))
				var log model.Log
				require.NoError(t, model.LOG_DB.First(&log).Error)
				assert.Equal(t, tc.quota, log.Quota)
				wantPath := "/v1/chat/completions"
				if route != "chat" {
					wantPath = "/v1/responses"
				}
				assert.Equal(t, wantPath, transport.path)
			})
		}
	}
}
