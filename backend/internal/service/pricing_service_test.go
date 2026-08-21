package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestPricingSchedulerBlankRemoteURLDoesNotStart(t *testing.T) {
	svc := NewPricingService(&config.Config{Pricing: config.PricingConfig{RemoteURL: "  \t  "}}, nil)
	defer svc.Stop()

	svc.startUpdateScheduler()
	done := make(chan struct{})
	go func() {
		svc.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("blank remote URL must not start scheduler")
	}
}

func TestPricingNonEmptyInvalidRemoteURLStillReturnsValidationError(t *testing.T) {
	svc := NewPricingService(&config.Config{Pricing: config.PricingConfig{
		RemoteURL: "://invalid",
		DataDir:   t.TempDir(),
	}}, nil)

	err := svc.ForceUpdate()

	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid pricing url")
}

func TestParsePricingData_ParsesPriorityAndServiceTierFields(t *testing.T) {
	svc := &PricingService{}
	body := []byte(`{
		"gpt-5.4": {
			"input_cost_per_token": 0.0000025,
			"input_cost_per_token_priority": 0.000005,
			"output_cost_per_token": 0.000015,
			"output_cost_per_token_priority": 0.00003,
			"cache_creation_input_token_cost": 0.0000025,
			"cache_creation_input_token_cost_priority": 0.000005,
			"cache_read_input_token_cost": 0.00000025,
			"cache_read_input_token_cost_priority": 0.0000005,
			"long_context_input_token_threshold": 272000,
			"long_context_input_cost_multiplier": 2,
			"long_context_output_cost_multiplier": 1.5,
			"supports_service_tier": true,
			"supports_prompt_caching": true,
			"litellm_provider": "openai",
			"mode": "chat"
		}
	}`)

	data, err := svc.parsePricingData(body)
	require.NoError(t, err)
	pricing := data["gpt-5.4"]
	require.NotNil(t, pricing)
	require.InDelta(t, 5e-6, pricing.InputCostPerTokenPriority, 1e-12)
	require.InDelta(t, 3e-5, pricing.OutputCostPerTokenPriority, 1e-12)
	require.InDelta(t, 5e-6, pricing.CacheCreationInputTokenCostPriority, 1e-12)
	require.InDelta(t, 5e-7, pricing.CacheReadInputTokenCostPriority, 1e-12)
	require.Equal(t, 272000, pricing.LongContextInputTokenThreshold)
	require.InDelta(t, 2.0, pricing.LongContextInputCostMultiplier, 1e-12)
	require.InDelta(t, 1.5, pricing.LongContextOutputCostMultiplier, 1e-12)
	require.True(t, pricing.SupportsServiceTier)
}

func TestBillingService_GPT56CacheWritePricingUsesOfficialMultiplier(t *testing.T) {
	tests := []struct {
		model             string
		input             float64
		inputPriority     float64
		output            float64
		outputPriority    float64
		cacheRead         float64
		cacheReadPriority float64
	}{
		{model: "gpt-5.6-sol", input: 5e-6, inputPriority: 10e-6, output: 30e-6, outputPriority: 60e-6, cacheRead: 0.5e-6, cacheReadPriority: 1e-6},
		{model: "gpt-5.6-terra", input: 2e-6, inputPriority: 4e-6, output: 12e-6, outputPriority: 24e-6, cacheRead: 0.2e-6, cacheReadPriority: 0.4e-6},
		{model: "gpt-5.6-luna", input: 0.2e-6, inputPriority: 0.4e-6, output: 1.2e-6, outputPriority: 2.4e-6, cacheRead: 0.02e-6, cacheReadPriority: 0.04e-6},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			pricingSvc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
				tt.model: {
					InputCostPerToken:               tt.input,
					InputCostPerTokenPriority:       tt.inputPriority,
					OutputCostPerToken:              tt.output,
					OutputCostPerTokenPriority:      tt.outputPriority,
					CacheReadInputTokenCost:         tt.cacheRead,
					CacheReadInputTokenCostPriority: tt.cacheReadPriority,
				},
			}}
			svc := NewBillingService(&config.Config{}, pricingSvc)

			pricing, err := svc.GetModelPricing(tt.model)
			require.NoError(t, err)
			require.InDelta(t, tt.input*1.25, pricing.CacheCreationPricePerToken, 1e-12)
			require.InDelta(t, tt.inputPriority*1.25, pricing.CacheCreationPricePerTokenPriority, 1e-12)
			require.Equal(t, 272000, pricing.LongContextInputThreshold)
			require.InDelta(t, 2.0, pricing.LongContextInputMultiplier, 1e-12)
			require.InDelta(t, 1.5, pricing.LongContextOutputMultiplier, 1e-12)

			tokens := UsageTokens{InputTokens: 700, OutputTokens: 50, CacheCreationTokens: 200, CacheReadTokens: 100}
			standard, err := svc.CalculateCostWithServiceTier(tt.model, tokens, 1, "")
			require.NoError(t, err)
			require.InDelta(t, 200*tt.input*1.25, standard.CacheCreationCost, 1e-12)

			priority, err := svc.CalculateCostWithServiceTier(tt.model, tokens, 1, "priority")
			require.NoError(t, err)
			require.InDelta(t, 200*tt.inputPriority*1.25, priority.CacheCreationCost, 1e-12)

			flex, err := svc.CalculateCostWithServiceTier(tt.model, tokens, 1, "flex")
			require.NoError(t, err)
			require.InDelta(t, 200*tt.input*1.25*0.5, flex.CacheCreationCost, 1e-12)
		})
	}
}

