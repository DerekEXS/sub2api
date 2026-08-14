package service

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

type OpenAIMessagesDispatchModelConfig = domain.OpenAIMessagesDispatchModelConfig
type GroupModelsListConfig = domain.GroupModelsListConfig
type ReasoningEffortMapping = domain.ReasoningEffortMapping
type PeakWindow = domain.PeakWindow

type Group struct {
	ID             int64
	Name           string
	Description    string
	Platform       string
	RateMultiplier float64
	// 高峰时段倍率：peak_rate_enabled 为 true 且当前时刻处于 [PeakStart, PeakEnd) 时，
	// token 计费倍率额外乘以 PeakRateMultiplier。详见 PeakMultiplierAt。
	PeakRateEnabled    bool
	PeakStart          string
	PeakEnd            string
	PeakRateMultiplier float64
	// 多窗口高峰倍率（DeepSeek 双窗口谷峰价）：[{start,end,multiplier,models}]。
	// 非空时优先于旧单窗口 4 字段；窗口左闭右开 [start,end)、窗口级模型白名单
	// （空 = 组内全部模型命中，支持 * 通配符）。详见 PeakMultiplierAt。
	PeakWindows []PeakWindow
	IsExclusive bool
	Status      string
	Hydrated    bool // indicates the group was loaded from a trusted repository source
	// DuplicateOperationID is internal persistence metadata used only to recover
	// an already committed one-click copy. It must never be mapped to API DTOs.
	DuplicateOperationID string

	SubscriptionType    string
	DailyLimitUSD       *float64
	WeeklyLimitUSD      *float64
	MonthlyLimitUSD     *float64
	DefaultValidityDays int

	// 图片生成计费配置（antigravity 和 gemini 平台使用）
	AllowImageGeneration         bool
	AllowBatchImageGeneration    bool
	ImageRateIndependent         bool
	ImageRateMultiplier          float64
	ImagePrice1K                 *float64
	ImagePrice2K                 *float64
	ImagePrice4K                 *float64
	BatchImageDiscountMultiplier float64
	BatchImageHoldMultiplier     float64
	VideoRateIndependent         bool
	VideoRateMultiplier          float64
	VideoPrice480P               *float64
	VideoPrice720P               *float64
	VideoPrice1080P              *float64
	// VideoModelPrices is optional per-model-family per-second pricing
	// (groups.video_model_prices JSONB). Shape: family → resolution → USD/s.
	// When set for a model, overrides VideoPrice* for that model only.
	VideoModelPrices map[string]map[string]float64
	// Codex alpha/search 网页搜索单次价格（USD/次，仅 openai 平台使用）；
	// nil 表示使用默认价 defaultWebSearchPricePerCall（官方 $10/1000 次）。
	WebSearchPricePerCall *float64

	// 搜索工具显式定价（per 1k calls）。
	SearchPricePer1k *float64
	// Grok Voice 显式定价（分组级，不按文本 RateMultiplier）。
	AudioRealtimePricePerMin     *float64
	AudioTTSPricePerMillionChars *float64
	AudioSTTPricePerHour         *float64

	// ModelPricing overrides channel and built-in prices for matching models.
	// Token intervals are selected only when LongContextPricingEnabled is true.
	LongContextPricingEnabled bool
	ModelPricing              []ChannelModelPricing

	// Claude Code 客户端限制
	ClaudeCodeOnly  bool
	FallbackGroupID *int64
	// 无效请求兜底分组（仅 anthropic 平台使用）
	FallbackGroupIDOnInvalidRequest *int64

	// 模型路由配置
	// key: 模型匹配模式（支持 * 通配符，如 "claude-opus-*"）
	// value: 优先账号 ID 列表
	ModelRouting        map[string][]int64
	ModelRoutingEnabled bool

	// MCP XML 协议注入开关（仅 antigravity 平台使用）
	MCPXMLInject bool

	// 支持的模型系列（仅 antigravity 平台使用）
	// 可选值: claude, gemini_text, gemini_image
	SupportedModelScopes []string

	// 分组排序
	SortOrder int

	// OpenAI Messages 调度配置（仅 openai 平台使用）
	AllowMessagesDispatch       bool
	AllowLive                   bool
	RequireOAuthOnly            bool // 仅允许非 apikey 类型账号关联（OpenAI/Antigravity/Anthropic/Gemini）
	RequirePrivacySet           bool // 调度时仅允许 privacy 已成功设置的账号（OpenAI/Antigravity/Anthropic/Gemini）
	DefaultMappedModel          string
	MessagesDispatchModelConfig OpenAIMessagesDispatchModelConfig
	ModelsListConfig            GroupModelsListConfig

	// RPMLimit 分组级每分钟请求数上限（0 = 不限制）。
	// 一旦设置即接管该分组用户的限流（覆盖用户级 rpm_limit），可被 user-group rpm_override 进一步覆盖。
	RPMLimit int

	// MaxReasoningEffort limits the effective OpenAI/Codex reasoning effort.
	// Empty means unlimited; supported values are minimal/low/medium/high/xhigh/max.
	MaxReasoningEffort string
	// ReasoningEffortMappings rewrites explicit request values before applying the ceiling.
	ReasoningEffortMappings []ReasoningEffortMapping

	// 分组利润控制（五个 token 计费平台可启用）。
	// 调度准入条件：账号倍率 U 满足 U <= D*(1-margin-buffer)，
	// D 为请求用户当刻有效下游倍率（用户覆盖 ?? 分组默认，再乘高峰因子）。
	// 只过滤候选账号，不改变既有排序/评分/粘性/熔断。
	ProfitControlEnabled bool
	ProfitMinMargin      float64 // 最低毛利率，小数存储（0.30=30%）
	ProfitSafetyBuffer   float64 // 安全缓冲，小数，与 margin 相加后从 D 中扣除

	CreatedAt time.Time
	UpdatedAt time.Time

	AccountGroups           []AccountGroup
	AccountCount            int64
	ActiveAccountCount      int64
	RateLimitedAccountCount int64
}

