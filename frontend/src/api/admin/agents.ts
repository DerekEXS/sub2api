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

export interface AgentListResponse {
  agents: AgentState[]
  pool: AgentPoolStats
}

export interface RegistrationAuditItem {
  user_id: number
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
 * 注册风险审计名单
 */
export async function listRegistrationAudit(): Promise<{ count: number; items: RegistrationAuditItem[] }> {
  const { data } = await apiClient.get<{ count: number; items: RegistrationAuditItem[] }>('/admin/registration-audit')
  return data
}

export const agentAdminAPI = { listAgents, deleteAgent, downloadAgentArchive, listRegistrationAudit }

export default agentAdminAPI
