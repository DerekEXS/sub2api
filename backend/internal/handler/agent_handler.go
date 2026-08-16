package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// AgentHandler 用户 Agent 服务（PicoClaw 容器一键部署/销毁，薄代理到 NY manager）。
// 用户级鉴权：仅操作当前登录用户自己的实例（manager 按 user_id 隔离）。
type AgentHandler struct {
	agentService *service.AgentService
	cfg          *config.Config
}

func NewAgentHandler(agentService *service.AgentService, cfg *config.Config) *AgentHandler {
	return &AgentHandler{agentService: agentService, cfg: cfg}
}

// Start POST /api/v1/agent/start — 启动（创建用户专属 key + 调 manager create）
func (h *AgentHandler) Start(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	state, err := h.agentService.StartAgent(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}

// Stop POST /api/v1/agent/stop — 关闭（销毁容器 + 吊销 key + 清库，幂等）
func (h *AgentHandler) Stop(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	if err := h.agentService.StopAgent(c.Request.Context(), subject.UserID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"status": "stopped"})
}

// Status GET /api/v1/agent/status — 查询当前用户实例状态
func (h *AgentHandler) Status(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	state, err := h.agentService.GetAgentStatus(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}

// Archive GET /api/v1/agent/archive — 流式下载当前用户实例归档 tar.gz
func (h *AgentHandler) Archive(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	r, err := h.agentService.DownloadArchive(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", `attachment; filename="agent-`+strconv.FormatInt(subject.UserID, 10)+`.tar.gz"`)
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, r) // 流式透传，不缓冲
}

// UI GET /api/v1/agent/ui + /api/v1/agent/ui/*path — 用户实例 Web UI 反向代理。
//
// 目标 = AGENT_UI_VHOST_URL（manager vhost 入口，如 http://127.0.0.1:28801），
// Host 头改写为 agent-<userID>.agent.cloudzone-api.cyou（vhost 按 Host 路由到实例端口），
// 原路径原样透传。WebSocket（如实例内终端）由 httputil.ReverseProxy 天然透传 Upgrade。
// 未配置 AGENT_UI_VHOST_URL 时返回 503 明确错误（不静默 404）。
func (h *AgentHandler) UI(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	target := strings.TrimSpace(h.cfg.Agent.UIVHostURL)
	if target == "" {
		response.Error(c, http.StatusServiceUnavailable,
			"Agent UI proxy not configured (AGENT_UI_VHOST_URL)")
		return
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://" + target
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		response.InternalError(c, "Invalid agent UI vhost URL")
		return
	}

	// gin 路由 /agent/ui/*path 的 *path 即实例侧原始路径（已剥掉 /api/v1/agent/ui 前缀）
	upstreamPath := c.Param("path")
	if upstreamPath == "" {
		upstreamPath = "/"
	}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.URL.Path = upstreamPath
		// Host 头改写为当前用户的实例子域：manager vhost 按
		// agent-<uid>.agent.cloudzone-api.cyou 分发到该用户实例端口。
		req.Host = fmt.Sprintf("agent-%d.agent.cloudzone-api.cyou", subject.UserID)
	}
	// launcher 根路径 302 到 /launcher-login 等相对路径：重写 Location 头
	// 回到 /api/v1/agent/ui 前缀，避免浏览器跟随落到 sub2api 自身路由（#325 遗留）。
	// 同时把 CSP 的 frame-ancestors 'none' 放开为 'self'（WebUI 需被 sub2api 同源 iframe 嵌入；
	// 只影响本代理路径的响应，全局 CSP 不动）。
	uiPrefix := "/api/v1/agent/ui"
	proxy.ModifyResponse = func(resp *http.Response) error {
		loc := resp.Header.Get("Location")
		if loc != "" {
			if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
				// 绝对 URL 不动
			} else if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, uiPrefix) {
				// 实例侧绝对路径 -> 加 UI 前缀
				resp.Header.Set("Location", uiPrefix+loc)
			}
			// 相对路径 -> 浏览器按当前 URL 解析，保持原样
		}
		if csp := resp.Header.Get("Content-Security-Policy"); csp != "" {
			resp.Header.Set("Content-Security-Policy",
				strings.ReplaceAll(csp, "frame-ancestors 'none'", "frame-ancestors 'self'"))
		}
		// 上游 launcher 若自带 X-Frame-Options（DENY/SAMEORIGIN 都会阻止同源 iframe 场景
		// 之外的行为差异），统一移除——嵌入策略由本代理路径的 CSP frame-ancestors 'self' 掌控。
		resp.Header.Del("X-Frame-Options")
		return nil
	}
	// 去掉全局 SecurityHeaders 中间件已设置的 CSP 与 X-Frame-Options：
	// - CSP：httputil 复制上游头用 Add 语义，双 CSP 并存浏览器取交集，
	//   frame-ancestors 仍会被 'none' 锁死；由 ModifyResponse 输出重写后的单策略。
	// - X-Frame-Options: DENY：中间件全局加 DENY 禁止一切 iframe 嵌入，
	//   会直接让 WebUI 显示"已阻止此内容"（#326 实测）。
	c.Writer.Header().Del("Content-Security-Policy")
	c.Writer.Header().Del("X-Frame-Options")
	proxy.ServeHTTP(c.Writer, c.Request)
}
