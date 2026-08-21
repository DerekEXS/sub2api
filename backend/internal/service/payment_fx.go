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
	// fxStaleCutoff 数据新鲜度阈值：来源自带时间戳且数据早于该阈值的视为"过期"。
	// 过期来源仅在全部来源都失败/过期时作为最后手段使用（不写入缓存，下次调用重新评估）。
	// 7 天覆盖：ECB（frankfurter）按工作日更新，长周末后数据最多 ~4 天旧；open.er-api 每日更新。
	fxStaleCutoff = 7 * 24 * time.Hour
)

// defaultFXAPIURLs 默认多源汇率 API 回退链（task #321 修复，2026-08-16 实测）：
//   - open.er-api.com/v6/latest/USD：可用，每日更新，免费无 key（exchangerate-api 免费版现行端点）
//   - api.frankfurter.dev/v1/latest?base=USD：可用，ECB 官方数据（api.frankfurter.app 已 301 到 .dev，直接用 .dev 省一跳）
//   - api.fxratesapi.com/latest?base=USD：可用，独立源
//
// 不再列入：api.exchangerate-api.com/v4（已弃用，响应体带 WARNING_UPGRADE_TO_V6，与 open.er-api 同源
// 无冗余意义）、api.exchangerate.host（实测已死：需付费 access_key，返回 missing_access_key）。
var defaultFXAPIURLs = []string{
	"https://open.er-api.com/v6/latest/USD",
	"https://api.frankfurter.dev/v1/latest?base=USD",
	"https://api.fxratesapi.com/latest?base=USD",
}