func TestBillingService_GPT56UsesLongContextPricingAcrossModelsAndTiers(t *testing.T) {
	models := []struct {
		name               string
		input, cached      float64
		cacheWrite, output float64
	}{
		{name: "gpt-5.6-sol", input: 5e-6, cached: 0.5e-6, cacheWrite: 6.25e-6, output: 30e-6},
		{name: "gpt-5.6-terra", input: 2e-6, cached: 0.2e-6, cacheWrite: 2.5e-6, output: 12e-6},
		{name: "gpt-5.6-luna", input: 0.2e-6, cached: 0.02e-6, cacheWrite: 0.25e-6, output: 1.2e-6},
	}
	tiers := []struct {
		name       string
		priceScale float64
	}{
		{name: "standard", priceScale: 1},
		{name: "priority", priceScale: 2},
		{name: "flex", priceScale: 0.5},
	}
	tokens := UsageTokens{
		InputTokens:         100000,
		CacheCreationTokens: 100000,
		CacheReadTokens:     73000,
		OutputTokens:        10,
	}

	for _, model := range models {
		for _, tier := range tiers {
			t.Run(model.name+"/"+tier.name, func(t *testing.T) {
				svc := NewBillingService(&config.Config{}, nil)
				serviceTier := ""
				if tier.name != "standard" {
					serviceTier = tier.name
				}
				cost, err := svc.CalculateCostWithServiceTier(model.name, tokens, 1, serviceTier)
				require.NoError(t, err)
				require.InDelta(t, float64(tokens.InputTokens)*model.input*tier.priceScale*2, cost.InputCost, 1e-12)
				require.InDelta(t, float64(tokens.CacheCreationTokens)*model.cacheWrite*tier.priceScale*2, cost.CacheCreationCost, 1e-12)
				require.InDelta(t, float64(tokens.CacheReadTokens)*model.cached*tier.priceScale*2, cost.CacheReadCost, 1e-12)
				require.InDelta(t, float64(tokens.OutputTokens)*model.output*tier.priceScale*1.5, cost.OutputCost, 1e-12)
			})
		}
	}
}

func TestBillingService_GPT56LongContextBoundaryIsExclusive(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	tokens := UsageTokens{InputTokens: 100000, CacheCreationTokens: 100000, CacheReadTokens: 72000, OutputTokens: 10}

	cost, err := svc.CalculateCost("gpt-5.6-sol", tokens, 1)
	require.NoError(t, err)
	require.InDelta(t, 100000*5e-6, cost.InputCost, 1e-12)
	require.InDelta(t, 100000*6.25e-6, cost.CacheCreationCost, 1e-12)
	require.InDelta(t, 72000*0.5e-6, cost.CacheReadCost, 1e-12)
	require.InDelta(t, 10*30e-6, cost.OutputCost, 1e-12)
}

func TestPricingService_BareGPT56AliasDeterministicallyUsesSol(t *testing.T) {
	pricingSvc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"gpt-5.6-sol":   {InputCostPerToken: 5e-6},
		"gpt-5.6-terra": {InputCostPerToken: 2e-6},
		"gpt-5.6-luna":  {InputCostPerToken: 0.2e-6},
		"gpt-5.4":       {InputCostPerToken: 2.5e-6},
	}}

	for i := 0; i < 100; i++ {
		for _, alias := range []string{"gpt-5.6", "openai/gpt-5.6"} {
			pricing := pricingSvc.GetModelPricing(alias)
			require.NotNil(t, pricing)
			require.InDelta(t, 5e-6, pricing.InputCostPerToken, 1e-12, "iteration=%d alias=%s", i, alias)
		}
	}

	billingSvc := NewBillingService(&config.Config{}, pricingSvc)
	for _, alias := range []string{"gpt-5.6", "openai/gpt-5.6"} {
		pricing, err := billingSvc.GetModelPricing(alias)
		require.NoError(t, err)
		require.InDelta(t, 5e-6, pricing.InputPricePerToken, 1e-12)
		require.InDelta(t, 6.25e-6, pricing.CacheCreationPricePerToken, 1e-12)
	}
}

func TestDefaultPricingIncludesOfficialGPT56Rates(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "resources", "model-pricing", "model_prices_and_context_window.json"))
	require.NoError(t, err)

	pricingSvc := &PricingService{}
	pricingData, err := pricingSvc.parsePricingData(data)
	require.NoError(t, err)
	pricingSvc.pricingData = pricingData
	billingSvc := NewBillingService(&config.Config{}, pricingSvc)

	tests := []struct {
		model                                                             string
		input, cached, cacheWrite, output                                 float64
		inputPriority, cachedPriority, cacheWritePriority, outputPriority float64
	}{
		{model: "gpt-5.6-sol", input: 5e-6, cached: 0.5e-6, cacheWrite: 6.25e-6, output: 30e-6, inputPriority: 10e-6, cachedPriority: 1e-6, cacheWritePriority: 12.5e-6, outputPriority: 60e-6},
		{model: "gpt-5.6-terra", input: 2e-6, cached: 0.2e-6, cacheWrite: 2.5e-6, output: 12e-6, inputPriority: 4e-6, cachedPriority: 0.4e-6, cacheWritePriority: 5e-6, outputPriority: 24e-6},
		{model: "gpt-5.6-luna", input: 0.2e-6, cached: 0.02e-6, cacheWrite: 0.25e-6, output: 1.2e-6, inputPriority: 0.4e-6, cachedPriority: 0.04e-6, cacheWritePriority: 0.5e-6, outputPriority: 2.4e-6},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			pricing, err := billingSvc.GetModelPricing(tt.model)
			require.NoError(t, err)
			require.InDelta(t, tt.input, pricing.InputPricePerToken, 1e-12)
			require.InDelta(t, tt.cached, pricing.CacheReadPricePerToken, 1e-12)
			require.InDelta(t, tt.cacheWrite, pricing.CacheCreationPricePerToken, 1e-12)
			require.InDelta(t, tt.output, pricing.OutputPricePerToken, 1e-12)
			require.InDelta(t, tt.inputPriority, pricing.InputPricePerTokenPriority, 1e-12)
			require.InDelta(t, tt.cachedPriority, pricing.CacheReadPricePerTokenPriority, 1e-12)
			require.InDelta(t, tt.cacheWritePriority, pricing.CacheCreationPricePerTokenPriority, 1e-12)
			require.InDelta(t, tt.outputPriority, pricing.OutputPricePerTokenPriority, 1e-12)
			require.Equal(t, 272000, pricing.LongContextInputThreshold)
			require.InDelta(t, 2.0, pricing.LongContextInputMultiplier, 1e-12)
			require.InDelta(t, 1.5, pricing.LongContextOutputMultiplier, 1e-12)
		})
	}
}

