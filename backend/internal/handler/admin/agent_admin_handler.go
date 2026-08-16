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

// RegAuditGetConfig GET /api/v1/admin/registration-audit/config — 当前生效的注册审计配置
// （有 Redis 时读 regaudit:config hash；无 Redis 返回硬编码默认值）
func (h *AgentAdminHandler) RegAuditGetConfig(c *gin.Context) {
	cfg := service.GetRegistrationGuard().GetAuditConfig(c.Request.Context())
	response.Success(c, cfg)
}

// RegAuditUpdateConfig PUT /api/v1/admin/registration-audit/config — 部分更新注册审计配置
// （仅更新 body 中出现的字段，nil 字段保持原值；返回更新后的完整配置）
func (h *AgentAdminHandler) RegAuditUpdateConfig(c *gin.Context) {
	var patch service.RegAuditConfigPatch
	if err := c.ShouldBindJSON(&patch); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	cfg, err := service.GetRegistrationGuard().UpdateAuditConfig(c.Request.Context(), patch)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

// GetConfig GET /api/v1/admin/agents/config — 全局 Agent 配置
func (h *AgentAdminHandler) GetConfig(c *gin.Context) {
	cfg, err := h.agentService.GetAgentConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

// UpdateConfig PUT /api/v1/admin/agents/config — 更新全局 Agent 配置
func (h *AgentAdminHandler) UpdateConfig(c *gin.Context) {
	var req service.AgentConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	cfg, err := h.agentService.UpdateAgentConfig(c.Request.Context(), req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

// GetUserConfig GET /api/v1/admin/agents/:user_id/config — 每用户配置
func (h *AgentAdminHandler) GetUserConfig(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("user_id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid user_id")
		return
	}
	cfg, err := h.agentService.GetAgentUserConfig(c.Request.Context(), userID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

// UpdateUserConfig PUT /api/v1/admin/agents/:user_id/config — 每用户配置覆盖
func (h *AgentAdminHandler) UpdateUserConfig(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("user_id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid user_id")
		return
	}
	var req map[string]int
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	cfg, err := h.agentService.UpdateAgentUserConfig(c.Request.Context(), userID, req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}
