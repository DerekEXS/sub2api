package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

func init() {
	// 测试固定全局时区为 UTC，确保判定可复现。
	_ = timezone.Init("UTC")
}

func newPeakGroup(enabled bool, start, end string, mult float64) *Group {
	return &Group{
		SubscriptionType:   "subscription",
		PeakRateEnabled:    enabled,
		PeakStart:          start,
		PeakEnd:            end,
		PeakRateMultiplier: mult,
	}
}

// newMultiWindowGroup 构造多窗口高峰分组（DeepSeek 双窗口范式）。
func newMultiWindowGroup(enabled bool, windows ...PeakWindow) *Group {
	return &Group{
		SubscriptionType: "subscription",
		PeakRateEnabled:  enabled,
		PeakWindows:      windows,
	}
}

func at(hour, min int) time.Time {
	return time.Date(2026, 6, 29, hour, min, 0, 0, time.UTC)
}

func TestPeakMultiplierAt_DisabledOrUnconfigured(t *testing.T) {
	cases := []struct {
		name string
		g    *Group
	}{
		{"disabled", newPeakGroup(false, "14:00", "18:00", 3.0)},
		{"empty start", newPeakGroup(true, "", "18:00", 3.0)},
		{"empty end", newPeakGroup(true, "14:00", "", 3.0)},
		{"invalid start>=end", newPeakGroup(true, "18:00", "14:00", 3.0)},
		{"equal start==end", newPeakGroup(true, "14:00", "14:00", 3.0)},
		{"malformed start", newPeakGroup(true, "99:99", "18:00", 3.0)},
		{"disabled with windows", newMultiWindowGroup(false, PeakWindow{Start: "09:00", End: "12:00", Multiplier: 2.0})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.g.PeakMultiplierAt(at(15, 0), ""); got != 1.0 {
				t.Fatalf("expect 1.0, got %v", got)
			}
		})
	}
}

func TestPeakMultiplierAt_NilReceiver(t *testing.T) {
	var g *Group
	if got := g.PeakMultiplierAt(at(15, 0), ""); got != 1.0 {
		t.Fatalf("expect 1.0, got %v", got)
	}
}

func TestPeakMultiplierAt_Boundaries(t *testing.T) {
	g := newPeakGroup(true, "14:00", "18:00", 3.0)
	cases := []struct {
		t    time.Time
		want float64
	}{
		{at(13, 59), 1.0},
		{at(14, 0), 3.0},
		{at(15, 30), 3.0},
		{at(17, 59), 3.0},
		{at(18, 0), 1.0},
		{at(23, 0), 1.0},
	}
	for _, c := range cases {
		t.Run(c.t.Format("15:04"), func(t *testing.T) {
			if got := g.PeakMultiplierAt(c.t, ""); got != c.want {
				t.Fatalf("at %s: expect %v, got %v", c.t.Format("15:04"), c.want, got)
			}
		})
	}
}

func TestPeakMultiplierAt_RespectsTimezoneLocation(t *testing.T) {
	// 全局时区为 UTC。北京 15:00 = UTC 07:00，不在 [14:00,18:00)。
	nonUTC := time.Date(2026, 6, 29, 15, 0, 0, 0, mustLoad("Asia/Shanghai"))
	g := newPeakGroup(true, "14:00", "18:00", 3.0)
	if got := g.PeakMultiplierAt(nonUTC, ""); got != 1.0 {
		t.Fatalf("expect 1.0 (converted to UTC 07:00), got %v", got)
	}
}

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func TestValidatePeakRateConfig(t *testing.T) {
	cases := []struct {
		name    string
		subType string
		enabled bool
		start   string
		end     string
		mult    float64
		wantErr bool
	}{
		{"disabled passes through", "subscription", false, "", "", 0, false},
		{"subscription enabled valid", "subscription", true, "14:00", "18:00", 3.0, false},
		{"standard enabled now allowed", "standard", true, "14:00", "18:00", 3.0, false},
		{"empty type treated as standard now allowed", "", true, "14:00", "18:00", 3.0, false},
		{"standard disabled passes", "standard", false, "", "", 0, false},
		{"enabled empty start", "subscription", true, "", "18:00", 1.0, true},
		{"enabled empty end", "subscription", true, "14:00", "", 1.0, true},
		{"enabled malformed start", "subscription", true, "99:99", "18:00", 1.0, true},
		{"enabled malformed end", "subscription", true, "14:00", "25:00", 1.0, true},
		{"enabled equal start==end", "subscription", true, "14:00", "14:00", 1.0, true},
		{"enabled cross-day rejected", "subscription", true, "22:00", "02:00", 1.0, true},
		{"enabled negative multiplier", "subscription", true, "14:00", "18:00", -0.5, true},
		{"enabled zero multiplier allowed", "subscription", true, "14:00", "18:00", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidatePeakRateConfig(c.subType, c.enabled, c.start, c.end, c.mult, nil)
			if c.wantErr && err == nil {
				t.Fatalf("expect error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("expect no error, got %v", err)
			}
		})
	}
}