func TestGPT56DedicatedFallbacksUseOfficialRates(t *testing.T) {
	tests := []struct {
		model                             string
		input, cached, cacheWrite, output float64
	}{
		{model: "gpt-5.6-sol", input: 5e-6, cached: 0.5e-6, cacheWrite: 6.25e-6, output: 30e-6},
		{model: "gpt-5.6-terra", input: 2e-6, cached: 0.2e-6, cacheWrite: 2.5e-6, output: 12e-6},
		{model: "gpt-5.6-luna", input: 0.2e-6, cached: 0.02e-6, cacheWrite: 0.25e-6, output: 1.2e-6},
	}

	for _, tt := range tests {
		t.Run(tt.model+"/pricing_service", func(t *testing.T) {
			pricingSvc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
				"gpt-5.1-codex": {InputCostPerToken: 1.25e-6},
			}}
			svc := NewBillingService(&config.Config{}, pricingSvc)
			pricing, err := svc.GetModelPricing(tt.model + "-preview")
			require.NoError(t, err)
			assertGPT56FallbackPricing(t, pricing, tt.input, tt.cached, tt.cacheWrite, tt.output)
		})

		t.Run(tt.model+"/billing_service", func(t *testing.T) {
			svc := NewBillingService(&config.Config{}, nil)
			pricing, err := svc.GetModelPricing(tt.model)
			require.NoError(t, err)
			assertGPT56FallbackPricing(t, pricing, tt.input, tt.cached, tt.cacheWrite, tt.output)
		})
	}
}

func assertGPT56FallbackPricing(t *testing.T, pricing *ModelPricing, input, cached, cacheWrite, output float64) {
	t.Helper()
	require.InDelta(t, input, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, cached, pricing.CacheReadPricePerToken, 1e-12)
	require.InDelta(t, cacheWrite, pricing.CacheCreationPricePerToken, 1e-12)
	require.InDelta(t, output, pricing.OutputPricePerToken, 1e-12)
	require.Equal(t, 272000, pricing.LongContextInputThreshold)
	require.InDelta(t, 2.0, pricing.LongContextInputMultiplier, 1e-12)
	require.InDelta(t, 1.5, pricing.LongContextOutputMultiplier, 1e-12)
}

func TestParsePricingData_KeepsImageOnlyPricing(t *testing.T) {
	svc := &PricingService{}
	body := []byte(`{
		"image-only-model": {
			"output_cost_per_image": 0.034,
			"litellm_provider": "vertex_ai-language-models",
			"mode": "image_generation"
		}
	}`)

	data, err := svc.parsePricingData(body)
	require.NoError(t, err)
	pricing := data["image-only-model"]
	require.NotNil(t, pricing)
	require.InDelta(t, 0.034, pricing.OutputCostPerImage, 1e-12)
	require.Equal(t, "image_generation", pricing.Mode)
	// 仅有图片价的条目必须标记 token 价缺失，供 token 计费路径 fail-closed。
	require.True(t, pricing.TokenPricingAbsent)
}

func TestBillingService_GetModelPricing_FailsClosedForImageOnlyEntries(t *testing.T) {
	pricingSvc := &PricingService{}
	data, err := pricingSvc.parsePricingData([]byte(`{
		"imagen-9.0-generate": {
			"output_cost_per_image": 0.04,
			"litellm_provider": "vertex_ai-image-models",
			"mode": "image_generation"
		},
		"gemini-image-with-token-price": {
			"input_cost_per_token": 0.0,
			"output_cost_per_token": 0.0,
			"output_cost_per_image": 0.034,
			"litellm_provider": "vertex_ai-language-models",
			"mode": "image_generation"
		}
	}`))
	require.NoError(t, err)
	pricingSvc.pricingData = data
	billingSvc := NewBillingService(&config.Config{}, pricingSvc)

	// image-only 条目不得进入 token 计费（否则 token 流量按 $0 计费），
	// 必须落到 fallback / ErrModelPricingUnavailable 的 fail-closed 路径。
	_, err = billingSvc.GetModelPricing("imagen-9.0-generate")
	require.ErrorIs(t, err, ErrModelPricingUnavailable)

	// 显式 0 token 价的免费条目保持历史行为：正常返回。
	pricing, err := billingSvc.GetModelPricing("gemini-image-with-token-price")
	require.NoError(t, err)
	require.Zero(t, pricing.InputPricePerToken)

	// 图片计费路径不受影响：仍能读到 image-only 条目的图片单价。
	raw := pricingSvc.GetModelPricing("imagen-9.0-generate")
	require.NotNil(t, raw)
	require.InDelta(t, 0.04, raw.OutputCostPerImage, 1e-12)
}