func (g *Group) IsActive() bool {
	return g.Status == StatusActive
}

func (g *Group) IsSubscriptionType() bool {
	return g.SubscriptionType == SubscriptionTypeSubscription
}

func (g *Group) HasDailyLimit() bool {
	return g.DailyLimitUSD != nil && *g.DailyLimitUSD > 0
}

func (g *Group) HasWeeklyLimit() bool {
	return g.WeeklyLimitUSD != nil && *g.WeeklyLimitUSD > 0
}

func (g *Group) HasMonthlyLimit() bool {
	return g.MonthlyLimitUSD != nil && *g.MonthlyLimitUSD > 0
}

// GetImagePrice 根据 image_size 返回对应的图片生成价格
// 如果分组未配置价格，返回 nil（调用方应使用默认值）
func (g *Group) GetImagePrice(imageSize string) *float64 {
	switch imageSize {
	case "1K":
		return g.ImagePrice1K
	case "2K":
		return g.ImagePrice2K
	case "4K":
		return g.ImagePrice4K
	default:
		// 未知尺寸默认按 2K 计费
		return g.ImagePrice2K
	}
}

// GetVideoPrice 根据 resolution 返回对应的视频生成价格。
// 如果分组未配置价格，返回 nil（调用方应使用默认值）。
func (g *Group) GetVideoPrice(resolution string) *float64 {
	switch NormalizeVideoBillingResolutionOrDefault(resolution) {
	case VideoBillingResolution480P:
		return g.VideoPrice480P
	case VideoBillingResolution720P:
		return g.VideoPrice720P
	case VideoBillingResolution1080P:
		return g.VideoPrice1080P
	default:
		return g.VideoPrice480P
	}
}

// GetVideoPriceForModel prefers VideoModelPrices for the model family, then flat columns.
func (g *Group) GetVideoPriceForModel(model, resolution string) *float64 {
	if g == nil {
		return nil
	}
	if price := LookupVideoModelPrice(g.VideoModelPrices, model, resolution); price != nil {
		return price
	}
	return g.GetVideoPrice(resolution)
}

// VideoPriceConfig builds billing config including optional per-model map.
func (g *Group) VideoPriceConfig() *VideoPriceConfig {
	if g == nil {
		return nil
	}
	return &VideoPriceConfig{
		Price480P:   g.VideoPrice480P,
		Price720P:   g.VideoPrice720P,
		Price1080P:  g.VideoPrice1080P,
		ModelPrices: NormalizeVideoModelPrices(g.VideoModelPrices),
	}
}