// TestValidatePeakRateConfig_MultiWindow 多窗口校验：窗口合法时 legacy 字段可为空，
// standard 分组可配置；窗口重叠/非法时拒绝。
func TestValidatePeakRateConfig_MultiWindow(t *testing.T) {
	deepseekWindows := []PeakWindow{
		{Start: "09:00", End: "12:00", Multiplier: 2.0},
		{Start: "14:00", End: "18:00", Multiplier: 2.0},
	}
	t.Run("standard group with windows passes", func(t *testing.T) {
		if err := ValidatePeakRateConfig("standard", true, "", "", 1.0, deepseekWindows); err != nil {
			t.Fatalf("expect no error, got %v", err)
		}
	})
	t.Run("adjacent windows do not overlap", func(t *testing.T) {
		windows := []PeakWindow{
			{Start: "09:00", End: "12:00", Multiplier: 2.0},
			{Start: "12:00", End: "14:00", Multiplier: 1.5},
		}
		if err := ValidatePeakRateConfig("standard", true, "", "", 1.0, windows); err != nil {
			t.Fatalf("adjacent [09:00,12:00)+[12:00,14:00) must pass, got %v", err)
		}
	})
	t.Run("overlapping windows rejected", func(t *testing.T) {
		windows := []PeakWindow{
			{Start: "09:00", End: "12:00", Multiplier: 2.0},
			{Start: "11:00", End: "14:00", Multiplier: 2.0},
		}
		if err := ValidatePeakRateConfig("standard", true, "", "", 1.0, windows); err == nil {
			t.Fatalf("overlapping windows must be rejected")
		}
	})
	t.Run("identical windows rejected", func(t *testing.T) {
		windows := []PeakWindow{
			{Start: "09:00", End: "12:00", Multiplier: 2.0},
			{Start: "09:00", End: "12:00", Multiplier: 3.0},
		}
		if err := ValidatePeakRateConfig("standard", true, "", "", 1.0, windows); err == nil {
			t.Fatalf("duplicate windows must be rejected")
		}
	})
	t.Run("malformed window start rejected", func(t *testing.T) {
		windows := []PeakWindow{{Start: "9:xx", End: "12:00", Multiplier: 2.0}}
		if err := ValidatePeakRateConfig("standard", true, "", "", 1.0, windows); err == nil {
			t.Fatalf("malformed start must be rejected")
		}
	})
	t.Run("cross-day window rejected", func(t *testing.T) {
		windows := []PeakWindow{{Start: "22:00", End: "02:00", Multiplier: 2.0}}
		if err := ValidatePeakRateConfig("standard", true, "", "", 1.0, windows); err == nil {
			t.Fatalf("cross-day window must be rejected")
		}
	})
	t.Run("negative multiplier rejected", func(t *testing.T) {
		windows := []PeakWindow{{Start: "09:00", End: "12:00", Multiplier: -1.0}}
		if err := ValidatePeakRateConfig("standard", true, "", "", 1.0, windows); err == nil {
			t.Fatalf("negative multiplier must be rejected")
		}
	})
	t.Run("empty model entry rejected", func(t *testing.T) {
		windows := []PeakWindow{{Start: "09:00", End: "12:00", Multiplier: 2.0, Models: []string{""}}}
		if err := ValidatePeakRateConfig("standard", true, "", "", 1.0, windows); err == nil {
			t.Fatalf("empty model entry must be rejected")
		}
	})
	t.Run("disabled with bad windows passes", func(t *testing.T) {
		windows := []PeakWindow{{Start: "99:99", End: "12:00", Multiplier: 2.0}}
		if err := ValidatePeakRateConfig("standard", false, "", "", 1.0, windows); err != nil {
			t.Fatalf("disabled must pass through, got %v", err)
		}
	})
}

