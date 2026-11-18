package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A model added to a channel without a price bills at the self-use fallback
// ratio. The fill task must price it from the official preset, and must never
// change a model that already has a price.
func TestPricingFillOnlyPricesUnpricedEnabledModels(t *testing.T) {
	db := modelManagementDB(t, "sqlite", "")
	const presetExpr = `tier("standard", p * 2 + c * 10)`
	body, err := common.Marshal(map[string]any{"success": true, "data": map[string]any{
		"billing_mode": map[string]string{"acme-new": "tiered_expr", "acme-priced": "tiered_expr", "acme-disabled": "tiered_expr"},
		"billing_expr": map[string]string{"acme-new": presetExpr, "acme-priced": presetExpr, "acme-disabled": presetExpr},
	}})
	require.NoError(t, err)
	preset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(preset.Close)
	previousURL := officialRatioPresetURL
	officialRatioPresetURL = preset.URL
	t.Cleanup(func() { officialRatioPresetURL = previousURL })

	for _, ability := range []model.Ability{
		{Group: "default", Model: "acme-new", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "acme-priced", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "acme-unknown", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "acme-disabled", ChannelId: 2, Enabled: false},
	} {
		require.NoError(t, db.Create(&ability).Error)
	}
	snapshot, err := model.GetModelPricingSnapshot([]string{"acme-priced"})
	require.NoError(t, err)
	require.NoError(t, model.UpdateModelPricing([]model.ModelPricingChange{{
		ModelName: "acme-priced", ExpectedVersion: snapshot.Entries[0].Version,
		Pricing: model.PricingValues{"ModelRatio": 1.5},
	}}))

	summary, err := runPricingFill(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"acme-new", "acme-unknown"}, summary.Missing)
	assert.Equal(t, []string{"acme-new"}, summary.Filled)
	assert.Equal(t, []string{"acme-unknown"}, summary.NotInPreset)
	assert.Empty(t, summary.Failed)

	expr, ok := billing_setting.GetBillingExpr("acme-new")
	assert.True(t, ok)
	assert.Equal(t, presetExpr, expr)
	assert.Equal(t, billing_setting.BillingModeRatio, billing_setting.GetBillingMode("acme-priced"))
	ratio, ok, _ := ratio_setting.GetModelRatio("acme-priced")
	assert.True(t, ok)
	assert.Equal(t, 1.5, ratio)
	assert.False(t, modelHasPrice("acme-disabled"), "disabled abilities are not filled")

	again, err := runPricingFill(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"acme-unknown"}, again.Missing)
	assert.Empty(t, again.Filled)
}