// IsGroupContextValid reports whether a group from context has the fields required for routing decisions.
func IsGroupContextValid(group *Group) bool {
	if group == nil {
		return false
	}
	if group.ID <= 0 {
		return false
	}
	if !group.Hydrated {
		return false
	}
	if group.Platform == "" || group.Status == "" {
		return false
	}
	return true
}

// GetRoutingAccountIDs 根据请求模型获取路由账号 ID 列表
// 返回匹配的优先账号 ID 列表，如果没有匹配规则则返回 nil
func (g *Group) GetRoutingAccountIDs(requestedModel string) []int64 {
	if !g.ModelRoutingEnabled || len(g.ModelRouting) == 0 || requestedModel == "" {
		return nil
	}

	// 1. 精确匹配优先
	if accountIDs, ok := g.ModelRouting[requestedModel]; ok && len(accountIDs) > 0 {
		return accountIDs
	}

	// 2. 通配符匹配（前缀匹配）
	for pattern, accountIDs := range g.ModelRouting {
		if matchModelPattern(pattern, requestedModel) && len(accountIDs) > 0 {
			return accountIDs
		}
	}

	return nil
}

// matchModelPattern 检查模型是否匹配模式
// 支持 * 通配符，如 "claude-opus-*" 匹配 "claude-opus-4-20250514"
func matchModelPattern(pattern, model string) bool {
	if pattern == model {
		return true
	}

	// 处理 * 通配符（仅支持末尾通配符）
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(model, prefix)
	}

	return false
}