func TestPricingService_MergesFallbackOnlyModels(t *testing.T) {
	dir := t.TempDir()
	fallbackFile := filepath.Join(dir, "fallback.json")
	require.NoError(t, os.WriteFile(fallbackFile, []byte(`{
		"remote-model": {
			"input_cost_per_token": 0.000001,
			"litellm_provider": "test",
			"mode": "chat"
		},
		"gemini-3.1-flash-lite-image": {
			"output_cost_per_image": 0.034,
			"litellm_provider": "vertex_ai-language-models",
			"mode": "image_generation"
		}
	}`), 0644))

	svc := &PricingService{cfg: &config.Config{}}
	svc.cfg.Pricing.FallbackFile = fallbackFile
	remoteData, err := svc.parsePricingData([]byte(`{
		"remote-model": {
			"input_cost_per_token": 0.000002,
			"litellm_provider": "test",
			"mode": "chat"
		}
	}`))
	require.NoError(t, err)

	merged := svc.mergeFallbackPricingData(remoteData)
	// 回退链取高价：manual 非锁定价 0.000001 与已有官方 0.000002 取最高 → 0.000002
	require.InDelta(t, 0.000002, merged["remote-model"].InputCostPerToken, 1e-12)
	require.NotNil(t, merged["gemini-3.1-flash-lite-image"])
	require.InDelta(t, 0.034, merged["gemini-3.1-flash-lite-image"].OutputCostPerImage, 1e-12)
}

func TestGetModelPricing_Gpt53CodexSparkUsesGpt51CodexPricing(t *testing.T) {
	sparkPricing := &LiteLLMModelPricing{InputCostPerToken: 1}
	gpt53Pricing := &LiteLLMModelPricing{InputCostPerToken: 9}

	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-5.1-codex": sparkPricing,
			"gpt-5.3":       gpt53Pricing,
		},
	}

	got := svc.GetModelPricing("gpt-5.3-codex-spark")
	require.Same(t, sparkPricing, got)
}

func TestGetModelPricing_Gpt53CodexFallbackStillUsesGpt52Codex(t *testing.T) {
	gpt52CodexPricing := &LiteLLMModelPricing{InputCostPerToken: 2}

	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-5.2-codex": gpt52CodexPricing,
		},
	}

	got := svc.GetModelPricing("gpt-5.3-codex")
	require.Same(t, gpt52CodexPricing, got)
}

func TestGetModelPricing_OpenAIFallbackMatchedLoggedAsInfo(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	gpt52CodexPricing := &LiteLLMModelPricing{InputCostPerToken: 2}
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-5.2-codex": gpt52CodexPricing,
		},
	}

	got := svc.GetModelPricing("gpt-5.3-codex")
	require.Same(t, gpt52CodexPricing, got)

	require.True(t, logSink.ContainsMessageAtLevel("[Pricing] OpenAI fallback matched gpt-5.3-codex -> gpt-5.2-codex", "info"))
	require.False(t, logSink.ContainsMessageAtLevel("[Pricing] OpenAI fallback matched gpt-5.3-codex -> gpt-5.2-codex", "warn"))
}

func TestGetModelPricing_Gpt54UsesStaticFallbackWhenRemoteMissing(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-5.1-codex": &LiteLLMModelPricing{InputCostPerToken: 1.25e-6},
		},
	}

	got := svc.GetModelPricing("gpt-5.4")
	require.NotNil(t, got)
	require.InDelta(t, 2.5e-6, got.InputCostPerToken, 1e-12)
	require.InDelta(t, 1.5e-5, got.OutputCostPerToken, 1e-12)
	require.InDelta(t, 2.5e-7, got.CacheReadInputTokenCost, 1e-12)
	require.Equal(t, 272000, got.LongContextInputTokenThreshold)
	require.InDelta(t, 2.0, got.LongContextInputCostMultiplier, 1e-12)
	require.InDelta(t, 1.5, got.LongContextOutputCostMultiplier, 1e-12)
}

func TestGetModelPricing_OpenAICompactAliasUsesStaticFallback(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-5.1-codex": {InputCostPerToken: 1.25e-6},
		},
	}

	got := svc.GetModelPricing("openai/gpt5.5")
	require.NotNil(t, got)
	require.InDelta(t, 2.5e-6, got.InputCostPerToken, 1e-12)
	require.InDelta(t, 1.5e-5, got.OutputCostPerToken, 1e-12)
}

func TestPricingService_Gemini36FlashThinkingTiersUseBasePricing(t *testing.T) {
	basePricing := &LiteLLMModelPricing{
		InputCostPerToken:       1.5e-6,
		OutputCostPerToken:      7.5e-6,
		CacheReadInputTokenCost: 0.15e-6,
	}
	svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"gemini-3.6-flash": basePricing,
	}}

	for _, model := range []string{
		"gemini-3.6-flash",
		"gemini-3.6-flash-high",
		"gemini-3.6-flash-low",
		"gemini-3.6-flash-medium",
		"gemini-3.6-flash-tiered",
	} {
		t.Run(model, func(t *testing.T) {
			require.Same(t, basePricing, svc.GetModelPricing(model))
		})
	}
}

func TestPricingService_Gemini36FlashTierSpecificPricingTakesPrecedence(t *testing.T) {
	basePricing := &LiteLLMModelPricing{InputCostPerToken: 1.5e-6}
	tierPricing := &LiteLLMModelPricing{InputCostPerToken: 2e-6}
	svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"gemini-3.6-flash":     basePricing,
		"gemini-3.6-flash-low": tierPricing,
	}}

	require.Same(t, tierPricing, svc.GetModelPricing("models/gemini-3.6-flash-low"))
}

func TestBillingService_Gemini36FlashThinkingTierFallbacksAreBillable(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	tokens := UsageTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000}

	for _, model := range []string{
		"gemini-3.6-flash",
		"gemini-3.6-flash-high",
		"gemini-3.6-flash-low",
		"gemini-3.6-flash-medium",
		"gemini-3.6-flash-tiered",
	} {
		t.Run(model, func(t *testing.T) {
			cost, err := svc.CalculateCost(model, tokens, 1)
			require.NoError(t, err)
			require.InDelta(t, 1.5, cost.InputCost, 1e-12)
			require.InDelta(t, 7.5, cost.OutputCost, 1e-12)
			require.InDelta(t, 0.15, cost.CacheReadCost, 1e-12)
			require.InDelta(t, 9.15, cost.TotalCost, 1e-12)
		})
	}
}

