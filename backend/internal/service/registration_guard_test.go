package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────
// RegistrationGuard 单测（mock redis counter）
// 覆盖: UA 拒绝矩阵 / CGNAT 豁免 / 24h 分档边界(3,4,5,6) /
//       邀请豁免 / 评分组合恰达 40 与 60 边界 / 10m 节奏 / 审计
// ──────────────────────────────────────────────────────────────

type mockRedisCounter struct {
	mu     sync.Mutex
	counts map[string]int64
	hashes map[string]map[string]string
}

func newMockRedisCounter() *mockRedisCounter {
	return &mockRedisCounter{counts: map[string]int64{}, hashes: map[string]map[string]string{}}
}

func (m *mockRedisCounter) Incr(ctx context.Context, key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[key]++
	return m.counts[key], nil
}

func (m *mockRedisCounter) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return nil
}

func (m *mockRedisCounter) HSet(ctx context.Context, key string, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hashes[key] == nil {
		m.hashes[key] = map[string]string{}
	}
	for k, v := range fields {
		m.hashes[key][k] = toString(v)
	}
	return nil
}

func (m *mockRedisCounter) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.hashes[key] {
		out[k] = v
	}
	return out, nil
}

func (m *mockRedisCounter) Keys(ctx context.Context, pattern string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.hashes {
		keys = append(keys, k)
	}
	return keys, nil
}

func (m *mockRedisCounter) Del(ctx context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.counts, k)
		delete(m.hashes, k)
	}
	return nil
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// countFor 获取某 IP 已注册次数（24h key）。
func (m *mockRedisCounter) countFor(ip string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts["reg:ip:24h:"+ip]
}

func newTestGuard(r *mockRedisCounter) *RegistrationGuard {
	g := newRegistrationGuard(r)
	return g
}

// ── UA 拒绝矩阵 ──

func TestRegistrationGuardUAReject(t *testing.T) {
	// 用真实 mock（避免 typed-nil 接口陷阱）；UA 拒绝在 L0 层不触 redis
	g := newTestGuard(newMockRedisCounter())
	ctx := context.Background()

	cases := []struct {
		ua      string
		blocked bool
	}{
		{"", true},                       // 空 UA
		{"curl/8.5.0", true},             // curl
		{"Go-http-client/1.1", true},     // Go http client
		{"python-requests/2.31.0", true}, // python
		{"Wget/1.21.4", true},            // wget
		{"java/17.0.2", true},            // java
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36", false}, // 合法浏览器
	}
	for _, tc := range cases {
		got := g.IsBlockedUA(tc.ua)
		if got != tc.blocked {
			t.Errorf("IsBlockedUA(%q) = %v, want %v", tc.ua, got, tc.blocked)
		}
		_, err := g.Evaluate(ctx, RegEvaluateInput{UserAgent: tc.ua, IP: "1.2.3.4"})
		if tc.blocked && !errors.Is(err, ErrRegistrationBlocked) {
			t.Errorf("Evaluate UA=%q: err = %v, want ErrRegistrationBlocked", tc.ua, err)
		}
		if !tc.blocked && err != nil {
			t.Errorf("Evaluate UA=%q: unexpected err %v", tc.ua, err)
		}
	}
}

// ── CGNAT 豁免 ──

func TestRegistrationGuardCGNATExempt(t *testing.T) {
	r := newMockRedisCounter()
	g := newTestGuard(r)
	ctx := context.Background()

	// CGNAT IP 连续注册 10 次，不产生 IP 计数信号
	ip := "100.64.5.6"
	for i := 0; i < 10; i++ {
		_, err := g.Evaluate(ctx, RegEvaluateInput{UserID: int64(100 + i), IP: ip, Email: "x@qq.com", UserAgent: "Mozilla/5.0 test"})
		if err != nil {
			t.Fatalf("CGNAT evaluate %d: %v", i, err)
		}
	}
	if c := r.countFor(ip); c != 0 {
		t.Fatalf("CGNAT ip count = %d, want 0 (exempt)", c)
	}
}

// ── 24h 分档边界 ──