// parseMinutes 把 "HH:MM" 解析为当日分钟数（0..1439），格式非法返回 (0,false)。
// 手工解析而非 time.Parse：本函数位于每请求的计费热路径（PeakMultiplierAt），
// 避免对静态配置字符串重复走 layout 解析与 time.Time 分配。
// 接受集与 time.Parse("15:04", s) 完全一致（存量数据按旧解析写入，不得收窄）：
// 小时 1–2 位数字（0..23，允许不补零如 "1:30"），分钟固定 2 位数字（00..59）。
func parseMinutes(hhmm string) (int, bool) {
	colon := strings.IndexByte(hhmm, ':')
	if (colon != 1 && colon != 2) || len(hhmm)-colon-1 != 2 {
		return 0, false
	}
	h := 0
	for i := 0; i < colon; i++ {
		d := hhmm[i] - '0'
		if d > 9 {
			return 0, false
		}
		h = h*10 + int(d)
	}
	m1, m2 := hhmm[colon+1]-'0', hhmm[colon+2]-'0'
	if m1 > 9 || m2 > 9 {
		return 0, false
	}
	m := int(m1)*10 + int(m2)
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// PeakMultiplierAt 返回指定时刻 now、指定计费模型 model 的高峰因子。
//   - 未启用 / 未配置 / 配置非法 / 非高峰时段 / 模型不在白名单 → 返回 1.0（安全降级）
//   - 多窗口（PeakWindows 非空）优先：遍历窗口，命中（时区 = timezone.Location，
//     左闭右开 [start,end)）且模型白名单命中（空 = 全模型）→ 返回该窗口倍率；
//     所有窗口均未命中 → 1.0。窗口间重叠已在校验层禁止，遍历序不影响结果。
//   - 无多窗口时兼容旧单窗口字段：区间为左闭右开 [PeakStart, PeakEnd)，
//     仅支持当日区间，不支持跨天（如 22:00-次日02:00）
//   - 订阅类型限制已放开：standard 与 subscription 分组均可生效（DeepSeek 谷峰价需求）
//   - model 为空字符串时：仅命中模型白名单为空（全模型）的窗口，避免误伤
//
// 该方法是纯函数，不读取任何外部状态，便于单测。
func (g *Group) PeakMultiplierAt(now time.Time, model string) float64 {
	if g == nil || !g.PeakRateEnabled {
		return 1.0
	}
	if len(g.PeakWindows) > 0 {
		return g.peakMultiplierAtWindows(now, model)
	}
	if g.PeakStart == "" || g.PeakEnd == "" {
		return 1.0
	}
	start, ok1 := parseMinutes(g.PeakStart)
	end, ok2 := parseMinutes(g.PeakEnd)
	if !ok1 || !ok2 || start >= end {
		return 1.0
	}
	t := now.In(timezone.Location())
	cur := t.Hour()*60 + t.Minute()
	if cur >= start && cur < end {
		return g.PeakRateMultiplier
	}
	return 1.0
}

// peakMultiplierAtWindows 多窗口高峰命中判定：时区 = timezone.Location，
// 窗口左闭右开 [start,end)；命中后还需过窗口级模型白名单（空 = 全模型，
// 非空 = 仅白名单内模型，支持 * 通配符后缀）。脏窗口（格式非法/end<=start/
// 负倍率）安全跳过，与单窗口路径的降级语义一致。
func (g *Group) peakMultiplierAtWindows(now time.Time, model string) float64 {
	t := now.In(timezone.Location())
	cur := t.Hour()*60 + t.Minute()
	for _, w := range g.PeakWindows {
		start, ok1 := parseMinutes(w.Start)
		end, ok2 := parseMinutes(w.End)
		if !ok1 || !ok2 || start >= end || w.Multiplier < 0 {
			continue // 脏窗口安全跳过
		}
		if cur < start || cur >= end {
			continue
		}
		if len(w.Models) > 0 && !modelMatchesAny(w.Models, model) {
			continue
		}
		return w.Multiplier
	}
	return 1.0
}

// modelMatchesAny 判断 model 是否命中白名单任一模式（支持 * 通配符后缀）。
// model 为空时仅空白名单（len==0）在调用方已被放行，这里只处理非空白名单，
// 空 model 与任何非空模式（含 "*"）均不匹配。
func modelMatchesAny(patterns []string, model string) bool {
	if model == "" {
		return false
	}
	for _, p := range patterns {
		if matchModelPattern(p, model) {
			return true
		}
	}
	return false
}

// ValidatePeakRateConfig 是高峰倍率配置的唯一校验来源，供 handler 与 service 层共用。
//   - 订阅类型限制已放开：standard 与 subscription 分组均可配置（DeepSeek 谷峰价需求）；
//   - 多窗口（windows 非空）优先：委托 ValidatePeakWindows 校验（HH:MM 格式、end>start、
//     窗口间无重叠、multiplier>=0），legacy 单窗口字段可同时携带但不参与判定；
//   - 无多窗口时校验 legacy 单窗口：start/end 必填且合法（end>start，不支持跨天），multiplier>=0；
//   - multiplier=0 是允许的，表示高峰 token 请求按 0 倍计费，可用于折扣/免费策略；
//   - enabled=false 时放行（不关心类型与内容）。
func ValidatePeakRateConfig(subscriptionType string, enabled bool, start, end string, multiplier float64, windows []PeakWindow) error {
	if !enabled {
		return nil
	}
	if len(windows) > 0 {
		return ValidatePeakWindows(windows)
	}
	if start == "" || end == "" {
		return errors.New("peak_rate_enabled 为 true 时 peak_start 与 peak_end 必填（或提供 peak_windows）")
	}
	st, okStart := parseMinutes(start)
	if !okStart {
		return fmt.Errorf("peak_start 格式应为 HH:MM，got %q", start)
	}
	en, okEnd := parseMinutes(end)
	if !okEnd {
		return fmt.Errorf("peak_end 格式应为 HH:MM，got %q", end)
	}
	if st >= en {
		return errors.New("peak_end 必须大于 peak_start（不支持跨天区间，如 22:00-02:00）")
	}
	if multiplier < 0 {
		return errors.New("peak_rate_multiplier 不能为负")
	}
	return nil
}

// ValidatePeakWindows 校验多窗口高峰配置列表：
//   - 每个窗口 start/end 均为 HH:MM（与 parseMinutes 接受集一致）且 end > start（不支持跨天）
//   - 每个窗口 multiplier >= 0（0 允许，表示高峰免费；NaN/Inf 拒绝）
//   - 每个窗口 models 白名单条目非空（空列表 = 全模型合法）
//   - 窗口间不允许重叠（左闭右开 [a,b) 与 [c,d) 重叠 ⇔ a < d && c < b；
//     相邻窗口 end == start 不算重叠，如 09:00-12:00 与 12:00-14:00 合法）
func ValidatePeakWindows(windows []PeakWindow) error {
	type windowSpan struct {
		start, end int
	}
	spans := make([]windowSpan, 0, len(windows))
	for _, w := range windows {
		start, okStart := parseMinutes(w.Start)
		if !okStart {
			return fmt.Errorf("peak_windows[].start 格式应为 HH:MM，got %q", w.Start)
		}
		end, okEnd := parseMinutes(w.End)
		if !okEnd {
			return fmt.Errorf("peak_windows[].end 格式应为 HH:MM，got %q", w.End)
		}
		if start >= end {
			return fmt.Errorf("peak_windows[].end 必须大于 start（got %s-%s，不支持跨天区间）", w.Start, w.End)
		}
		if math.IsNaN(w.Multiplier) || math.IsInf(w.Multiplier, 0) {
			return fmt.Errorf("peak_windows[].multiplier 非法：%v", w.Multiplier)
		}
		if w.Multiplier < 0 {
			return fmt.Errorf("peak_windows[].multiplier 不能为负：%v", w.Multiplier)
		}
		for _, m := range w.Models {
			if strings.TrimSpace(m) == "" {
				return errors.New("peak_windows[].models 不能包含空模型名")
			}
		}
		spans = append(spans, windowSpan{start: start, end: end})
	}
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			a, b := spans[i].start, spans[i].end
			c, d := spans[j].start, spans[j].end
			if a < d && c < b {
				return fmt.Errorf("peak_windows 窗口 %02d:%02d-%02d:%02d 与 %02d:%02d-%02d:%02d 重叠",
					a/60, a%60, b/60, b%60, c/60, c%60, d/60, d%60)
			}
		}
	}
	return nil
}

