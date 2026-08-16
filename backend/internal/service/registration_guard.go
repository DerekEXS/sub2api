package service

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ──────────────────────────────────────────────────────────────
// RegistrationGuard - 注册 IP 限制（多维评分，禁止单维封禁）
//
// 设计原则（主人规范 2026-08-15）：
//   - L0 硬规则：UA 为空/不含 "Mozilla" -> 403（curl/Go-http-client/python-requests 全拒）
//   - CGNAT 豁免：100.64.0.0/10 跳过 IP 计数（不豁免 L0）
//   - L1 评分：Redis 计数（24h / 10m 窗口）+ 邮箱形态 + 节奏
//   - L2 燃烧速率：未充值用户近 1h 消耗 > $5/h -> throttle 标记
//   - 绝不自动封禁、绝不拒绝注册（除 L0）；邀请码豁免 IP 计数；充值用户铁律豁免
//   - 40-59 打标、>=60 强标；写 Redis regflag:<user_id> 供审计
//
// Redis 注入：包级 setter（仿 SetWebSearchManager 模式），http.go 在启动时
// SetRegistrationGuardRedis(client)。未注入时 guard 只做 L0 检查（fail-open）。
// ──────────────────────────────────────────────────────────────

// EmailDomainWhitelist 默认邮箱域名白名单（settings 可覆盖）。
var EmailDomainWhitelist = []string{"qq.com", "163.com", "gmail.com"}

// regAuditConfigKey 是注册风险审计可配置参数的 Redis hash key（无 Redis 时用硬编码默认值）。
const regAuditConfigKey = "regaudit:config"

// RegAuditConfig 注册风险审计的可配置评分参数（主人 2026-08-16 规范）：
// 全部评分/阈值参数可配置，持久化在 Redis hash `regaudit:config`；
// 无 Redis（未注入）时使用 DefaultRegAuditConfig 硬编码默认值。
type RegAuditConfig struct {
	UaCheckEnabled      bool    `json:"ua_check_enabled"`        // L0 UA 硬规则开关
	CGNATExempt         bool    `json:"cgnat_exempt"`            // CGNAT IP 计数豁免开关
	FlagThreshold       int     `json:"flag_threshold"`          // 打标阈值（>= 打标）
	StrongThreshold     int     `json:"strong_threshold"`        // 强标阈值（>= 强标）
	EmailLongLocalMin   int     `json:"email_long_local_min"`    // 邮箱本地部分长度下限（随机串特征）
	EmailVowelRatioMax  float64 `json:"email_vowel_ratio_max"`   // 邮箱元音占比上限（随机串特征）
	ScoreEmailRandom    int     `json:"score_email_random"`      // 邮箱随机串形态分
	ScoreEmailAlias     int     `json:"score_email_alias"`       // 邮箱 + 别名分
	ScoreEmailWhitelist int     `json:"score_email_whitelist"`   // 白名单域名分（负值=减分）
	ScoreRhythm         int     `json:"score_rhythm"`            // 10m 节奏分（同 IP >=2 次）
	Score24h45          int     `json:"score_24h_4_5"`           // 24h 分档 4-5 次
	Score24h6Plus       int     `json:"score_24h_6_plus"`        // 24h 分档 >=6 次
}

// DefaultRegAuditConfig 返回硬编码默认值（与 Redis 是否可用无关）。
func DefaultRegAuditConfig() RegAuditConfig {
	return RegAuditConfig{
		UaCheckEnabled:      true,
		CGNATExempt:         true,
		FlagThreshold:       40,
		StrongThreshold:     60,
		EmailLongLocalMin:   14,
		EmailVowelRatioMax:  0.15,
		ScoreEmailRandom:    20,
		ScoreEmailAlias:     20,
		ScoreEmailWhitelist: -10,
		ScoreRhythm:         20,
		Score24h45:          30,
		Score24h6Plus:       60,
	}
}

// RegAuditConfigPatch 是审计配置部分更新请求（nil 字段不更新；支持全量或部分 body）。
type RegAuditConfigPatch struct {
	UaCheckEnabled      *bool    `json:"ua_check_enabled"`
	CGNATExempt         *bool    `json:"cgnat_exempt"`
	FlagThreshold       *int     `json:"flag_threshold"`
	StrongThreshold     *int     `json:"strong_threshold"`
	EmailLongLocalMin   *int     `json:"email_long_local_min"`
	EmailVowelRatioMax  *float64 `json:"email_vowel_ratio_max"`
	ScoreEmailRandom    *int     `json:"score_email_random"`
	ScoreEmailAlias     *int     `json:"score_email_alias"`
	ScoreEmailWhitelist *int     `json:"score_email_whitelist"`
	ScoreRhythm         *int     `json:"score_rhythm"`
	Score24h45          *int     `json:"score_24h_4_5"`
	Score24h6Plus       *int     `json:"score_24h_6_plus"`
}