func TestDefaultPricingIncludesGemini36FlashRates(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "resources", "model-pricing", "model_prices_and_context_window.json"))
	require.NoError(t, err)

	pricingSvc := &PricingService{}
	pricingData, err := pricingSvc.parsePricingData(data)
	require.NoError(t, err)
	pricingSvc.pricingData = pricingData
	billingSvc := NewBillingService(&config.Config{}, pricingSvc)

	for _, model := range []string{"gemini-3.6-flash", "gemini-3.6-flash-low", "gemini-3.6-flash-high"} {
		t.Run(model, func(t *testing.T) {
			pricing, err := billingSvc.GetModelPricing(model)
			require.NoError(t, err)
			require.InDelta(t, 1.5e-6, pricing.InputPricePerToken, 1e-12)
			require.InDelta(t, 7.5e-6, pricing.OutputPricePerToken, 1e-12)
			require.InDelta(t, 0.15e-6, pricing.CacheReadPricePerToken, 1e-12)
		})
	}
}

func TestDefaultPricingUsesCurrentCodexAutoReviewBaseRates(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "resources", "model-pricing", "model_prices_and_context_window.json"))
	require.NoError(t, err)

	svc := &PricingService{}
	pricingData, err := svc.parsePricingData(data)
	require.NoError(t, err)
	svc.pricingData = pricingData

	got := svc.GetModelPricing("codex-auto-review")
	require.NotNil(t, got)
	require.InDelta(t, 0.2e-6, got.InputCostPerToken, 1e-12)
	require.InDelta(t, 1.2e-6, got.OutputCostPerToken, 1e-12)
	require.InDelta(t, 0.02e-6, got.CacheReadInputTokenCost, 1e-12)

	// Auto-review is an internal Codex model. Do not infer public GPT-5.6 API
	// service-tier, cache-write, or long-context pricing without an upstream
	// usage contract for this dedicated model.
	require.Zero(t, got.InputCostPerTokenPriority)
	require.Zero(t, got.OutputCostPerTokenPriority)
	require.Zero(t, got.CacheReadInputTokenCostPriority)
	require.Zero(t, got.CacheCreationInputTokenCost)
	require.Zero(t, got.CacheCreationInputTokenCostPriority)
	require.Zero(t, got.LongContextInputTokenThreshold)
}

func TestGetModelPricing_Gpt54MiniUsesDedicatedStaticFallbackWhenRemoteMissing(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-5.1-codex": {InputCostPerToken: 1.25e-6},
		},
	}

	got := svc.GetModelPricing("gpt-5.4-mini")
	require.NotNil(t, got)
	require.InDelta(t, 7.5e-7, got.InputCostPerToken, 1e-12)
	require.InDelta(t, 4.5e-6, got.OutputCostPerToken, 1e-12)
	require.InDelta(t, 7.5e-8, got.CacheReadInputTokenCost, 1e-12)
	require.Zero(t, got.LongContextInputTokenThreshold)
}

func TestGetModelPricing_Gpt54NanoUsesDedicatedStaticFallbackWhenRemoteMissing(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-5.1-codex": {InputCostPerToken: 1.25e-6},
		},
	}

	got := svc.GetModelPricing("gpt-5.4-nano")
	require.NotNil(t, got)
	require.InDelta(t, 2e-7, got.InputCostPerToken, 1e-12)
	require.InDelta(t, 1.25e-6, got.OutputCostPerToken, 1e-12)
	require.InDelta(t, 2e-8, got.CacheReadInputTokenCost, 1e-12)
	require.Zero(t, got.LongContextInputTokenThreshold)
}

func TestGetModelPricing_ImageModelDoesNotFallbackToTextModel(t *testing.T) {
	imagePricing := &LiteLLMModelPricing{InputCostPerToken: 3}
	textPricing := &LiteLLMModelPricing{InputCostPerToken: 9}

	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-image-2": imagePricing,
			"gpt-5.4":     textPricing,
		},
	}

	got := svc.GetModelPricing("gpt-image-3")
	require.Same(t, imagePricing, got)
}

func TestParsePricingData_PreservesPriorityAndServiceTierFields(t *testing.T) {
	raw := map[string]any{
		"gpt-5.4": map[string]any{
			"input_cost_per_token":                 2.5e-6,
			"input_cost_per_token_priority":        5e-6,
			"output_cost_per_token":                15e-6,
			"output_cost_per_token_priority":       30e-6,
			"cache_read_input_token_cost":          0.25e-6,
			"cache_read_input_token_cost_priority": 0.5e-6,
			"supports_service_tier":                true,
			"supports_prompt_caching":              true,
			"litellm_provider":                     "openai",
			"mode":                                 "chat",
		},
	}
	body, err := json.Marshal(raw)
	require.NoError(t, err)

	svc := &PricingService{}
	pricingMap, err := svc.parsePricingData(body)
	require.NoError(t, err)

	pricing := pricingMap["gpt-5.4"]
	require.NotNil(t, pricing)
	require.InDelta(t, 2.5e-6, pricing.InputCostPerToken, 1e-12)
	require.InDelta(t, 5e-6, pricing.InputCostPerTokenPriority, 1e-12)
	require.InDelta(t, 15e-6, pricing.OutputCostPerToken, 1e-12)
	require.InDelta(t, 30e-6, pricing.OutputCostPerTokenPriority, 1e-12)
	require.InDelta(t, 0.25e-6, pricing.CacheReadInputTokenCost, 1e-12)
	require.InDelta(t, 0.5e-6, pricing.CacheReadInputTokenCostPriority, 1e-12)
	require.True(t, pricing.SupportsServiceTier)
}

