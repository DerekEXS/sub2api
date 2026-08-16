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
	proxy.ServeHTTP(c.Writer, c.Request)
}