// RedisCounter 抽象注册评分所需的 Redis 操作（接口化便于 mock）。
type RedisCounter interface {
	// Incr 对 key 自增并返回新值（自动 EXPIRE 由调用方负责）。
	Incr(ctx context.Context, key string) (int64, error)
	// Expire 设置 key 过期。
	Expire(ctx context.Context, key string, ttl time.Duration) error
	// HSet 写 hash 字段。
	HSet(ctx context.Context, key string, fields map[string]any) error
	// HGetAll 读 hash 全部字段。
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	// Keys 按模式列 key（审计端点用）。
	Keys(ctx context.Context, pattern string) ([]string, error)
	// Del 删除 key（审计清理用）。
	Del(ctx context.Context, keys ...string) error
}

// RedisCounterAdapter 把 *redis.Client 适配为 RedisCounter（server 层注入用）。
type RedisCounterAdapter struct {
	Client *redis.Client
}

func (a *RedisCounterAdapter) Incr(ctx context.Context, key string) (int64, error) {
	return a.Client.Incr(ctx, key).Result()
}

func (a *RedisCounterAdapter) Expire(ctx context.Context, key string, ttl time.Duration) error {
	_, err := a.Client.Expire(ctx, key, ttl).Result()
	return err
}

func (a *RedisCounterAdapter) HSet(ctx context.Context, key string, fields map[string]any) error {
	args := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}
	_, err := a.Client.HSet(ctx, key, args...).Result()
	return err
}

func (a *RedisCounterAdapter) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return a.Client.HGetAll(ctx, key).Result()
}

func (a *RedisCounterAdapter) Keys(ctx context.Context, pattern string) ([]string, error) {
	return a.Client.Keys(ctx, pattern).Result()
}

func (a *RedisCounterAdapter) Del(ctx context.Context, keys ...string) error {
	_, err := a.Client.Del(ctx, keys...).Result()
	return err
}

// registrationGuardPtr 存全局 guard（原子安全）。
var registrationGuardPtr atomic.Pointer[RegistrationGuard]

// SetRegistrationGuardRedis 注入 redis 到全局 guard（启动时调用；nil 则禁用 L1/L2）。
func SetRegistrationGuardRedis(r RedisCounter) {
	g := newRegistrationGuard(r)
	registrationGuardPtr.Store(g)
}

// GetRegistrationGuard 返回全局 guard；未注入时返回仅 L0 的默认实例。
func GetRegistrationGuard() *RegistrationGuard {
	if g := registrationGuardPtr.Load(); g != nil {
		return g
	}
	g := newRegistrationGuard(nil)
	registrationGuardPtr.Store(g)
	return g
}

// RegistrationGuard 实现多维注册评分。
type RegistrationGuard struct {
	redis     RedisCounter // 可能为 nil（未注入 -> 跳过 L1/L2）
	whitelist map[string]struct{}
}

func newRegistrationGuard(r RedisCounter) *RegistrationGuard {
	wl := make(map[string]struct{}, len(EmailDomainWhitelist))
	for _, d := range EmailDomainWhitelist {
		wl[strings.ToLower(d)] = struct{}{}
	}
	return &RegistrationGuard{redis: r, whitelist: wl}
}

// GetAuditConfig 返回当前生效的审计配置。每次读取 Redis hash `regaudit:config`
// （注册路径低频，读取开销可忽略）；无 Redis 或读失败时返回硬编码默认值。
func (g *RegistrationGuard) GetAuditConfig(ctx context.Context) RegAuditConfig {
	cfg := DefaultRegAuditConfig()
	if g.redis == nil {
		return cfg
	}
	fields, err := g.redis.HGetAll(ctx, regAuditConfigKey)
	if err != nil || len(fields) == 0 {
		return cfg
	}
	applyRegAuditFields(&cfg, fields)
	return cfg
}

