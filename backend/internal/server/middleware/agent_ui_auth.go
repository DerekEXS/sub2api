package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// AgentUISessionAuth Agent Web UI 会话认证中间件类型。
type AgentUISessionAuth gin.HandlerFunc

const (
	// agentUISessionCookieName 会话 cookie 名。
	agentUISessionCookieName = "cz_ui_session"
	// agentUISessionTTL 会话有效期（12h）。
	agentUISessionTTL = 12 * time.Hour
	// agentUISessionPath cookie 作用路径（只随 Agent UI 代理请求携带）。
	agentUISessionPath = "/api/v1/agent/ui"
)

// agentUISession 内存会话条目。
type agentUISession struct {
	userID int64
	exp    int64 // unix 秒
}

// agentUISessions 包内全局会话存储（sid → agentUISession）。
// 查询时惰性删除过期条目，不跑后台清理 goroutine（简单优先）。
var agentUISessions sync.Map

// NewAgentUISessionAuth 创建 Agent Web UI 会话认证中间件。
//
// 背景：Agent 服务用户页用 iframe 嵌入 PicoClaw WebUI（src=/api/v1/agent/ui/），
// iframe 内浏览器的请求不带 Authorization header（JWT 存前端 localStorage，
// 浏览器不会自动附加），而 JWT 中间件只认 Authorization: Bearer → 全部 401。
//
// 认证三通道（按序）：
//   - a. Authorization: Bearer <token> — 复用 jwtAuth 严格校验链路
//     （ValidateToken → GetByID → IsActive → TokenVersion → 会话绑定）。
//     会话绑定/审计依赖以 nil 注入：enforceSessionBinding 对 nil settingService
//     直接放行，不重复绑定校验（代理路径与面板同源同浏览器，绑定必然一致）。
//   - b. query 参数 cz_token — ValidateToken 通过后签发 HttpOnly cookie 会话
//     （随机 32 字节 hex，TTL 12h，SameSite=Lax，Secure=false 适配本地 http）。
//   - c. cookie cz_ui_session — 查内存会话，未过期且用户仍 active 即通过。
func NewAgentUISessionAuth(authService *service.AuthService, userService *service.UserService) AgentUISessionAuth {
	strict := jwtAuth(authService, userService, userService, nil, nil)
	return AgentUISessionAuth(func(c *gin.Context) {
		// 通道 a：Authorization: Bearer（与面板同款严格校验，失败即 401 abort）
		if strings.TrimSpace(c.GetHeader("Authorization")) != "" {
			strict(c)
			return
		}
		// 通道 b：?cz_token= 换会话 cookie。
		// cz_token 会随 iframe 各子请求（静态资源/WS 握手）反复出现在 query 中，
		// 已有有效 cookie 会话时直接复用，避免每次 mint 新 session 泄漏条目。
		if tok := strings.TrimSpace(c.Query("cz_token")); tok != "" {
			if sid, err := c.Cookie(agentUISessionCookieName); err == nil && resolveAgentUISession(c, userService, sid) {
				c.Next()
				return
			}
			if mintAgentUISession(c, authService, userService, tok) {
				c.Next()
				return
			}
			return // 401 已写并 abort
		}
		// 通道 c：cz_ui_session cookie
		if sid, err := c.Cookie(agentUISessionCookieName); err == nil {
			if resolveAgentUISession(c, userService, sid) {
				c.Next()
				return
			}
			AbortWithError(c, 401, "INVALID_SESSION", "Agent UI session is invalid or expired")
			return
		}
		AbortWithError(c, 401, "UNAUTHORIZED", "Authentication required")
	})
}

// mintAgentUISession 校验 cz_token，通过则签发会话 cookie 并设置 subject。
func mintAgentUISession(
	c *gin.Context,
	authService *service.AuthService,
	userService *service.UserService,
	tokenString string,
) bool {
	claims, err := authService.ValidateToken(tokenString)
	if err != nil {
		if errors.Is(err, service.ErrTokenExpired) {
			AbortWithError(c, 401, "TOKEN_EXPIRED", "Token has expired")
		} else {
			AbortWithError(c, 401, "INVALID_TOKEN", "Invalid token")
		}
		return false
	}
	user, err := userService.GetByID(c.Request.Context(), claims.UserID)
	if err != nil {
		AbortWithError(c, 401, "USER_NOT_FOUND", "User not found")
		return false
	}
	if !user.IsActive() {
		AbortWithError(c, 401, "USER_INACTIVE", "User account is not active")
		return false
	}
	if claims.TokenVersion != user.TokenVersion {
		AbortWithError(c, 401, "TOKEN_REVOKED", "Token has been revoked (password changed)")
		return false
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		AbortWithError(c, http.StatusInternalServerError, "SESSION_CREATE_FAILED", "Failed to create session")
		return false
	}
	sid := hex.EncodeToString(buf)
	agentUISessions.Store(sid, agentUISession{userID: user.ID, exp: time.Now().Add(agentUISessionTTL).Unix()})
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(agentUISessionCookieName, sid, int(agentUISessionTTL.Seconds()), agentUISessionPath, "", false, true)

	// 首个请求（带 cz_token 的 iframe 入口）也需要 subject，否则 handler 直接 401。
	setAgentUISubject(c, user)
	return true
}

// resolveAgentUISession 校验 cookie 对应的内存会话：未过期 + 用户存在且 active
// 则设置 subject 返回 true；否则惰性删除条目返回 false。
func resolveAgentUISession(c *gin.Context, userService *service.UserService, sid string) bool {
	if sid == "" {
		return false
	}
	entry, ok := agentUISessions.Load(sid)
	if !ok {
		return false
	}
	es := entry.(agentUISession)
	if time.Now().Unix() > es.exp {
		agentUISessions.Delete(sid)
		return false
	}
	user, err := userService.GetByID(c.Request.Context(), es.userID)
	if err != nil || !setAgentUISubject(c, user) {
		agentUISessions.Delete(sid)
		return false
	}
	return true
}

// setAgentUISubject 按 auth_subject.go 的 key 与 AuthSubject 结构写入认证身份。
func setAgentUISubject(c *gin.Context, user *service.User) bool {
	if user == nil || !user.IsActive() {
		return false
	}
	c.Set(string(ContextKeyUser), AuthSubject{
		UserID:      user.ID,
		Concurrency: user.Concurrency,
	})
	c.Set(string(ContextKeyUserRole), user.Role)
	c.Set(ContextKeyAuthEmail, user.Email)
	return true
}