// TestNormalizePeakWindows 归一化：脏窗口丢弃、模型白名单清洗、空结果归一为 nil。
func TestNormalizePeakWindows(t *testing.T) {
	t.Run("nil stays nil", func(t *testing.T) {
		if got := NormalizePeakWindows(nil); got != nil {
			t.Fatalf("expect nil, got %v", got)
		}
	})
	t.Run("dirty windows dropped", func(t *testing.T) {
		windows := []PeakWindow{
			{Start: "09:00", End: "12:00", Multiplier: 2.0},
			{Start: "bad", End: "12:00", Multiplier: 2.0},
			{Start: "14:00", End: "14:00", Multiplier: 2.0},  // start==end
			{Start: "15:00", End: "18:00", Multiplier: -0.5}, // negative
		}
		got := NormalizePeakWindows(windows)
		if len(got) != 1 || got[0].Start != "09:00" {
			t.Fatalf("expect only valid window kept, got %+v", got)
		}
	})
	t.Run("all dirty becomes nil", func(t *testing.T) {
		if got := NormalizePeakWindows([]PeakWindow{{Start: "bad", End: "12:00", Multiplier: 2.0}}); got != nil {
			t.Fatalf("expect nil, got %+v", got)
		}
	})
	t.Run("models trimmed and empties dropped", func(t *testing.T) {
		windows := []PeakWindow{{Start: "09:00", End: "12:00", Multiplier: 2.0, Models: []string{" deepseek-v4-flash ", "", "deepseek-*"}}}
		got := NormalizePeakWindows(windows)
		if len(got) != 1 || len(got[0].Models) != 2 ||
			got[0].Models[0] != "deepseek-v4-flash" || got[0].Models[1] != "deepseek-*" {
			t.Fatalf("unexpected normalized models: %+v", got)
		}
	})
}

// TestPeakMultiplierAt_MultiWindow DeepSeek 双窗口范式：
// [09:00,12:00) ×2 + [14:00,18:00) ×3，空白名单 = 全模型。
func TestPeakMultiplierAt_MultiWindow(t *testing.T) {
	g := newMultiWindowGroup(true,
		PeakWindow{Start: "09:00", End: "12:00", Multiplier: 2.0},
		PeakWindow{Start: "14:00", End: "18:00", Multiplier: 3.0},
	)
	cases := []struct {
		t    time.Time
		want float64
	}{
		{at(8, 59), 1.0},
		{at(9, 0), 2.0}, // 第一窗口左闭
		{at(11, 59), 2.0},
		{at(12, 0), 1.0},  // 边界：12:00 属于空闲（第一窗口右开）
		{at(12, 30), 1.0}, // 窗口间隙（午间）空闲
		{at(13, 59), 1.0},
		{at(14, 0), 3.0}, // 第二窗口左闭
		{at(17, 59), 3.0},
		{at(18, 0), 1.0}, // 边界：18:00 属于空闲（第二窗口右开）
		{at(18, 1), 1.0}, // 窗口间隙（晚间）空闲
		{at(23, 59), 1.0},
	}
	for _, c := range cases {
		t.Run(c.t.Format("15:04"), func(t *testing.T) {
			if got := g.PeakMultiplierAt(c.t, "deepseek-v4-flash"); got != c.want {
				t.Fatalf("at %s: expect %v, got %v", c.t.Format("15:04"), c.want, got)
			}
		})
	}
}

// TestPeakMultiplierAt_MultiWindowModelWhitelist 窗口级模型白名单：
// 命中窗口 + 白名单命中 → 倍率；白名单未命中 → 1.0；空 model 不命中非空白名单。
func TestPeakMultiplierAt_MultiWindowModelWhitelist(t *testing.T) {
	g := newMultiWindowGroup(true,
		PeakWindow{Start: "09:00", End: "12:00", Multiplier: 2.0, Models: []string{"deepseek-v4-flash", "deepseek-*"}},
		PeakWindow{Start: "14:00", End: "18:00", Multiplier: 3.0},
	)
	cases := []struct {
		name  string
		model string
		want  float64
	}{
		{"whitelist exact hit", "deepseek-v4-flash", 2.0},
		{"whitelist wildcard hit", "deepseek-v4-pro", 2.0},
		{"whitelist miss", "gpt-5.6", 1.0},
		{"empty model with non-empty whitelist", "", 1.0},
		{"second window no whitelist hits all", "gpt-5.6", 1.0}, // 14:00 未到
		{"second window whitelist-less at 15:00", "gpt-5.6", 3.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got float64
			if c.name == "second window whitelist-less at 15:00" {
				got = g.PeakMultiplierAt(at(15, 0), c.model)
			} else {
				got = g.PeakMultiplierAt(at(10, 0), c.model)
			}
			if got != c.want {
				t.Fatalf("model %q: expect %v, got %v", c.model, c.want, got)
			}
		})
	}
}