// UpdateAuditConfig 部分更新审计配置（非 nil 字段生效），返回更新后的完整配置。
// 有 Redis 时把全量字段写入 regaudit:config hash；无 Redis 时仅返回合并结果（不持久化）。
func (g *RegistrationGuard) UpdateAuditConfig(ctx context.Context, patch RegAuditConfigPatch) (RegAuditConfig, error) {
	cfg := g.GetAuditConfig(ctx)
	if patch.UaCheckEnabled != nil {
		cfg.UaCheckEnabled = *patch.UaCheckEnabled
	}
	if patch.CGNATExempt != nil {
		cfg.CGNATExempt = *patch.CGNATExempt
	}
	if patch.FlagThreshold != nil {
		cfg.FlagThreshold = *patch.FlagThreshold
	}
	if patch.StrongThreshold != nil {
		cfg.StrongThreshold = *patch.StrongThreshold
	}
	if patch.EmailLongLocalMin != nil {
		cfg.EmailLongLocalMin = *patch.EmailLongLocalMin
	}
	if patch.EmailVowelRatioMax != nil {
		cfg.EmailVowelRatioMax = *patch.EmailVowelRatioMax
	}
	if patch.ScoreEmailRandom != nil {
		cfg.ScoreEmailRandom = *patch.ScoreEmailRandom
	}
	if patch.ScoreEmailAlias != nil {
		cfg.ScoreEmailAlias = *patch.ScoreEmailAlias
	}
	if patch.ScoreEmailWhitelist != nil {
		cfg.ScoreEmailWhitelist = *patch.ScoreEmailWhitelist
	}
	if patch.ScoreRhythm != nil {
		cfg.ScoreRhythm = *patch.ScoreRhythm
	}
	if patch.Score24h45 != nil {
		cfg.Score24h45 = *patch.Score24h45
	}
	if patch.Score24h6Plus != nil {
		cfg.Score24h6Plus = *patch.Score24h6Plus
	}
	if g.redis != nil {
		if err := g.redis.HSet(ctx, regAuditConfigKey, regAuditFields(cfg)); err != nil {
			return cfg, fmt.Errorf("registration guard persist config: %w", err)
		}
	}
	return cfg, nil
}

// applyRegAuditFields 把 Redis hash 的字符串字段解析进 cfg（未知/非法字段跳过，保持原值）。
func applyRegAuditFields(cfg *RegAuditConfig, fields map[string]string) {
	get := func(k string) (string, bool) {
		v, ok := fields[k]
		return v, ok && v != ""
	}
	if v, ok := get("ua_check_enabled"); ok {
		cfg.UaCheckEnabled = v == "true"
	}
	if v, ok := get("cgnat_exempt"); ok {
		cfg.CGNATExempt = v == "true"
	}
	if v, ok := get("flag_threshold"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.FlagThreshold = n
		}
	}
	if v, ok := get("strong_threshold"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.StrongThreshold = n
		}
	}
	if v, ok := get("email_long_local_min"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.EmailLongLocalMin = n
		}
	}
	if v, ok := get("email_vowel_ratio_max"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.EmailVowelRatioMax = f
		}
	}
	if v, ok := get("score_email_random"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ScoreEmailRandom = n
		}
	}
	if v, ok := get("score_email_alias"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ScoreEmailAlias = n
		}
	}
	if v, ok := get("score_email_whitelist"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ScoreEmailWhitelist = n
		}
	}
	if v, ok := get("score_rhythm"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ScoreRhythm = n
		}
	}
	if v, ok := get("score_24h_4_5"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Score24h45 = n
		}
	}
	if v, ok := get("score_24h_6_plus"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Score24h6Plus = n
		}
	}
}

// regAuditFields 把配置序列化为 Redis hash 字符串字段（全量写，读时按需解析）。
func regAuditFields(cfg RegAuditConfig) map[string]any {
	return map[string]any{
		"ua_check_enabled":       boolStr(cfg.UaCheckEnabled),
		"cgnat_exempt":           boolStr(cfg.CGNATExempt),
		"flag_threshold":         strconv.Itoa(cfg.FlagThreshold),
		"strong_threshold":       strconv.Itoa(cfg.StrongThreshold),
		"email_long_local_min":   strconv.Itoa(cfg.EmailLongLocalMin),
		"email_vowel_ratio_max":  strconv.FormatFloat(cfg.EmailVowelRatioMax, 'f', -1, 64),
		"score_email_random":     strconv.Itoa(cfg.ScoreEmailRandom),
		"score_email_alias":      strconv.Itoa(cfg.ScoreEmailAlias),
		"score_email_whitelist":  strconv.Itoa(cfg.ScoreEmailWhitelist),
		"score_rhythm":           strconv.Itoa(cfg.ScoreRhythm),
		"score_24h_4_5":          strconv.Itoa(cfg.Score24h45),
		"score_24h_6_plus":       strconv.Itoa(cfg.Score24h6Plus),
	}
}