type fxCacheEntry struct {
	FromCurrency string    `json:"from_currency"`
	ToCurrency   string    `json:"to_currency"`
	Rate         float64   `json:"rate"` // 1 from = rate to
	BaseCurrency string    `json:"base"` // 原始 base（用户 API 的 base）
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
		cache:      fxCacheFile{Entries: map[string]fxCacheEntry{}},
		cachePath:  cachePath,
		log:        log,
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
//   - 用户配了 FX API URL（可多条回退链）→ 逐个 HTTP 拉取，用 rates[to]/rates[from] 数学换算（与 base 无关）
//   - 全部拉取失败/未配 → 用 fallbackRate（settings.FXFallbackRate，CNY per USD）做 USD 中转
//
// 优先级（主人 2026-08-02 规范）：API 汇率优先，固定汇率兜底。
func (s *FXService) GetRate(ctx context.Context, from, to string, apiURLs []string, fallbackRate float64) float64 {
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

	// 尝试拉取（多 URL 回退链：逐个试，第一个成功的生效）
	urls := make([]string, 0, len(apiURLs))
	for _, u := range apiURLs {
		v := strings.TrimSpace(u)
		if v != "" {
			urls = append(urls, v)
		}
	}
	if len(urls) == 0 {
		// 未配置任何 URL 时用内置多源默认链，避免静默落到固定汇率
		urls = defaultFXAPIURLs
	}
	// 过期数据候选：全部来源都过期时作为最后手段（不写缓存，优先新鲜数据）
	var staleRate float64
	staleFound := false
	for _, url := range urls {
		rates, base, fetchedAt, ok := s.fetchAPI(ctx, url)
		if !ok {
			s.mu.Lock()
			s.consecutiveFail++
			s.mu.Unlock()
			if s.log != nil {
				s.log.Warn("fx api fetch failed", zap.String("url", url), zap.Int("consecutive_fail", s.consecutiveFail))
			}
			continue
		}
		// 数学换算：rate = rates[to] / rates[from]（与 API 的 base 无关）
		rFrom, okFrom := rates[from]
		rTo, okTo := rates[to]
		if !okFrom || !okTo || rFrom <= 0 || rTo <= 0 {
			if s.log != nil {
				s.log.Warn("fx api missing pair", zap.String("from", from), zap.String("to", to), zap.String("url", url))
			}
			continue
		}
		rate := rTo / rFrom
		if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			if s.log != nil {
				s.log.Warn("fx api abnormal rate", zap.String("url", url), zap.Float64("rate", rate))
			}
			continue
		}
		// 新鲜度判定：无时间戳的来源按新鲜处理；有过期时间戳的来源只作最后手段
		if fetchedAt.IsZero() || now.Sub(fetchedAt) <= fxStaleCutoff {
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
		if s.log != nil {
			s.log.Warn("fx api data stale, skipping", zap.String("url", url), zap.Time("fetched_at", fetchedAt))
		}
		if !staleFound {
			staleFound = true
			staleRate = rate
		}
	}
	// 全部来源都过期：用最后手段的过期值（比固定汇率更贴近市场），不写缓存
	if staleFound {
		if s.log != nil {
			s.log.Warn("fx api all sources stale, using stale rate as last resort", zap.Float64("rate", staleRate))
		}
		return staleRate
	}

	// 兜底：用 settings.FXFallbackRate（CNY per USD）做 USD 中转
	return fallbackCrossViaUSD(from, to, fallbackRate)
}

// TestFetchAPI 公开测试单条汇率 API（v4.6.2 task 3，"立即获取汇率"按钮用）。
// 返回 (1 USD = ? CNY, base, ok)；不写缓存、不降级。
func (s *FXService) TestFetchAPI(url string) (float64, string, bool) {
	if s == nil {
		return 0, "", false
	}
	rates, base, _, ok := s.fetchAPI(context.Background(), url)
	if !ok {
		return 0, base, false
	}
	usd, okUSD := rates["USD"]
	cny, okCNY := rates["CNY"]
	if !okUSD || usd <= 0 || !okCNY || cny <= 0 {
		return 0, base, false
	}
	return cny / usd, base, true
}

// fxAPIResponse 兼容常见免费汇率 API 的响应结构（task #321 加固）：
//   - exchangerate-api v4 / frankfurter：{"base": "...", "rates": {...}, "date": "..."}
//   - open.er-api v6：{"result": "success", "base_code": "...", "rates": {...},
//     "time_last_update_utc": "Sun, 16 Aug 2026 ...", "time_last_update_unix": 1786838551}
//   - fxratesapi：{"base": "...", "rates": {...}, "date": "2026-08-16T10:22:00.000Z"}
type fxAPIResponse struct {
	Result             string             `json:"result"`
	Base               string             `json:"base"`
	BaseCode           string             `json:"base_code"`
	Rates              map[string]float64 `json:"rates"`
	Date               string             `json:"date"`
	TimeLastUpdateUTC  string             `json:"time_last_update_utc"`
	TimeLastUpdateUnix int64              `json:"time_last_update_unix"`
}

// parseFxTimestamp 解析常见汇率 API 的时间戳字段（按优先级）：
// time_last_update_unix（秒）→ time_last_update_utc（RFC1123/RFC3339）→ date（YYYY-MM-DD/RFC3339）。
// 全部解析失败返回零值，调用方视为"无时间戳"（按新鲜数据处理）。
func parseFxTimestamp(p *fxAPIResponse) time.Time {
	if p.TimeLastUpdateUnix > 0 {
		return time.Unix(p.TimeLastUpdateUnix, 0).UTC()
	}
	if s := strings.TrimSpace(p.TimeLastUpdateUTC); s != "" {
		for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC()
			}
		}
	}
	if s := strings.TrimSpace(p.Date); s != "" {
		for _, layout := range []string{"2006-01-02", time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

// fetchAPI 拉取汇率 API，返回完整 rates map + base + 数据时间戳（零值=无时间戳）。
// 实际：从 GET {apiURL} 拿 JSON，解析 "rates" map（key=币种, value=1 base = X target）。
// base 缺失时按 "USD" 处理（open.er-api 的 base_code 字段兼容）；result 字段非 success
// （如 error-type）直接跳过；调用方用 rates[to]/rates[from] 数学换算（与 base 无关）。
func (s *FXService) fetchAPI(ctx context.Context, apiURL string) (map[string]float64, string, time.Time, bool) {
	if !strings.HasPrefix(apiURL, "http://") && !strings.HasPrefix(apiURL, "https://") {
		return nil, "", time.Time{}, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, "", time.Time{}, false
	}
	req.Header.Set("User-Agent", "Sub2API/4.6.2 (+fx)")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, "", time.Time{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, "", time.Time{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, "", time.Time{}, false
	}
	var payload fxAPIResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", time.Time{}, false
	}
	if payload.Result != "" && payload.Result != "success" {
		return nil, "", time.Time{}, false
	}
	base := strings.TrimSpace(payload.Base)
	if base == "" {
		base = strings.TrimSpace(payload.BaseCode)
	}
	if base == "" {
		base = "USD"
	}
	if len(payload.Rates) == 0 {
		return nil, base, time.Time{}, false
	}
	return payload.Rates, base, parseFxTimestamp(&payload), true
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