func TestRegistrationGuard24hBanding(t *testing.T) {
	// 验证 24h 分档: 3次=0, 4-5次=+30, >=6次=+60。
	// 邮箱用白名单外中性（本地短无+ -> 邮箱分 0），使分数纯粹来自 IP 分档。
	r := newMockRedisCounter()
	g := newTestGuard(r)
	ctx := context.Background()

	// 期望第 N 次注册的档位：断言那次 Evaluate 也算一次计数。
	// count=3：24h 档 0 + 10m 节奏 20 = 20（无强标）
	for i := 1; i <= 2; i++ {
		_, _ = g.Evaluate(ctx, RegEvaluateInput{UserID: int64(i), IP: "8.8.8.8", Email: "ab@example.org", UserAgent: "Mozilla/5.0 x"})
	}
	d3, _ := g.Evaluate(ctx, RegEvaluateInput{UserID: 3, IP: "8.8.8.8", Email: "ab@example.org", UserAgent: "Mozilla/5.0 x"})
	if d3.Score != 20 {
		t.Fatalf("count=3 score=%d, want 20 (24h band 0 + 10m 20)", d3.Score)
	}
	if d3.Strong {
		t.Fatalf("count=3 strong=%v, want false", d3.Strong)
	}

	// count=5：24h 档 30 + 10m 20 = 50
	r2 := newMockRedisCounter()
	g2 := newTestGuard(r2)
	ip2 := "9.9.9.9"
	for i := 1; i <= 4; i++ {
		_, _ = g2.Evaluate(ctx, RegEvaluateInput{UserID: int64(200 + i), IP: ip2, Email: "ab@example.org", UserAgent: "Mozilla/5.0 x"})
	}
	d5, _ := g2.Evaluate(ctx, RegEvaluateInput{UserID: 205, IP: ip2, Email: "ab@example.org", UserAgent: "Mozilla/5.0 x"})
	if d5.Score != 50 {
		t.Fatalf("count=5 score=%d, want 50 (24h 30 + 10m 20)", d5.Score)
	}

	// count=6：24h 档 60 + 10m 20 = 80 强标
	r3 := newMockRedisCounter()
	g3 := newTestGuard(r3)
	ip3 := "7.7.7.7"
	for i := 1; i <= 5; i++ {
		_, _ = g3.Evaluate(ctx, RegEvaluateInput{UserID: int64(300 + i), IP: ip3, Email: "ab@example.org", UserAgent: "Mozilla/5.0 x"})
	}
	d6, _ := g3.Evaluate(ctx, RegEvaluateInput{UserID: 306, IP: ip3, Email: "ab@example.org", UserAgent: "Mozilla/5.0 x"})
	if d6.Score != 80 || !d6.Strong {
		t.Fatalf("count=6 score=%d strong=%v, want 80 strong", d6.Score, d6.Strong)
	}
}

// ── 邀请豁免 ──

func TestRegistrationGuardInviteExempt(t *testing.T) {
	r := newMockRedisCounter()
	g := newTestGuard(r)
	ctx := context.Background()

	// 同一 IP 用邀请码注册 8 次：不产生 IP 计数信号（仅 -10 邮箱白名单）
	for i := 1; i <= 8; i++ {
		_, err := g.Evaluate(ctx, RegEvaluateInput{UserID: int64(400 + i), IP: "6.6.6.6", Email: "ab@qq.com", UserAgent: "Mozilla/5.0 x", Invited: true})
		if err != nil {
			t.Fatalf("invite evaluate %d: %v", i, err)
		}
	}
	if c := r.countFor("6.6.6.6"); c != 0 {
		t.Fatalf("invited ip count = %d, want 0 (exempt)", c)
	}
	// 邀请豁免者也写 flag invited=1
	if _, ok := r.hashes["regflag:408"]; !ok {
		t.Fatal("invited user should still write regflag (invited=1)")
	}
	if r.hashes["regflag:408"]["invited"] != "true" {
		t.Fatalf("regflag invited = %q, want true", r.hashes["regflag:408"]["invited"])
	}
}

// ── 评分组合恰达 40 边界 ──

func TestRegistrationGuardScoreBoundary40(t *testing.T) {
	r := newMockRedisCounter()
	g := newTestGuard(r)
	ctx := context.Background()

	// 构造 40 分: 24h 计数 4-5 (+30) + 10m 节奏 (+20) - 邮箱白名单 10 = 40
	// 4 次注册同一 IP，最后一次 10m 计数>=2
	for i := 1; i <= 4; i++ {
		d, err := g.Evaluate(ctx, RegEvaluateInput{UserID: int64(500 + i), IP: "5.5.5.5", Email: "ab@qq.com", UserAgent: "Mozilla/5.0 x"})
		if err != nil {
			t.Fatalf("evaluate %d: %v", i, err)
		}
		if i == 4 {
			// score = 30 (band 4-5) + 20 (10m>=2) - 10 (whitelist) = 40 -> 打标
			if d.Score < 40 {
				t.Fatalf("boundary40: score=%d want >=40", d.Score)
			}
			if d.Strong {
				t.Fatalf("boundary40: score=%d strong=%v want NOT strong", d.Score, d.Strong)
			}
		}
	}
}

// ── 邮箱形态 ──