// Evaluate 对一次注册请求做评分处置。
//   - L0 不通过时返回 (ErrBlocked, nil)，调用方应 403。
//   - 其余情况返回 (nil, *RegDecision)，调用方据此打标/放行。
//
// success 表示注册是否真正成功（评分只在注册成功路径计数）。
// invited 表示本次注册使用了有效邀请码（IP 计数信号豁免）。
func (g *RegistrationGuard) Evaluate(ctx context.Context, req RegEvaluateInput) (*RegDecision, error) {
	cfg := g.GetAuditConfig(ctx)

	// ── L0 硬规则：UA（受 ua_check_enabled 控制）──
	if cfg.UaCheckEnabled {
		if req.UserAgent == "" {
			return nil, ErrRegistrationBlocked
		}
		if !strings.Contains(req.UserAgent, "Mozilla") {
			return nil, ErrRegistrationBlocked
		}
	}

	// ── CGNAT 豁免（不豁免 L0；受 cgnat_exempt 控制）──
	cgnat := cfg.CGNATExempt && isCGNAT(req.IP)

	decision := &RegDecision{
		UserID:  req.UserID,
		IP:      req.IP,
		Invited: req.Invited,
		Strong:  false,
	}

	// ── 邀请码豁免：IP 计数信号全置 0，仅保留邮箱白名单分 ──
	inviteExempt := req.Invited

	score := 0

	// ── 邮箱形态（分值/阈值取自配置）──
	emailScore := scoreForEmail(req.Email, cfg)
	score += emailScore

	// ── L1 Redis 评分（仅当注入 redis 且非 CGNAT 且非邀请豁免）──
	if g.redis != nil && !cgnat && !inviteExempt {
		ip := req.IP
		c24, err := g.redis.Incr(ctx, "reg:ip:24h:"+ip)
		if err == nil {
			_ = g.redis.Expire(ctx, "reg:ip:24h:"+ip, 24*time.Hour)
		}
		c10, err10 := g.redis.Incr(ctx, "reg:ip:10m:"+ip)
		if err10 == nil {
			_ = g.redis.Expire(ctx, "reg:ip:10m:"+ip, 10*time.Minute)
		}
		// 24h 分档（分值取自配置）
		switch {
		case c24 >= 6:
			score += cfg.Score24h6Plus
		case c24 >= 4:
			score += cfg.Score24h45
		}
		// 10m 节奏（分值取自配置）
		if c10 >= 2 {
			score += cfg.ScoreRhythm
		}
	}

	// ── 处置（阈值取自配置）──
	switch {
	case score >= cfg.StrongThreshold:
		decision.Strong = true
	case score >= cfg.FlagThreshold:
		// 打标
	default:
		// 放行
	}

	// 写 flag（所有成功注册都写，含邀请者 invited=1）
	if g.redis != nil && req.UserID > 0 {
		fields := map[string]any{
			"user_id":    fmt.Sprintf("%d", req.UserID),
			"score":      fmt.Sprintf("%d", score),
			"strong":     boolStr(decision.Strong),
			"invited":    boolStr(req.Invited),
			"ip":         req.IP,
			"created_at": fmt.Sprintf("%d", time.Now().Unix()),
		}
		_ = g.redis.HSet(ctx, fmt.Sprintf("regflag:%d", req.UserID), fields)
		_ = g.redis.Expire(ctx, fmt.Sprintf("regflag:%d", req.UserID), 7*24*time.Hour)
	}
	decision.Score = score
	return decision, nil
}

// IsBlockedUA 暴露 L0 判定（handler 可在验证码前调用）。
// 受 ua_check_enabled 配置控制：关闭时始终放行。
func (g *RegistrationGuard) IsBlockedUA(ua string) bool {
	if !g.GetAuditConfig(context.Background()).UaCheckEnabled {
		return false
	}
	if ua == "" {
		return true
	}
	return !strings.Contains(ua, "Mozilla")
}

