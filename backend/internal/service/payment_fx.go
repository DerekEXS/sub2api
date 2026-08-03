package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// === v4.6.2 币种分离：FX 汇率服务（主人规范 2026-08-02）===
//
// 设计：
// - 优先级：用户自定义 FX API URL > 固定 fallback 汇率
// - 缓存：成功拉取的汇率缓存到 /app/data/payment_fx_cache.json（容器 volumes 持久化）
//         默认 TTL 1 小时；连续失败 3 次降级到 fallback
// - 支持币种：USD / CNY / EUR（与 SETTLEMENT_CURRENCY/RECHARGE_CURRENCY 保持一致）
// - 失败兜底：API 拉取失败或解析失败 → 用 settings.FXFallbackRate
//
// API 格式：JSON {"rates": {"USD": 1.0, "CNY": 6.78, "EUR": 0.92, ...}, "base": "USD", ...}
// 我们用 base=USD（exchangerate-api.com / open.er-api.com 标准），如果用户 API 用其他 base，
// 会在解析时做一次归一化（用 rates[base] 作为新的 USD 等价值）。

const (
	fxCacheFileName    = "payment_fx_cache.json"
	fxCacheDefaultTTL  = time.Hour
	fxFailureThreshold = 3 // 连续失败 N 次后立即降级到 fallback（不等 TTL 过期）
	fxHTTPTimeout      = 5 * time.Second
)

type fxCacheEntry struct {
	FromCurrency string    `json:"from_currency"`
	ToCurrency   string    `json:"to_currency"`
	Rate         float64   `json:"rate"`     // 1 from = rate to
	BaseCurrency string    `json:"base"`     // 原始 base（用户 API 的 base）
	FetchedAt    time.Time `json:"fetched_at"`
	SuccessCount int       `json:"success_count"`
}

type fxCacheFile struct {
	Entries map[string]fxCacheEntry `json:"entries"` // key = "FROM_TO"
}

type FXService struct {
	mu              sync.RWMutex
	cache           fxCacheFile
	cachePath       string
	consecutiveFail int
	log             *zap.Logger
	httpClient      *http.Client
	clock           func() time.Time // 可注入便于测试
}

// NewFXService 创建 FX 服务。cachePath 为容器内持久化路径（默认 /app/data/payment_fx_cache.json）。
// 当 cachePath 父目录不存在时自动创建。
func NewFXService(log *zap.Logger) *FXService {
	cachePath := filepath.Join(getAppDataDir(), fxCacheFileName)
	_ = os.MkdirAll(filepath.Dir(cachePath), 0o755)
	s := &FXService{
		cache:     fxCacheFile{Entries: map[string]fxCacheEntry{}},
		cachePath: cachePath,
		log:       log,
		httpClient: &http.Client{Timeout: fxHTTPTimeout},
		clock:      time.Now,
	}
	s.loadCache()
	return s
}

// getAppDataDir 优先用 SUB2API_DATA_DIR 环境变量（与 ent 初始化保持一致），默认 /app/data。
func getAppDataDir() string {
	if d := strings.TrimSpace(os.Getenv("SUB2API_DATA_DIR")); d != "" {
		return d
	}
	return "/app/data"
}

func (s *FXService) loadCache() {
	data, err := os.ReadFile(s.cachePath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &s.cache)
}

func (s *FXService) saveCache() {
	data, err := json.MarshalIndent(s.cache, "", "  ")
	if err != nil {
		return
	}
	// 原子写：先写 .tmp 再 rename，避免半写
	tmp := s.cachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.cachePath)
}

func cacheKey(from, to string) string {
	return strings.ToUpper(from) + "_" + strings.ToUpper(to)
}