func TestParsePricingData_PreservesServiceTierPriorityFields(t *testing.T) {
	svc := &PricingService{}
	pricingData, err := svc.parsePricingData([]byte(`{
		"gpt-5.4": {
			"input_cost_per_token": 0.0000025,
			"input_cost_per_token_priority": 0.000005,
			"output_cost_per_token": 0.000015,
			"output_cost_per_token_priority": 0.00003,
			"cache_read_input_token_cost": 0.00000025,
			"cache_read_input_token_cost_priority": 0.0000005,
			"supports_service_tier": true,
			"litellm_provider": "openai",
			"mode": "chat"
		}
	}`))
	require.NoError(t, err)

	pricing := pricingData["gpt-5.4"]
	require.NotNil(t, pricing)
	require.InDelta(t, 0.0000025, pricing.InputCostPerToken, 1e-12)
	require.InDelta(t, 0.000005, pricing.InputCostPerTokenPriority, 1e-12)
	require.InDelta(t, 0.000015, pricing.OutputCostPerToken, 1e-12)
	require.InDelta(t, 0.00003, pricing.OutputCostPerTokenPriority, 1e-12)
	require.InDelta(t, 0.00000025, pricing.CacheReadInputTokenCost, 1e-12)
	require.InDelta(t, 0.0000005, pricing.CacheReadInputTokenCostPriority, 1e-12)
	require.True(t, pricing.SupportsServiceTier)
}

// ---------------------------------------------------------------------------
// ListModelNamesByProvider
// ---------------------------------------------------------------------------

func TestListModelNamesByProvider_ReturnsMatchingModels(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"claude-opus-4-5-20251101": {LiteLLMProvider: "anthropic", InputCostPerToken: 1.5e-5},
			"claude-sonnet-4-5":        {LiteLLMProvider: "anthropic", InputCostPerToken: 3e-6},
			"gpt-4o":                   {LiteLLMProvider: "openai", InputCostPerToken: 5e-6},
			"gemini-2.5-pro":           {LiteLLMProvider: "google", InputCostPerToken: 1.25e-6},
		},
	}

	got := svc.ListModelNamesByProvider("anthropic")
	require.ElementsMatch(t, []string{"claude-opus-4-5-20251101", "claude-sonnet-4-5"}, got)
	// Must be sorted
	require.Equal(t, "claude-opus-4-5-20251101", got[0])
	require.Equal(t, "claude-sonnet-4-5", got[1])
}

func TestListModelNamesByProvider_CaseInsensitive(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-4o": {LiteLLMProvider: "OpenAI", InputCostPerToken: 5e-6},
		},
	}

	got := svc.ListModelNamesByProvider("openai")
	require.Equal(t, []string{"gpt-4o"}, got)

	got2 := svc.ListModelNamesByProvider("OPENAI")
	require.Equal(t, []string{"gpt-4o"}, got2)
}

func TestListModelNamesByProvider_NoMatch(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-4o": {LiteLLMProvider: "openai", InputCostPerToken: 5e-6},
		},
	}

	got := svc.ListModelNamesByProvider("anthropic")
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestListModelNamesByProvider_EmptyCatalog(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{},
	}

	got := svc.ListModelNamesByProvider("openai")
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestPricingService_MergeFallbackTakeHigher(t *testing.T) {
	dir := t.TempDir()
	fallbackFile := filepath.Join(dir, "fallback.json")
	// 手动非锁定价低于官方价 → 取高价（官方价保留）
	require.NoError(t, os.WriteFile(fallbackFile, []byte(`{
		"shared-model": {
			"input_cost_per_token": 0.000001,
			"output_cost_per_token": 0.000003,
			"litellm_provider": "test",
			"mode": "chat"
		}
	}`), 0644))

	svc := &PricingService{cfg: &config.Config{}}
	svc.cfg.Pricing.FallbackFile = fallbackFile
	remoteData, err := svc.parsePricingData([]byte(`{
		"shared-model": {
			"input_cost_per_token": 0.000002,
			"output_cost_per_token": 0.000002,
			"litellm_provider": "test",
			"mode": "chat"
		}
	}`))
	require.NoError(t, err)

	merged := svc.mergeFallbackPricingData(remoteData)
	// 取高：input 0.000002 > 0.000001 → 0.000002；output 0.000003 > 0.000002 → 0.000003
	require.InDelta(t, 0.000002, merged["shared-model"].InputCostPerToken, 1e-12)
	require.InDelta(t, 0.000003, merged["shared-model"].OutputCostPerToken, 1e-12)
}

func TestPricingService_MergeFallbackForceLocked(t *testing.T) {
	dir := t.TempDir()
	fallbackFile := filepath.Join(dir, "fallback.json")
	// 强制定价：locked=true，无论官方价更高/更低一律以 locked 为准
	require.NoError(t, os.WriteFile(fallbackFile, []byte(`{
		"locked-model": {
			"input_cost_per_token": 0.000004,
			"output_cost_per_token": 0.000006,
			"litellm_provider": "test",
			"mode": "chat",
			"locked": true
		}
	}`), 0644))

	svc := &PricingService{cfg: &config.Config{}}
	svc.cfg.Pricing.FallbackFile = fallbackFile
	remoteData, err := svc.parsePricingData([]byte(`{
		"locked-model": {
			"input_cost_per_token": 0.000005,
			"output_cost_per_token": 0.000007,
			"litellm_provider": "test",
			"mode": "chat"
		}
	}`))
	require.NoError(t, err)

	merged := svc.mergeFallbackPricingData(remoteData)
	// locked 强制：即使官方更高，仍用 locked 的 0.000004/0.000006
	require.InDelta(t, 0.000004, merged["locked-model"].InputCostPerToken, 1e-12)
	require.InDelta(t, 0.000006, merged["locked-model"].OutputCostPerToken, 1e-12)
	require.True(t, merged["locked-model"].PriceLocked)
}