// CheckBurnRate 检查未充值用户近 1h 燃烧速率（L2）。
// 由定时任务调用；返回被 throttle 的用户数。redis 未注入时 no-op。
//
// 注意：本实现依赖外部提供近 1h 消耗数据（usage repo 查询），这里以
// costPerUserFn 注入解耦（见集成清单）。nil 时仅做标记接口（TODO 留待接入）。
func (g *RegistrationGuard) CheckBurnRate(ctx context.Context, costPerUserFn func(context.Context) (map[int64]float64, error)) (int, error) {
	if g.redis == nil {
		return 0, nil
	}
	if costPerUserFn == nil {
		return 0, nil // 数据源未接入，no-op
	}
	costs, err := costPerUserFn(ctx)
	if err != nil {
		return 0, fmt.Errorf("registration guard burn rate: %w", err)
	}
	throttled := 0
	for userID, cost := range costs {
		if cost > 5.0 { // $5/h
			_ = g.redis.HSet(ctx, fmt.Sprintf("throttle:%d", userID), map[string]any{"throttle": "1", "cost": fmt.Sprintf("%.2f", cost)})
			_ = g.redis.Expire(ctx, fmt.Sprintf("throttle:%d", userID), time.Hour)
			throttled++
		}
	}
	return throttled, nil
}

// ListAudit 返回 regflag:* 全部记录（审计端点）。
func (g *RegistrationGuard) ListAudit(ctx context.Context) ([]RegDecision, error) {
	if g.redis == nil {
		return nil, nil
	}
	keys, err := g.redis.Keys(ctx, "regflag:*")
	if err != nil {
		return nil, err
	}
	out := make([]RegDecision, 0, len(keys))
	for _, k := range keys {
		if !strings.HasPrefix(k, "regflag:") { // 排除 regaudit:config 等非 flag 键
			continue
		}
		fields, err := g.redis.HGetAll(ctx, k)
		if err != nil {
			continue
		}
		uid := strings.TrimPrefix(k, "regflag:")
		out = append(out, RegDecision{
			UserID:    atoi64(uid),
			Score:     int(atoi64(fields["score"])),
			Strong:    fields["strong"] == "true",
			Invited:   fields["invited"] == "true",
			IP:        fields["ip"],
			CreatedAt: fields["created_at"],
		})
	}
	return out, nil
}

// RegEvaluateInput 单次注册评估输入。
type RegEvaluateInput struct {
	UserID    int64
	IP        string
	Email     string
	UserAgent string
	Invited   bool
	Recharged bool // 充值用户铁律豁免（预留：供处置端判断）
}

// RegDecision 评分结果。
type RegDecision struct {
	UserID    int64  `json:"user_id"`
	Email     string `json:"email,omitempty"` // handler 层按 UserID 补全（#328：跳转/定位用邮箱）
	Score     int    `json:"score"`
	Strong    bool   `json:"strong"`
	Invited   bool   `json:"invited"`
	IP        string `json:"ip"`
	CreatedAt string `json:"created_at,omitempty"`
}

// ErrRegistrationBlocked 是 L0 硬拒错误。
var ErrRegistrationBlocked = fmt.Errorf("register blocked by UA policy")

// isCGNAT 判断 IP 是否落在 CGNAT 保留段 100.64.0.0/10。
func isCGNAT(ip string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	prefix := netip.MustParsePrefix("100.64.0.0/10")
	return prefix.Contains(addr)
}

// scoreForEmail 邮箱形态评分（阈值/分值取自可配置审计参数 RegAuditConfig）。
func scoreForEmail(email string, cfg RegAuditConfig) int {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return 0
	}
	local := email[:at]
	domain := email[at+1:]
	score := 0

	// 本地部分 >=email_long_local_min 且元音占比 <email_vowel_ratio_max（随机串特征）
	if len(local) >= cfg.EmailLongLocalMin {
		vowels := 0
		for _, r := range local {
			if strings.ContainsRune("aeiou", r) {
				vowels++
			}
		}
		if len(local) > 0 && float64(vowels)/float64(len(local)) < cfg.EmailVowelRatioMax {
			score += cfg.ScoreEmailRandom
		}
	}
	// + 别名
	if strings.Contains(local, "+") {
		score += cfg.ScoreEmailAlias
	}
	// 白名单域名（分值取自配置，默认 -10）
	for _, d := range EmailDomainWhitelist {
		if domain == strings.ToLower(d) {
			score += cfg.ScoreEmailWhitelist
			break
		}
	}
	return score
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func atoi64(s string) int64 {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}
