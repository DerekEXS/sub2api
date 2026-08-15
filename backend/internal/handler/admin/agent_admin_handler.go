package admin

import (
	"io"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// AgentAdminHandler 管理端 Agent 服务管理（薄代理到 NY manager）。
// 路由由管理端鉴权中间件保护（GET /api/v1/admin/agents, DELETE /api/v1/admin/agents/:user_id）。
type AgentAdminHandler struct {
	agentService *service.AgentService
}

func NewAgentAdminHandler(agentService *service.AgentService) *AgentAdminHandler {
	return &AgentAdminHandler{agentService: agentService}
}

// List GET /api/v1/admin/agents — 全量实例列表 + 池统计
func (h *AgentAdminHandler) List(c *gin.Context) {
	lst, err := h.agentService.ListAgents(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, lst)
}

// Delete DELETE /api/v1/admin/agents/:user_id — 销毁指定用户实例
func (h *AgentAdminHandler) Delete(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("user_id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid user_id")
		return
	}
	if err := h.agentService.StopAgent(c.Request.Context(), userID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"status": "stopped", "user_id": userID})
}

// Archive GET /api/v1/admin/agents/:user_id/archive — 流式下载指定用户实例归档
func (h *AgentAdminHandler) Archive(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("user_id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid user_id")
		return
	}
	r, err := h.agentService.DownloadArchive(c.Request.Context(), userID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", `attachment; filename="agent-`+strconv.FormatInt(userID, 10)+`.tar.gz"`)
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, r) // 流式透传，不缓冲
}

// RegistrationAudit GET /api/v1/admin/registration-audit — 注册风险审计名单
func (h *AgentAdminHandler) RegistrationAudit(c *gin.Context) {
	audit, err := service.GetRegistrationGuard().ListAudit(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if audit == nil {
		audit = []service.RegDecision{}
	}
	response.Success(c, gin.H{"count": len(audit), "items": audit})
}