func TestBranchModelCandidates_DoubaoHyphenToDot(t *testing.T) {
	cands := branchModelCandidates("doubao-seed-2-0-lite")
	var found bool
	for _, c := range cands {
		if c == "doubao-seed-2.0-lite" {
			found = true
		}
	}
	require.True(t, found, "doubao 连字符版本号应归一为点号: %v", cands)

	cands2 := branchModelCandidates("doubao-seed-2-1-turbo")
	var found2 bool
	for _, c := range cands2 {
		if c == "doubao-seed-2.1-turbo" {
			found2 = true
		}
	}
	require.True(t, found2, "doubao-seed-2-1 应归一为 2.1: %v", cands2)

	// 不应误伤普通连字符词（seed / evolving 等无数字-数字形态）
	for _, c := range branchModelCandidates("doubao-seed-evolving") {
		require.NotEqual(t, "doubao.seed-evolving", c)
	}
}

// ========== GetModelMetadata fallback 测试（2026-08-21 新增）==========
// 覆盖 models.dev 未收录的模型（如豆包系）从 LiteLLM 上游 fallback JSON 携带的
// max_input_tokens / max_output_tokens / supported_modalities_* 字段取元数据。

// TestParsePricingData_PreservesMetadataFields 验证 parsePricingData 把 fallback JSON
// 里的 max_input_tokens / max_output_tokens / supported_modalities_input /
// supported_modalities_output 正确写入 LiteLLMModelPricing。
func TestParsePricingData_PreservesMetadataFields(t *testing.T) {
	svc := &PricingService{}
	body := []byte(`{
		"doubao-seed-2.1-turbo": {
			"input_cost_per_token": 0.0000008,
			"output_cost_per_token": 0.000002,
			"max_input_tokens": 128000,
			"max_output_tokens": 8192,
			"supported_modalities_input": ["text"],
			"supported_modalities_output": ["text"]
		},
		"doubao-seed-evolving": {
			"input_cost_per_token": 0.000001,
			"output_cost_per_token": 0.0000025,
			"max_input_tokens": 256000,
			"max_output_tokens": 16384,
			"supported_modalities_input": ["text", "image"],
			"supported_modalities_output": ["text"]
		}
	}`)

	data, err := svc.parsePricingData(body)
	require.NoError(t, err)

	// doubao-seed-2.1-turbo
	p1 := data["doubao-seed-2.1-turbo"]
	require.NotNil(t, p1)
	require.Equal(t, int64(128000), p1.ContextLength, "context_length 应从 max_input_tokens 解析")
	require.Equal(t, int64(8192), p1.MaxOutput, "max_output 应从 max_output_tokens 解析")
	require.Equal(t, []string{"text"}, p1.ModalitiesInput)
	require.Equal(t, []string{"text"}, p1.ModalitiesOutput)

	// doubao-seed-evolving：多模态 input
	p2 := data["doubao-seed-evolving"]
	require.NotNil(t, p2)
	require.Equal(t, int64(256000), p2.ContextLength)
	require.Equal(t, int64(16384), p2.MaxOutput)
	require.Equal(t, []string{"text", "image"}, p2.ModalitiesInput)
	require.Equal(t, []string{"text"}, p2.ModalitiesOutput)

	// 复制应独立：parsePricingData 处理多 entry 时不能让两个 entry 的
	// ModalitiesInput 共享同一 backing array（json.Unmarshal 虽然通常分配
	// 新 slice，但显式拷贝可防止未来 json 优化/共享 buffer 时污染）。
	p2.ModalitiesInput[0] = "MUTATED"
	require.Equal(t, "text", data["doubao-seed-2.1-turbo"].ModalitiesInput[0],
		"修改 doubao-seed-evolving 不应污染 doubao-seed-2.1-turbo（独立 backing array）")
	// 自己污染自己 map 的同条目属于 Go 指针语义，不测
}

// TestParsePricingData_MetadataOptional 验证价格字段存在但无元数据时不报错也不写入零值。
// 部分 LiteLLM 条目可能只携带价格不携带 max_*_tokens 字段。
func TestParsePricingData_MetadataOptional(t *testing.T) {
	svc := &PricingService{}
	body := []byte(`{
		"gpt-image-2": {
			"output_cost_per_image": 0.04,
			"litellm_provider": "openai"
		}
	}`)
	data, err := svc.parsePricingData(body)
	require.NoError(t, err)
	p := data["gpt-image-2"]
	require.NotNil(t, p)
	require.Equal(t, int64(0), p.ContextLength, "无 max_input_tokens 时 context_length 应为 0")
	require.Equal(t, int64(0), p.MaxOutput)
	require.Nil(t, p.ModalitiesInput)
	require.Nil(t, p.ModalitiesOutput)
}

// TestGetModelMetadata_FallbackToFallbackJSON 主场景：models.dev miss → pricingData fallback 命中。
// 这是豆包系模型参数/模态显示的修复路径。
func TestGetModelMetadata_FallbackToFallbackJSON(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"doubao-seed-2.1-turbo": {
				InputCostPerToken:  0.0000008,
				OutputCostPerToken: 0.000002,
				ContextLength:      128000,
				MaxOutput:          8192,
				ModalitiesInput:    []string{"text"},
				ModalitiesOutput:   []string{"text"},
			},
			"doubao-seed-evolving": {
				InputCostPerToken:  0.000001,
				OutputCostPerToken: 0.0000025,
				ContextLength:      256000,
				MaxOutput:          16384,
				ModalitiesInput:    []string{"text", "image"},
				ModalitiesOutput:   []string{"text"},
			},
		},
	}
	// modelsDev 留 nil：模拟「上游完全没收录」场景

	ctx, maxOut, mods := svc.GetModelMetadata("doubao-seed-2.1-turbo")
	require.Equal(t, int64(128000), ctx)
	require.Equal(t, int64(8192), maxOut)
	require.Equal(t, []string{"text"}, mods)

	ctx2, maxOut2, mods2 := svc.GetModelMetadata("doubao-seed-evolving")
	require.Equal(t, int64(256000), ctx2)
	require.Equal(t, int64(16384), maxOut2)
	// 多模态 input：去重 + 保序
	require.Equal(t, []string{"text", "image"}, mods2,
		"modalities 应去重并保持 input→output 顺序")
}