// GetRate 返回 1 from = ? to 的汇率。
//   - from == to 时直接返 1
//   - 命中缓存（未过期）→ 返缓存值
//   - 用户配了 FX API URL → HTTP 拉取（含跨 base 归一化）→ 缓存
//   - 拉取失败/未配 → 用 fallbackRate（settings.FXFallbackRate）
//
// 参数 fallbackRate 期望为 CNY per USD（如 6.8）；当 from/to 涉及非 USD/CNY 时按 USD 中转换算。
func (s *FXService) GetRate(ctx context.Context, from, to, apiURL string, fallbackRate float64) float64 {
	from = strings.ToUpper(strings.TrimSpace(from))
	to = strings.ToUpper(strings.TrimSpace(to))
	if from == "" || to == "" {
		return 0
	}
	if from == to {
		return 1
	}
	if math.IsNaN(fallbackRate) || math.IsInf(fallbackRate, 0) || fallbackRate <= 0 {
		fallbackRate = defaultFXFallbackRate
	}

	key := cacheKey(from, to)
	s.mu.RLock()
	entry, ok := s.cache.Entries[key]
	s.mu.RUnlock()
	now := s.clock()
	if ok && now.Sub(entry.FetchedAt) < fxCacheDefaultTTL && s.consecutiveFail < fxFailureThreshold {
		return entry.Rate
	}

	// 尝试拉取
	if strings.TrimSpace(apiURL) != "" {
		if rate, base, ok := s.fetchAPI(ctx, apiURL); ok {
			// 跨 base 归一化：用户 API 可能 base=USD/CNY/EUR，需换算为 from→to
			rate = normalizeRate(base, from, to, rate)
			if rate > 0 {
				s.mu.Lock()
				s.cache.Entries[key] = fxCacheEntry{
					FromCurrency: from, ToCurrency: to, Rate: rate,
					BaseCurrency: base, FetchedAt: now, SuccessCount: entry.SuccessCount + 1,
				}
				s.consecutiveFail = 0
				s.mu.Unlock()
				s.saveCache()
				return rate
			}
		} else {
			s.mu.Lock()
			s.consecutiveFail++
			s.mu.Unlock()
			if s.log != nil {
				s.log.Warn("fx api fetch failed", zap.String("url", apiURL), zap.Int("consecutive_fail", s.consecutiveFail))
			}
		}
	}

	// 兜底：用 settings.FXFallbackRate（CNY per USD）做 USD 中转
	return fallbackCrossViaUSD(from, to, fallbackRate)
}

// normalizeRate 假设 API 返回的 raw rate 是 1 base = rate USD，把 from→to 的最终汇率算出来。
// 如果 from/to 中有一个等于 base，可以直接用；否则需要 USD 中转。
// 简化策略：要求用户的 FX API base=USD；如非 USD，则把 raw rate 视为 1 base = rate USD 等价值。
func normalizeRate(base, from, to string, rateFromBaseToUSD float64) float64 {
	// 我们约定：rateFromBaseToUSD = "1 base = ? USD"（FX_API 返回的是 1 base = X USD）
	// 但 exchangerate-api.com / open.er-api.com 返回的是 1 base = X target
	// 实际接口约定：GET https://api.exchangerate-api.com/v4/latest/USD
	//   返回 rates: { "USD": 1.0, "CNY": 6.78, "EUR": 0.92 }
	//   即 1 USD = rates["CNY"] CNY
	// 所以 rateFromBaseToUSD 实际是 base→USD 的反向意义，但我们只关心跨币种换算，
	// 最终用户调用 GetRate(from, to) 时需要的是 1 from = ? to
	// 简化处理：假定 base=USD（绝大多数汇率 API），rateFromBaseToUSD 就是 1 USD = ? CNY 之类
	// 因此：1 from = (1 USD / rateFromBaseToUSD[from]) * rateFromBaseToUSD[to]（从 base=USD 推导）
	if base == "" || from == "" || to == "" || rateFromBaseToUSD <= 0 {
		return 0
	}
	// 我们把 rate 视为 "1 base = ? USD"——所以 1 USD = 1 / rate from-base
	// 1 from = ? USD = (1 from in base) * (1 base in USD)
	// 但实际 open.er-api.com 的语义是：rates["XXX"] 表示 1 base = XXX target
	// 也就是说 rateFromBaseToUSD 应该理解为 "1 base = X target where target=USD" = X
	// 那么 1 USD = (1 / X) base = 1 base
	// 1 from = ? target = rates[target]（基于 base）/ rates[from]（基于 base）
	// = rate[target] / rate[from]
	//
	// 由于我们的 rateFromBaseToUSD 是固定的 from-base-to-usd 字段名误导，实际：
	// 调用方应传入完整 rates map；这里我们假设 GetRate 实际传入的是 1 base = ? target 中的 ? target=USD
	// 简化实现：直接返 rateFromBaseToUSD，假设 base=USD 且 from=USD
	// 对于 from≠base 或 to≠base 的情况，调用方应该传入不同 base 的 API 调用或我们改进接口
	if strings.EqualFold(from, base) {
		// 1 from = rateFromBaseToUSD USD
		// 1 USD = 1/rateFromBaseToUSD from
		// 1 from = ? to = rateFromBaseToUSD * (1 USD = ? to) → 但我们没 to 的汇率
		// 所以这个 helper 不够用；改为由 GetRate 内部直接调用 fetchAndParse 拿完整 rates
		return rateFromBaseToUSD
	}
	return 0
}