func TestRegistrationGuardEmailShape(t *testing.T) {
	r := newMockRedisCounter()
	g := newTestGuard(r)
	ctx := context.Background()

	// 随机长串邮箱（无白名单、含 + 别名、元音少）
	d, err := g.Evaluate(ctx, RegEvaluateInput{UserID: 600, IP: "4.4.4.4", Email: "xqkzjbvnrmlpasdf+1@example.org", UserAgent: "Mozilla/5.0 x"})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// 长本地 + 元音少 (+20) + 含 + (+20) = 40 -> 打标（无 IP 信号 +0）
	if d.Score < 40 {
		t.Fatalf("email shape score=%d, want >=40 (random+plus)", d.Score)
	}
}

// ── 审计列表 ──

func TestRegistrationGuardAudit(t *testing.T) {
	r := newMockRedisCounter()
	g := newTestGuard(r)
	ctx := context.Background()

	_, _ = g.Evaluate(ctx, RegEvaluateInput{UserID: 700, IP: "3.3.3.3", Email: "ab@qq.com", UserAgent: "Mozilla/5.0 x"})
	_, _ = g.Evaluate(ctx, RegEvaluateInput{UserID: 701, IP: "3.3.3.3", Email: "ab@qq.com", UserAgent: "Mozilla/5.0 x", Invited: true})

	audit, err := g.ListAudit(ctx)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(audit) < 2 {
		t.Fatalf("audit len = %d, want >=2", len(audit))
	}
	// 701 invited
	found := false
	for _, a := range audit {
		if a.UserID == 701 && a.Invited {
			found = true
		}
	}
	if !found {
		t.Fatal("audit should include invited=1 for user 701")
	}
}

// ── L2 燃烧速率 ──

func TestRegistrationGuardBurnRate(t *testing.T) {
	r := newMockRedisCounter()
	g := newTestGuard(r)
	ctx := context.Background()

	costs := func(context.Context) (map[int64]float64, error) {
		return map[int64]float64{42: 6.0, 43: 0.5}, nil
	}
	n, err := g.CheckBurnRate(ctx, costs)
	if err != nil {
		t.Fatalf("burn rate: %v", err)
	}
	if n != 1 {
		t.Fatalf("throttled = %d, want 1 (only user 42 > $5/h)", n)
	}
	if _, ok := r.hashes["throttle:42"]; !ok {
		t.Fatal("throttle:42 should exist")
	}
	if _, ok := r.hashes["throttle:43"]; ok {
		t.Fatal("throttle:43 should NOT exist (cost 0.5 < 5)")
	}
}

// ── 审计配置（可配置评分参数 regaudit:config）──

func TestRegistrationAuditConfigDefaults(t *testing.T) {
	// 无 redis -> 硬编码默认值
	g := newRegistrationGuard(nil)
	cfg := g.GetAuditConfig(context.Background())
	if cfg != DefaultRegAuditConfig() {
		t.Fatalf("default config = %+v, want %+v", cfg, DefaultRegAuditConfig())
	}
	// 有 redis 但 hash 空 -> 默认值
	r := newMockRedisCounter()
	g2 := newTestGuard(r)
	if cfg2 := g2.GetAuditConfig(context.Background()); cfg2 != DefaultRegAuditConfig() {
		t.Fatalf("empty-hash config = %+v, want defaults", cfg2)
	}
}

func TestRegistrationAuditConfigRoundTrip(t *testing.T) {
	r := newMockRedisCounter()
	g := newTestGuard(r)
	ctx := context.Background()

	// 初始 = 默认
	cfg := g.GetAuditConfig(ctx)
	if cfg.FlagThreshold != 40 || cfg.Score24h6Plus != 60 || cfg.ScoreEmailWhitelist != -10 {
		t.Fatalf("initial config = %+v", cfg)
	}

	// 部分更新：只改两个字段，其余保持默认
	flag, strong := 55, 80
	upd, err := g.UpdateAuditConfig(ctx, RegAuditConfigPatch{FlagThreshold: &flag, StrongThreshold: &strong})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.FlagThreshold != 55 || upd.StrongThreshold != 80 || upd.ScoreRhythm != 20 || !upd.UaCheckEnabled || upd.EmailVowelRatioMax != 0.15 {
		t.Fatalf("updated config = %+v", upd)
	}

	// hash 持久化 + 新 guard 重新读取（模拟重启后）
	g2 := newTestGuard(r)
	cfg2 := g2.GetAuditConfig(ctx)
	if cfg2.FlagThreshold != 55 || cfg2.StrongThreshold != 80 || cfg2.ScoreEmailAlias != 20 || cfg2.EmailVowelRatioMax != 0.15 {
		t.Fatalf("reloaded config = %+v", cfg2)
	}

	// 布尔/浮点字段更新
	ua, ratio := false, 0.25
	upd2, err := g2.UpdateAuditConfig(ctx, RegAuditConfigPatch{UaCheckEnabled: &ua, EmailVowelRatioMax: &ratio})
	if err != nil {
		t.Fatalf("update2: %v", err)
	}
	if upd2.UaCheckEnabled || upd2.EmailVowelRatioMax != 0.25 || upd2.FlagThreshold != 55 {
		t.Fatalf("updated2 = %+v", upd2)
	}

	// 负值分数字段（白名单 -10）也正确往返
	wl := -5
	upd3, err := g2.UpdateAuditConfig(ctx, RegAuditConfigPatch{ScoreEmailWhitelist: &wl})
	if err != nil {
		t.Fatalf("update3: %v", err)
	}
	if upd3.ScoreEmailWhitelist != -5 {
		t.Fatalf("updated3 whitelist score = %d, want -5", upd3.ScoreEmailWhitelist)
	}
	cfg3 := newTestGuard(r).GetAuditConfig(ctx)
	if cfg3.ScoreEmailWhitelist != -5 {
		t.Fatalf("reloaded3 whitelist score = %d, want -5", cfg3.ScoreEmailWhitelist)
	}
}