// TestGetModelMetadata_BranchCandidateFallback 验证分支模型剥离回退：
// 查询 doubao-seed-2-1-turbo（连字符版本）时，branchModelCandidates 会归一化
// 为 doubao-seed-2.1-turbo 命中 pricingData。
func TestGetModelMetadata_BranchCandidateFallback(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"doubao-seed-2.1-turbo": {
				ContextLength:   128000,
				MaxOutput:       8192,
				ModalitiesInput: []string{"text"},
			},
		},
	}

	ctx, maxOut, _ := svc.GetModelMetadata("doubao-seed-2-1-turbo")
	require.Equal(t, int64(128000), ctx, "分支剥离应将 doubao-seed-2-1-turbo 归一为 doubao-seed-2.1-turbo")
	require.Equal(t, int64(8192), maxOut)
}

// TestGetModelMetadata_ModelsDevPriorityWins 验证 models.dev 命中时不再走 fallback。
// 即使 pricingData 有同名条目，models.dev 的元数据应优先（语义：主数据源）。
func TestGetModelMetadata_ModelsDevPriorityWins(t *testing.T) {
	devClient := NewModelsDevClient("https://example.invalid/api.json")
	devClient.data["gpt-5.6"] = ModelsDevModel{
		ID: "gpt-5.6",
		Limit: struct {
			Context int64 `json:"context"`
			Output  int64 `json:"output"`
		}{Context: 100000, Output: 5000},
		Modalities: struct {
			Input  []string `json:"input"`
			Output []string `json:"output"`
		}{Input: []string{"text"}, Output: []string{"text"}},
	}

	svc := &PricingService{
		modelsDev: devClient,
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-5.6": {
				// fallback 值与 models.dev 不同：验证优先级
				ContextLength:   999999,
				MaxOutput:       88888,
				ModalitiesInput: []string{"text", "image"},
			},
		},
	}

	ctx, maxOut, mods := svc.GetModelMetadata("gpt-5.6")
	require.Equal(t, int64(100000), ctx, "models.dev 命中时 fallback 不应覆盖")
	require.Equal(t, int64(5000), maxOut)
	require.Equal(t, []string{"text"}, mods,
		"models.dev modalities 优先于 pricingData")
}

// TestGetModelMetadata_NoDataReturnsZero 验证完全无数据时返零值（前端表现为不显示元数据行）。
func TestGetModelMetadata_NoDataReturnsZero(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			// 只有价格无元数据
			"some-other-model": {InputCostPerToken: 0.001},
		},
	}
	ctx, maxOut, mods := svc.GetModelMetadata("totally-unknown-model")
	require.Equal(t, int64(0), ctx)
	require.Equal(t, int64(0), maxOut)
	require.Nil(t, mods)
}

// TestGetModelMetadata_NilSafe 验证 nil service / 空模型名守卫。
func TestGetModelMetadata_NilSafe(t *testing.T) {
	var svc *PricingService
	ctx, maxOut, mods := svc.GetModelMetadata("any-model")
	require.Equal(t, int64(0), ctx)
	require.Equal(t, int64(0), maxOut)
	require.Nil(t, mods)

	svc = &PricingService{}
	ctx2, maxOut2, mods2 := svc.GetModelMetadata("")
	require.Equal(t, int64(0), ctx2)
	require.Equal(t, int64(0), maxOut2)
	require.Nil(t, mods2)
}

// TestGetModelMetadata_PartialFallbackFields 验证 fallback JSON 只携带部分元数据时的优雅降级。
func TestGetModelMetadata_PartialFallbackFields(t *testing.T) {
	svc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			// 只有 context_length，没有 modalities
			"legacy-model-a": {
				ContextLength: 32000,
			},
			// 只有 modalities，没有 max_output
			"legacy-model-b": {
				ContextLength:   64000,
				ModalitiesInput: []string{"text"},
			},
		},
	}

	ctx, maxOut, mods := svc.GetModelMetadata("legacy-model-a")
	require.Equal(t, int64(32000), ctx)
	require.Equal(t, int64(0), maxOut, "无 max_output_tokens 时应返 0")
	require.Nil(t, mods, "无 modalities 时应返 nil")

	ctx2, maxOut2, mods2 := svc.GetModelMetadata("legacy-model-b")
	require.Equal(t, int64(64000), ctx2)
	require.Equal(t, int64(0), maxOut2)
	require.Equal(t, []string{"text"}, mods2)
}

// TestMergeModalities 单元测试：去重 + 去空 + 保序 + 全部为空返 nil。
func TestMergeModalities(t *testing.T) {
	t.Run("both empty returns nil", func(t *testing.T) {
		require.Nil(t, mergeModalities(nil, nil))
		require.Nil(t, mergeModalities([]string{}, []string{}))
	})
	t.Run("dedupes across input and output", func(t *testing.T) {
		// text 同时出现在 input 和 output：只保留一次
		got := mergeModalities([]string{"text", "image"}, []string{"text"})
		require.Equal(t, []string{"text", "image"}, got)
	})
	t.Run("skips empty strings", func(t *testing.T) {
		got := mergeModalities([]string{"", "text"}, []string{"image", ""})
		require.Equal(t, []string{"text", "image"}, got)
	})
	t.Run("preserves order", func(t *testing.T) {
		got := mergeModalities([]string{"image", "text"}, []string{"audio", "image"})
		require.Equal(t, []string{"image", "text", "audio"}, got)
	})
}
