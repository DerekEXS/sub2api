package service

import (
	"context"
	"fmt"
	"net/netip"
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

// Evaluate 对一次注册请求做评分处置。
//   - L0 不通过时返回 (ErrBlocked, nil)，调用方应 403。
//   - 其余情况返回 (nil, *RegDecision)，调用方据此打标/放行。
//
// success 表示注册是否真正成功（评分只在注册成功路径计数）。
// invited 表示本次注册使用了有效邀请码（IP 计数信号豁免）。
func (g *RegistrationGuard) Evaluate(ctx context.Context, req RegEvaluateInput) (*RegDecision, error) {
	// ── L0 硬规则：UA ──
	if req.UserAgent == "" {
		return nil, ErrRegistrationBlocked
	}
	if !strings.Contains(req.UserAgent, "Mozilla") {
		return nil, ErrRegistrationBlocked
	}

	// ── CGNAT 豁免（不豁免 L0）──
	cgnat := isCGNAT(req.IP)

	decision := &RegDecision{
		UserID:  req.UserID,
		IP:      req.IP,
		Invited: req.Invited,
		Strong:  false,
	}

	// ── 邀请码豁免：IP 计数信号全置 0，仅保留邮箱 -10 ──
	inviteExempt := req.Invited

	score := 0

	// ── 邮箱形态 ──
	emailScore := scoreForEmail(req.Email)
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
		// 24h 分档
		switch {
		case c24 >= 6:
			score += 60
		case c24 >= 4:
			score += 30
		}
		// 10m 节奏
		if c10 >= 2 {
			score += 20
		}
	}

	// ── 处置 ──
	switch {
	case score >= 60:
		decision.Strong = true
	case score >= 40:
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
func (g *RegistrationGuard) IsBlockedUA(ua string) bool {
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

// scoreForEmail 邮箱形态评分。
func scoreForEmail(email string) int {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return 0
	}
	local := email[:at]
	domain := email[at+1:]
	score := 0

	// 本地部分 >=14 且元音占比 <15%（随机串特征）
	if len(local) >= 14 {
		vowels := 0
		for _, r := range local {
			if strings.ContainsRune("aeiou", r) {
				vowels++
			}
		}
		if len(local) > 0 && float64(vowels)/float64(len(local)) < 0.15 {
			score += 20
		}
	}
	// + 别名
	if strings.Contains(local, "+") {
		score += 20
	}
	// 白名单域名 -10
	for _, d := range EmailDomainWhitelist {
		if domain == strings.ToLower(d) {
			score -= 10
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
