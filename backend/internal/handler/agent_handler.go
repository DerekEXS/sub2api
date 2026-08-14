package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// AgentHandler 用户 Agent 服务（PicoClaw 容器一键部署/销毁）。
// 用户级鉴权：仅操作当前登录用户自己的实例（agents 表按 user_id 隔离）。
type AgentHandler struct {
	agentService *service.AgentService
}

func NewAgentHandler(agentService *service.AgentService) *AgentHandler {
	return &AgentHandler{agentService: agentService}
}

// Start POST /api/v1/agent/start — 启动（创建用户专属 key + NY 容器 + 落库）
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