// TestPeakMultiplierAt_MultiWindowLegacyFallback 兼容性：peak_windows 为空时走
// 旧单窗口逻辑；非空时旧字段被忽略（多窗口优先）。
func TestPeakMultiplierAt_MultiWindowLegacyFallback(t *testing.T) {
	t.Run("legacy fields work when windows empty", func(t *testing.T) {
		g := newPeakGroup(true, "14:00", "18:00", 3.0)
		if got := g.PeakMultiplierAt(at(15, 0), ""); got != 3.0 {
			t.Fatalf("legacy single window must apply, got %v", got)
		}
	})
	t.Run("windows take precedence over legacy", func(t *testing.T) {
		g := newMultiWindowGroup(true,
			PeakWindow{Start: "09:00", End: "12:00", Multiplier: 2.0},
		)
		// 遗留字段同时存在且指向其他窗口，不应生效
		g.PeakStart, g.PeakEnd, g.PeakRateMultiplier = "14:00", "18:00", 3.0
		if got := g.PeakMultiplierAt(at(10, 0), ""); got != 2.0 {
			t.Fatalf("windows must take precedence, got %v", got)
		}
		if got := g.PeakMultiplierAt(at(15, 0), ""); got != 1.0 {
			t.Fatalf("legacy fields must be ignored when windows set, got %v", got)
		}
	})
	t.Run("dirty windows skipped safely", func(t *testing.T) {
		g := newMultiWindowGroup(true,
			PeakWindow{Start: "bad", End: "12:00", Multiplier: 2.0},
			PeakWindow{Start: "14:00", End: "13:00", Multiplier: 3.0}, // end<=start
			PeakWindow{Start: "16:00", End: "18:00", Multiplier: 4.0},
		)
		if got := g.PeakMultiplierAt(at(10, 0), ""); got != 1.0 {
			t.Fatalf("dirty window must be skipped, got %v", got)
		}
		if got := g.PeakMultiplierAt(at(17, 0), ""); got != 4.0 {
			t.Fatalf("clean window must apply, got %v", got)
		}
	})
	t.Run("multi window respects timezone", func(t *testing.T) {
		// 全局时区 UTC。北京时间 10:00 = UTC 02:00，不在 [09:00,12:00) UTC 判定。
		beijing10 := time.Date(2026, 6, 29, 10, 0, 0, 0, mustLoad("Asia/Shanghai"))
		g := newMultiWindowGroup(true, PeakWindow{Start: "09:00", End: "12:00", Multiplier: 2.0})
		if got := g.PeakMultiplierAt(beijing10, ""); got != 1.0 {
			t.Fatalf("expect 1.0 (UTC 02:00 outside window), got %v", got)
		}
	})
}

// TestPeakMultiplierAt_StandardTypeNoLongerDegrades 语义变更：standard 分组
// 现在可以使用高峰倍率（原 TestPeakMultiplierAt_StandardTypeDegradesToOne 语义反转，
// 订阅类型限制已放开）。
func TestPeakMultiplierAt_StandardTypeNoLongerDegrades(t *testing.T) {
	g := newPeakGroup(true, "14:00", "18:00", 3.0)
	g.SubscriptionType = "standard"
	if got := g.PeakMultiplierAt(at(15, 30), ""); got != 3.0 {
		t.Fatalf("standard group peak multiplier must now apply, got %v", got)
	}

	sub := newPeakGroup(true, "14:00", "18:00", 3.0)
	sub.SubscriptionType = "subscription"
	if got := sub.PeakMultiplierAt(at(15, 30), ""); got != 3.0 {
		t.Fatalf("subscription group peak multiplier: got %v, want 3.0", got)
	}

	multi := newMultiWindowGroup(true, PeakWindow{Start: "09:00", End: "12:00", Multiplier: 2.0})
	multi.SubscriptionType = "standard"
	if got := multi.PeakMultiplierAt(at(10, 0), ""); got != 2.0 {
		t.Fatalf("standard multi-window peak multiplier must now apply, got %v", got)
	}
}

