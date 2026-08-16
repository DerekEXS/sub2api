/**
 * 管理端 Agent 服务 API（实例列表 / 销毁 / 归档下载 / 注册风险审计）
 */

import { apiClient } from '../client'
import type { AgentState } from '../agent'

export interface AgentPoolStats {
  free_gb: number
  active: number
  queued: number
  archived: number
}

export interface AgentAdminListItem extends AgentState {
  user_id?: number
  email?: string
  username?: string
}

export interface AgentListResponse {
  agents: AgentAdminListItem[]
  pool: AgentPoolStats
}

/** Agent 后端宿主硬件指标（#328 仪表盘） */
export interface AgentHostMetrics {
  ts: number
  cpu_percent: number
  cpu_cores: number
  load1: number
  load5: number
  load15: number
  mem_total_mb: number
  mem_used_mb: number
  mem_percent: number
  swap_total_mb: number
  swap_used_mb: number
  disk_total_gb: number
  disk_used_gb: number
  disk_percent: number
  containers_running: number
  uptime_seconds: number
}

export interface RegistrationAuditItem {
  user_id: number
  email?: string
  score: number
  strong: boolean
  invited: boolean
  ip: string
  created_at: string
}

/**
 * 全量实例列表 + 池统计
 */
export async function listAgents(): Promise<AgentListResponse> {
  const { data } = await apiClient.get<AgentListResponse>('/admin/agents')
  return data
}

/**
 * 销毁指定用户实例（归档+销毁）
 */
export async function deleteAgent(userId: number): Promise<{ status: string }> {
  const { data } = await apiClient.delete<{ status: string }>(`/admin/agents/${userId}`)
  return data
}

/**
 * 下载指定用户实例归档（流式，触发浏览器下载）
 */
export async function downloadAgentArchive(userId: number): Promise<void> {
  const resp = await fetch(`${apiClient.defaults.baseURL}/admin/agents/${userId}/archive`, {
    credentials: 'include',
    headers: {
      Authorization: `Bearer ${localStorage.getItem('access_token') || ''}`,
    },
  })
  if (!resp.ok) {
    throw new Error(`download archive failed: ${resp.status}`)
  }
  const blob = await resp.blob()
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `agent-${userId}-archive.tar.gz`
  a.click()
  URL.revokeObjectURL(url)
}

/**
 * 注册风险审计名单（分页，默认 20/页）
 */
export interface RegistrationAuditPage {
  count: number
  page: number
  page_size: number
  items: RegistrationAuditItem[]
}

export async function listRegistrationAudit(page = 1, pageSize = 20): Promise<RegistrationAuditPage> {
  const { data } = await apiClient.get<RegistrationAuditPage>('/admin/registration-audit', {
    params: { page, page_size: pageSize },
  })
  return data
}

/**
 * Agent 后端宿主硬件指标
 */
export async function getAgentMetrics(): Promise<AgentHostMetrics> {
  const { data } = await apiClient.get<AgentHostMetrics>('/admin/agents/metrics')
  return data
}

/**
 * 注册风险审计规则配置（S5 后端并行开发，GET/PUT /admin/registration-audit/config）
 */
export interface RegistrationAuditConfig {
  ua_check_enabled: boolean // UA 检查开关
  cgnat_exempt: boolean // CGNAT 豁免开关
  flag_threshold: number // 打标阈值
  strong_threshold: number // 强标阈值
  email_long_local_min: number // 邮箱长本地最小长度
  email_vowel_ratio_max: number // 邮箱元音最大比例（0-1）
  score_email_random: number // 随机邮箱分值（允许负数）
  score_email_alias: number // 别名邮箱分值（允许负数）
  score_email_whitelist: number // 白名单邮箱分值（允许负数）
  score_rhythm: number // 注册节奏分值（允许负数）
  score_24h_4_5: number // 24h 4-5 次注册分值（允许负数）
  score_24h_6_plus: number // 24h 6 次以上注册分值（允许负数）
}

export async function getRegistrationAuditConfig(): Promise<RegistrationAuditConfig> {
  const { data } = await apiClient.get<RegistrationAuditConfig>('/admin/registration-audit/config')
  return data
}

export async function updateRegistrationAuditConfig(cfg: RegistrationAuditConfig): Promise<RegistrationAuditConfig> {
  const { data } = await apiClient.put<RegistrationAuditConfig>('/admin/registration-audit/config', cfg)
  return data
}

export interface AgentConfig {
  retain_hours: number // 关闭后保留期（小时，默认 72）
  hardcap_hours: number // 硬顶（小时，自首次启动起算，默认 168）
  workspace_quota_mb: number
  memory_mb: number
  idle_timeout_minutes: number
}

export interface AgentUserConfigResponse {
  global: AgentConfig
  overrides: Record<string, number>
  effective: AgentConfig
}

/**
 * 全局 Agent 配置
 */
export async function getAgentConfig(): Promise<AgentConfig> {
  const { data } = await apiClient.get<AgentConfig>('/admin/agents/config')
  return data
}

export async function updateAgentConfig(cfg: AgentConfig): Promise<AgentConfig> {
  const { data } = await apiClient.put<AgentConfig>('/admin/agents/config', cfg)
  return data
}

/**
 * 每用户配置（覆盖 + 生效值）
 */
export async function getAgentUserConfig(userId: number): Promise<AgentUserConfigResponse> {
  const { data } = await apiClient.get<AgentUserConfigResponse>(`/admin/agents/${userId}/config`)
  return data
}

export async function updateAgentUserConfig(userId: number, overrides: Record<string, number>): Promise<AgentUserConfigResponse> {
  const { data } = await apiClient.put<AgentUserConfigResponse>(`/admin/agents/${userId}/config`, overrides)
  return data
}

export const agentAdminAPI = {
  listAgents,
  getAgentMetrics,
  deleteAgent,
  downloadAgentArchive,
  listRegistrationAudit,
  getRegistrationAuditConfig,
  updateRegistrationAuditConfig,
  getAgentConfig,
  updateAgentConfig,
  getAgentUserConfig,
  updateAgentUserConfig,
}

export default agentAdminAPI
