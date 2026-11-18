package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// officialRatioPresetURL is the file the admin "official preset" sync reads
// (web/src/features/system-settings/models/constants.ts). A var for tests.
var officialRatioPresetURL = officialRatioPresetBaseURL + "/llm-metadata/api/newapi/ratio_config-v1-base.json"

var pricingFieldOptionKeys = map[string]string{
	"model_ratio":                    "ModelRatio",
	"completion_ratio":               "CompletionRatio",
	"cache_ratio":                    "CacheRatio",
	"create_cache_ratio":             "CreateCacheRatio",
	"image_ratio":                    "ImageRatio",
	"audio_ratio":                    "AudioRatio",
	"audio_completion_ratio":         "AudioCompletionRatio",
	"model_price":                    "ModelPrice",
	billing_setting.BillingModeField: "billing_setting.billing_mode",
	billing_setting.BillingExprField: "billing_setting.billing_expr",
}

// pricingFillHandler gives enabled models that have no price the official
// preset's price, so new models stop billing at the self-use fallback ratio
// until an admin syncs by hand. Models that already have a price are never
// touched.
type pricingFillHandler struct{}

const systemTaskTypePricingFill = "pricing_fill"

func (pricingFillHandler) Type() string { return systemTaskTypePricingFill }

func (pricingFillHandler) Enabled() bool {
	return common.GetEnvOrDefaultBool("PRICING_FILL_TASK_ENABLED", true)
}

func (pricingFillHandler) Interval() time.Duration { return 30 * time.Minute }

func (pricingFillHandler) NewPayload() any { return nil }

func (pricingFillHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	summary, err := runPricingFill(ctx)
	status := model.SystemTaskStatusSucceeded
	if err != nil {
		status = model.SystemTaskStatusFailed
	}
	finishSystemTaskHandler(task, runnerID, status, summary, err)
}

// modelHasPrice mirrors the relay's lookup order (relay/helper/price.go):
// tiered expression, fixed price, then ratio. The self-use fallback does not count.
func modelHasPrice(name string) bool {
	if billing_setting.GetBillingMode(name) == billing_setting.BillingModeTieredExpr {
		return true
	}
	if _, ok := ratio_setting.GetModelPrice(name, false); ok {
		return true
	}
	return ratio_setting.HasConfiguredModelRatio(name)
}

type pricingFillSummary struct {
	Missing     []string          `json:"missing"`
	Filled      []string          `json:"filled,omitempty"`
	NotInPreset []string          `json:"not_in_preset,omitempty"`
	Failed      map[string]string `json:"failed,omitempty"`
}

func runPricingFill(ctx context.Context) (*pricingFillSummary, error) {
	summary := &pricingFillSummary{Missing: []string{}}
	for _, name := range model.GetEnabledModels() {
		if !modelHasPrice(name) {
			summary.Missing = append(summary.Missing, name)
		}
	}
	if len(summary.Missing) == 0 {
		return summary, nil
	}
	sort.Strings(summary.Missing)
	preset, err := fetchOfficialRatioPreset(ctx)
	if err != nil {
		return summary, err
	}
	snapshot, err := model.GetModelPricingSnapshot(summary.Missing)
	if err != nil {
		return summary, err
	}
	for _, entry := range snapshot.Entries {
		values := modelPricingSyncValues(preset, entry.ModelName)
		if len(values) == 0 {
			summary.NotInPreset = append(summary.NotInPreset, entry.ModelName)
			continue
		}
		// Keep whatever the model already has (e.g. a cache ratio) and add the preset's price.
		pricing := model.PricingValues{}
		for key, value := range entry.Configured {
			pricing[key] = value
		}
		for field, value := range values {
			pricing[pricingFieldOptionKeys[field]] = value
		}
		// ExpectedVersion makes a concurrent admin edit win over this fill.
		change := model.ModelPricingChange{ModelName: entry.ModelName, ExpectedVersion: entry.Version, Pricing: pricing}
		if err := model.UpdateModelPricing([]model.ModelPricingChange{change}); err != nil {
			if summary.Failed == nil {
				summary.Failed = map[string]string{}
			}
			summary.Failed[entry.ModelName] = err.Error()
			common.SysError(common.LogText("pricing fill: %s: %v", entry.ModelName, err))
			continue
		}
		summary.Filled = append(summary.Filled, entry.ModelName)
		common.SysLog(common.LogText("pricing fill: %s priced from %s: %v", entry.ModelName, officialRatioPresetName, values))
	}
	if len(summary.NotInPreset) > 0 {
		common.SysLog(common.LogText("pricing fill: no preset price, still on fallback ratio: %v", summary.NotInPreset))
	}
	return summary, nil
}

// fetchOfficialRatioPreset returns the preset normalized the way the admin
// sync compares it (effectivePricingSyncData).
func fetchOfficialRatioPreset(ctx context.Context) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, officialRatioPresetURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("official ratio preset: %s", resp.Status)
	}
	var body struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
		Message string         `json:"message"`
	}
	if err := common.DecodeJson(io.LimitReader(resp.Body, maxRatioConfigBytes), &body); err != nil {
		return nil, err
	}
	if !body.Success || len(body.Data) == 0 {
		return nil, fmt.Errorf("official ratio preset: empty or unsuccessful (%s)", body.Message)
	}
	return effectivePricingSyncData(body.Data), nil
}