// NormalizePeakRateConfig 归一化最终落库的单窗口高峰配置，CreateGroup 与 UpdateGroup 两条写路径共用（唯一收口）：
//   - 订阅类型限制已放开：不再按 subscriptionType 清空配置（standard 分组可携带高峰配置）；
//   - 关闭高峰时保留已配置的合法窗口（便于临时停用后再启用），
//     但清掉无法解析的脏字符串与负倍率，避免脏数据入库。
//
// 与 ValidatePeakRateConfig 的分工：enabled=true 时校验已保证各字段合法，本函数为无操作；
// enabled=false 时校验放行，由本函数兜底清洗。调用顺序为先归一化、后校验。
func NormalizePeakRateConfig(subscriptionType string, enabled bool, start, end string, multiplier float64) (bool, string, string, float64) {
	if !enabled {
		if _, ok := parseMinutes(start); !ok {
			start = ""
		}
		if _, ok := parseMinutes(end); !ok {
			end = ""
		}
		if multiplier < 0 {
			multiplier = 1.0
		}
	}
	return enabled, start, end, multiplier
}

// NormalizePeakWindows 归一化多窗口高峰配置：丢弃脏窗口（时间格式非法、
// end<=start、倍率为负/NaN/Inf）并清洗模型白名单（去空条目、trim），
// 与单窗口路径的清洗语义一致。enabled=false 时窗口保留但暂不生效。
func NormalizePeakWindows(windows []PeakWindow) []PeakWindow {
	if len(windows) == 0 {
		return nil
	}
	out := make([]PeakWindow, 0, len(windows))
	for _, w := range windows {
		start, okStart := parseMinutes(w.Start)
		end, okEnd := parseMinutes(w.End)
		if !okStart || !okEnd || start >= end ||
			math.IsNaN(w.Multiplier) || math.IsInf(w.Multiplier, 0) || w.Multiplier < 0 {
			continue // 脏窗口丢弃
		}
		models := make([]string, 0, len(w.Models))
		for _, m := range w.Models {
			if trimmed := strings.TrimSpace(m); trimmed != "" {
				models = append(models, trimmed)
			}
		}
		w.Models = models
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// computePeakAwareMultipliers 把"基础 token 倍率 base"（已含系统/分组/用户级倍率，但不含高峰）
// 拆分为最终 token 倍率与图片按次倍率：图片按次倍率基于 base 现算、不受高峰影响；token 倍率在 base 上叠加高峰因子。
// model 为本次请求的计费模型名，透传给 PeakMultiplierAt 用于窗口级模型白名单判定。
// gateway_service.recordUsageCore 与 openai_gateway_service.RecordUsage 共用此函数，
// 锁死"高峰因子只乘入 token 倍率、图片按次倍率不受影响"这一叠加顺序——任何调换都会被 group_peak_rate_test 覆盖。
func computePeakAwareMultipliers(apiKey *APIKey, base float64, now time.Time, model string) (text, image float64) {
	image = resolveImageRateMultiplier(apiKey, base)
	peak := 1.0
	if apiKey != nil && apiKey.Group != nil {
		peak = apiKey.Group.PeakMultiplierAt(now, model)
	}
	text = base * peak
	return
}

// validProfitControlRatio 判定 margin/buffer 是否为可落库的合法小数：[0,1) 且非 NaN/Inf。
func validProfitControlRatio(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < 1
}

// NormalizeGroupPlatform 把创建分组时省略的 platform 归一化为默认平台。
// handler 的入参预校验必须与 CreateGroup 落库时用同一个归一化结果，否则
// 「省略 platform + 启用利润控制」会被 handler 以「平台不支持」400 掉，
// 而该分组本会被建成受支持的 anthropic 分组。
func NormalizeGroupPlatform(platform string) string {
	if platform == "" {
		return PlatformAnthropic
	}
	return platform
}

// ValidateProfitControlConfig 是分组利润控制配置的唯一校验来源，handler 与 service 层共用。
// enabled=true 时仅允许五个可计费平台分组；margin/buffer 各自 ∈ [0,1)，且 margin+buffer < 1
// （相加 >=1 时阈值 <=0，所有可核价账号都会被排除，视为配置错误而不是静默全黑）。
// enabled=false 时放行（不关心平台），由 Normalize 兜底清洗数值。
func ValidateProfitControlConfig(platform string, enabled bool, minMargin, safetyBuffer float64) error {
	if !enabled {
		return nil
	}
	if !profitControlPlatformSupported(platform) {
		return errors.New("利润控制仅支持 openai、anthropic、gemini、grok、antigravity 平台分组")
	}
	if !validProfitControlRatio(minMargin) {
		return fmt.Errorf("profit_min_margin 应为 [0,1) 的小数，got %v", minMargin)
	}
	if !validProfitControlRatio(safetyBuffer) {
		return fmt.Errorf("profit_safety_buffer 应为 [0,1) 的小数，got %v", safetyBuffer)
	}
	if minMargin+safetyBuffer >= 1 {
		return errors.New("profit_min_margin 与 profit_safety_buffer 之和必须小于 1，否则将排除全部账号")
	}
	return nil
}

// NormalizeProfitControlConfig 归一化最终落库的利润控制配置，CreateGroup 与 UpdateGroup 共用（唯一收口）：
//   - 非五个平台分组不携带利润控制，一律重置为默认（关、0、0）；
//   - 支持平台关闭开关时保留合法数值（便于再次启用），清洗 NaN/Inf/越界脏值。
//
// 与 ValidateProfitControlConfig 的分工同高峰倍率：先归一化、后校验，
// 使"openai 转其他平台"这类更新能静默清空利润配置而不是被校验拒绝。
func NormalizeProfitControlConfig(platform string, enabled bool, minMargin, safetyBuffer float64) (bool, float64, float64) {
	if !profitControlPlatformSupported(platform) {
		return false, 0, 0
	}
	if !enabled {
		if !validProfitControlRatio(minMargin) {
			minMargin = 0
		}
		if !validProfitControlRatio(safetyBuffer) {
			safetyBuffer = 0
		}
	}
	return enabled, minMargin, safetyBuffer
}

func profitControlPlatformSupported(platform string) bool {
	switch platform {
	case PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformGrok, PlatformAntigravity:
		return true
	default:
		return false
	}
}

// GetSearchPricePer1k returns explicit search/tool price per 1k calls if configured.
func (g *Group) GetSearchPricePer1k() *float64 {
	if g == nil {
		return nil
	}
	return g.SearchPricePer1k
}