// TestNormalizePeakRateConfig_StandardNotCleared 语义变更：NormalizePeakRateConfig
// 不再对 standard 分组清空高峰配置。
func TestNormalizePeakRateConfig_StandardNotCleared(t *testing.T) {
	enabled, start, end, mult := NormalizePeakRateConfig("standard", true, "14:00", "18:00", 3.0)
	if !enabled || start != "14:00" || end != "18:00" || mult != 3.0 {
		t.Fatalf("standard group config must be preserved, got (%v,%q,%q,%v)", enabled, start, end, mult)
	}
	// disabled 时仍清洗脏字段
	enabled, start, end, mult = NormalizePeakRateConfig("standard", false, "bad", "18:00", -1.0)
	if enabled || start != "" || end != "18:00" || mult != 1.0 {
		t.Fatalf("disabled dirty cleanup, got (%v,%q,%q,%v)", enabled, start, end, mult)
	}
}

// TestPeakMultiplier_GatewayBillingSequence 调用 gateway_service.recordUsageCore 与
// openai_gateway_service.RecordUsage 共用的 computePeakAwareMultipliers，验证计费叠加顺序：
// 图片按次倍率基于基础倍率算出且不受高峰影响，高峰因子只乘入 token 倍率。
// 若有人调换叠加顺序或把高峰并入 imageMultiplier，此测试会失败。
func TestPeakMultiplier_GatewayBillingSequence(t *testing.T) {
	const baseMultiplier = 0.8
	apiKey := &APIKey{Group: newPeakGroup(true, "14:00", "18:00", 3.0)}
	approxEq := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

	t.Run("peak hour amplifies token multiplier only", func(t *testing.T) {
		now := at(15, 30) // 处于 [14:00, 18:00)
		tokenMultiplier, imageMultiplier := computePeakAwareMultipliers(apiKey, baseMultiplier, now, "deepseek-v4-flash")
		if !approxEq(imageMultiplier, baseMultiplier) {
			t.Fatalf("image multiplier must not be affected by peak: got %v, want %v", imageMultiplier, baseMultiplier)
		}
		if want := baseMultiplier * 3.0; !approxEq(tokenMultiplier, want) {
			t.Fatalf("token multiplier should include peak factor: got %v, want %v", tokenMultiplier, want)
		}
	})

	t.Run("off-peak leaves both multipliers at base", func(t *testing.T) {
		now := at(20, 0)
		tokenMultiplier, imageMultiplier := computePeakAwareMultipliers(apiKey, baseMultiplier, now, "deepseek-v4-flash")
		if !approxEq(imageMultiplier, baseMultiplier) {
			t.Fatalf("image multiplier: got %v, want %v", imageMultiplier, baseMultiplier)
		}
		if !approxEq(tokenMultiplier, baseMultiplier) {
			t.Fatalf("token multiplier should equal base off-peak: got %v, want %v", tokenMultiplier, baseMultiplier)
		}
	})

	t.Run("image independent mode decoupled from peak", func(t *testing.T) {
		indGroup := newPeakGroup(true, "14:00", "18:00", 3.0)
		indGroup.ImageRateIndependent = true
		indGroup.ImageRateMultiplier = 0.5
		indKey := &APIKey{Group: indGroup}
		now := at(15, 30)
		tokenMultiplier, imageMultiplier := computePeakAwareMultipliers(indKey, baseMultiplier, now, "deepseek-v4-flash")
		if !approxEq(imageMultiplier, 0.5) {
			t.Fatalf("independent image multiplier: got %v, want 0.5", imageMultiplier)
		}
		if want := baseMultiplier * 3.0; !approxEq(tokenMultiplier, want) {
			t.Fatalf("token multiplier should include peak factor: got %v, want %v", tokenMultiplier, want)
		}
	})

	t.Run("multi-window whitelist model applies to token multiplier", func(t *testing.T) {
		multiKey := &APIKey{Group: newMultiWindowGroup(true,
			PeakWindow{Start: "09:00", End: "12:00", Multiplier: 2.0, Models: []string{"deepseek-v4-flash"}},
		)}
		now := at(10, 0)
		hitMultiplier, _ := computePeakAwareMultipliers(multiKey, baseMultiplier, now, "deepseek-v4-flash")
		if !approxEq(hitMultiplier, baseMultiplier*2.0) {
			t.Fatalf("whitelist hit multiplier: got %v, want %v", hitMultiplier, baseMultiplier*2.0)
		}
		missMultiplier, _ := computePeakAwareMultipliers(multiKey, baseMultiplier, now, "gpt-5.6")
		if !approxEq(missMultiplier, baseMultiplier) {
			t.Fatalf("whitelist miss multiplier must stay at base: got %v, want %v", missMultiplier, baseMultiplier)
		}
	})

	t.Run("nil api key degrades to base multipliers", func(t *testing.T) {
		now := at(15, 30)
		tokenMultiplier, imageMultiplier := computePeakAwareMultipliers(nil, baseMultiplier, now, "deepseek-v4-flash")
		if !approxEq(tokenMultiplier, baseMultiplier) {
			t.Fatalf("nil group token multiplier: got %v, want %v", tokenMultiplier, baseMultiplier)
		}
		if !approxEq(imageMultiplier, baseMultiplier) {
			t.Fatalf("nil group image multiplier: got %v, want %v", imageMultiplier, baseMultiplier)
		}
	})
}