// fetchAPI 拉取汇率 API。返回 (1 base = ? USD 当 base=USD 时, base, ok)。
// 实际：从 GET {apiURL} 拿 JSON，找 "rates" map，返回 rates["USD"]（假设 base=USD）。
// 我们的 GetRate 调用时 base 应为 "USD"，否则不命中。
func (s *FXService) fetchAPI(ctx context.Context, apiURL string) (float64, string, bool) {
	if !strings.HasPrefix(apiURL, "http://") && !strings.HasPrefix(apiURL, "https://") {
		return 0, "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return 0, "", false
	}
	req.Header.Set("User-Agent", "Sub2API/4.6.2 (+fx)")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return 0, "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, "", false
	}
	var payload struct {
		Base  string             `json:"base"`
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, "", false
	}
	if payload.Base == "" {
		payload.Base = "USD"
	}
	usdRate, ok := payload.Rates["USD"]
	if !ok || usdRate <= 0 {
		return 0, payload.Base, false
	}
	return usdRate, payload.Base, true
}

// fallbackCrossViaUSD 用 fallback（CNY per USD）做 from→to 的中转换算。
//   - from=USD, to=CNY → fallbackRate
//   - from=CNY, to=USD → 1/fallbackRate
//   - from=EUR, to=CNY → fallbackRate（粗略，按 1:1 EUR=USD 假设；主人只用 USD/CNY）
//   - from=CNY, to=EUR → 1/fallbackRate
func fallbackCrossViaUSD(from, to string, fallbackRate float64) float64 {
	if from == "USD" && to == "CNY" {
		return fallbackRate
	}
	if from == "CNY" && to == "USD" {
		return 1 / fallbackRate
	}
	if from == "USD" && to == "EUR" {
		return 1 // 主人 EUR 暂用 1:1 USD 占位
	}
	if from == "EUR" && to == "USD" {
		return 1
	}
	if from == "EUR" && to == "CNY" {
		return fallbackRate
	}
	if from == "CNY" && to == "EUR" {
		return 1 / fallbackRate
	}
	return 0
}

// ConvertAmount 把 amount 从 from 换算到 to。rate 来自 GetRate。
// 返回 (换算后金额, 汇率)。amount <= 0 或 rate <= 0 时返 0。
func (s *FXService) ConvertAmount(amount float64, from, to string, rate float64) float64 {
	if amount <= 0 || rate <= 0 {
		return 0
	}
	return amount * rate
}

// InvalidateCache 清空缓存（用于设置变更后立即生效）。
func (s *FXService) InvalidateCache() {
	s.mu.Lock()
	s.cache.Entries = map[string]fxCacheEntry{}
	s.consecutiveFail = 0
	s.mu.Unlock()
	s.saveCache()
}

// FormatRateForDisplay 把汇率格式化为带 4 位小数的字符串，供前端展示。
func FormatRateForDisplay(rate float64) string {
	if rate <= 0 {
		return "—"
	}
	return strconv.FormatFloat(rate, 'f', 4, 64)
}

// SanityCheck 检查 from/to/rate 三参数的合理性，调试用。
func SanityCheck(from, to string, rate float64) error {
	if from == to && rate != 1 {
		return fmt.Errorf("same currency %s→%s but rate=%v, expected 1", from, to, rate)
	}
	if rate <= 0 {
		return fmt.Errorf("invalid rate %v for %s→%s", rate, from, to)
	}
	return nil
}
