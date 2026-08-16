/**
 * Agent 服务 v2（PicoClaw 容器一键部署/销毁，薄代理到 NY manager）
 * 用户级接口：每个用户只能操作自己的实例（后端按 user_id 隔离）
 */

import { apiClient } from './client'

export type AgentStatus =
  | 'not_started'
  | 'active'
  | 'running'
  | 'queued'
  | 'provisioning'
  | 'retained'
  | 'over_quota'
  | 'stopped'
  | 'error'

export interface AgentState {
  user_id?: number // 管理端列表响应含 user_id；用户端自身状态无
  status: AgentStatus
  port?: number
  agent_url?: string
  access_host?: string
  access_password?: string
  idle_deadline?: number // unix 秒（0 = 无）
  retain_deadline?: number // unix 秒
  hardcap_deadline?: number // unix 秒
  position?: number // queued 时的排队位置
  created_at?: string
}

/**
 * 启动 Agent：创建用户专属 key + NY 部署独立 PicoClaw 容器。
 * 返回 active(201)/queued(202)/already(200) 三种状态。
 */
export async function start(): Promise<AgentState> {
  const { data } = await apiClient.post<AgentState>('/agent/start')
  return data
}

/**
 * 关闭 Agent：销毁容器 + 吊销专属 key（幂等）
 */
export async function stop(): Promise<{ status: string }> {
  const { data } = await apiClient.post<{ status: string }>('/agent/stop')
  return data
}

/**
 * 查询当前用户 Agent 实例状态（含三项倒计时 deadline）
 */
export async function status(): Promise<AgentState> {
  const { data } = await apiClient.get<AgentState>('/agent/status')
  return data
}

/**
 * 下载当前用户实例归档（tar.gz，流式）。触发浏览器下载。
 */
export async function downloadArchive(): Promise<void> {
  const resp = await fetch(`${apiClient.defaults.baseURL}/agent/archive`, {
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
  a.download = 'agent-archive.tar.gz'
  a.click()
  URL.revokeObjectURL(url)
}

export const agentAPI = { start, stop, status, downloadArchive }

export default agentAPI