func TestRegistrationAuditConfigEffects(t *testing.T) {
	ctx := context.Background()

	// ua_check_enabled=false：curl UA 不再被拒
	r := newMockRedisCounter()
	g := newTestGuard(r)
	ua := false
	if _, err := g.UpdateAuditConfig(ctx, RegAuditConfigPatch{UaCheckEnabled: &ua}); err != nil {
		t.Fatalf("update ua: %v", err)
	}
	if g.IsBlockedUA("curl/8.5.0") {
		t.Fatal("ua_check_enabled=false: curl UA should pass IsBlockedUA")
	}
	if _, err := g.Evaluate(ctx, RegEvaluateInput{UserAgent: "curl/8.5.0", IP: "1.2.3.4"}); err != nil {
		t.Fatalf("Evaluate with ua_check_enabled=false: %v", err)
	}

	// cgnat_exempt=false：CGNAT IP 参与计数
	r2 := newMockRedisCounter()
	g2 := newTestGuard(r2)
	cg := false
	if _, err := g2.UpdateAuditConfig(ctx, RegAuditConfigPatch{CGNATExempt: &cg}); err != nil {
		t.Fatalf("update cgnat: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := g2.Evaluate(ctx, RegEvaluateInput{UserID: int64(800 + i), IP: "100.64.5.6", UserAgent: "Mozilla/5.0 x"}); err != nil {
			t.Fatalf("cgnat-off evaluate %d: %v", i, err)
		}
	}
	if c := r2.countFor("100.64.5.6"); c != 3 {
		t.Fatalf("cgnat_exempt=false ip count = %d, want 3", c)
	}

	// strong_threshold 下调：40 分邮箱形态即强标
	r3 := newMockRedisCounter()
	g3 := newTestGuard(r3)
	st := 30
	if _, err := g3.UpdateAuditConfig(ctx, RegAuditConfigPatch{StrongThreshold: &st}); err != nil {
		t.Fatalf("update threshold: %v", err)
	}
	d3, err := g3.Evaluate(ctx, RegEvaluateInput{UserID: 900, IP: "4.4.4.4", Email: "xqkzjbvnrmlpasdf+1@example.org", UserAgent: "Mozilla/5.0 x"})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d3.Score != 40 || !d3.Strong {
		t.Fatalf("score=%d strong=%v, want 40/strong (strong_threshold=30)", d3.Score, d3.Strong)
	}

	// flag_threshold 上调：40 分不再打标/强标
	r4 := newMockRedisCounter()
	g4 := newTestGuard(r4)
	ft := 50
	if _, err := g4.UpdateAuditConfig(ctx, RegAuditConfigPatch{FlagThreshold: &ft}); err != nil {
		t.Fatalf("update flag threshold: %v", err)
	}
	d4, err := g4.Evaluate(ctx, RegEvaluateInput{UserID: 901, IP: "4.4.4.5", Email: "xqkzjbvnrmlpasdf+1@example.org", UserAgent: "Mozilla/5.0 x"})
	if err != nil {
		t.Fatalf("evaluate2: %v", err)
	}
	if d4.Score != 40 || d4.Strong {
		t.Fatalf("score=%d strong=%v, want 40/not-strong (flag_threshold=50)", d4.Score, d4.Strong)
	}
}

// ── isCGNAT ──

func TestIsCGNAT(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"100.64.0.1", true},
		{"100.127.255.254", true},
		{"100.63.255.255", false},
		{"100.128.0.1", false},
		{"8.8.8.8", false},
		{"not-an-ip", false},
	}
	for _, tc := range cases {
		if got := isCGNAT(tc.ip); got != tc.want {
			t.Errorf("isCGNAT(%q) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}