// TestPeakMultiplier_SnapshotRoundTrip 防回归：认证缓存快照（APIKeyAuthGroupSnapshot）
// 必须携带高峰倍率字段（含多窗口 peak_windows），否则扣费路径拿到的 apiKey.Group
// 会缺字段、PeakMultiplierAt 恒降级为 1.0。
// 调用真实链路 snapshotFromAPIKey → snapshotToAPIKey，验证 peak 配置经快照往返后仍生效。
func TestPeakMultiplier_SnapshotRoundTrip(t *testing.T) {
	apiKey := &APIKey{
		User: &User{ID: 1, Status: StatusActive, Role: RoleUser},
		Group: newMultiWindowGroup(true,
			PeakWindow{Start: "09:00", End: "12:00", Multiplier: 2.0, Models: []string{"deepseek-v4-flash"}},
		),
	}
	// 同时携带 legacy 字段，验证两条路径都随快照往返
	apiKey.Group.PeakStart = "14:00"
	apiKey.Group.PeakEnd = "18:00"
	apiKey.Group.PeakRateMultiplier = 3.0
	svc := &APIKeyService{}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	if snapshot == nil || snapshot.Group == nil {
		t.Fatalf("snapshot or snapshot.Group must not be nil")
	}
	restored := svc.snapshotToAPIKey("k", snapshot)
	if restored.Group == nil {
		t.Fatalf("restored.Group must not be nil")
	}

	if !restored.Group.PeakRateEnabled ||
		restored.Group.PeakStart != "14:00" ||
		restored.Group.PeakEnd != "18:00" ||
		restored.Group.PeakRateMultiplier != 3.0 {
		t.Fatalf("legacy peak fields lost in snapshot round-trip: %+v", restored.Group)
	}
	if len(restored.Group.PeakWindows) != 1 ||
		restored.Group.PeakWindows[0].Start != "09:00" ||
		restored.Group.PeakWindows[0].End != "12:00" ||
		restored.Group.PeakWindows[0].Multiplier != 2.0 ||
		len(restored.Group.PeakWindows[0].Models) != 1 ||
		restored.Group.PeakWindows[0].Models[0] != "deepseek-v4-flash" {
		t.Fatalf("peak_windows lost in snapshot round-trip: %+v", restored.Group.PeakWindows)
	}
	if got := restored.Group.PeakMultiplierAt(at(10, 0), "deepseek-v4-flash"); got != 2.0 {
		t.Fatalf("multi-window peak multiplier after round-trip: got %v, want 2.0", got)
	}
	if got := restored.Group.PeakMultiplierAt(at(10, 0), "gpt-5.6"); got != 1.0 {
		t.Fatalf("whitelist miss after round-trip: got %v, want 1.0", got)
	}
	if got := restored.Group.PeakMultiplierAt(at(20, 0), "deepseek-v4-flash"); got != 1.0 {
		t.Fatalf("off-peak multiplier after round-trip: got %v, want 1.0", got)
	}
}
