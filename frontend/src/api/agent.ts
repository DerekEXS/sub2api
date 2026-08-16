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
  idle_deadline?: number // unix 秒 或相对秒数（0 = 无）
  retain_deadline?: number // unix 秒 或相对秒数
  hardcap_deadline?: number // unix 秒 或相对秒数
  position?: number // queued 时的排队位置
  idle_timeout_minutes?: number // 空闲自动销毁超时（分钟，供前端展示）
  retain_hours?: number // 关闭后保留期（小时，供前端展示；S5 新字段）
  hardcap_hours?: number // 硬顶（小时，自首次启动起算；S5 新字段）
  data_retention_hours?: number // 旧字段：关闭后数据保留时长（小时），S5 后不再返回，保留兼容
  created_at?: string
}

/**
 * 启动 Agent：创建用户专属 key + NY 部署独立 PicoClaw 容器。
 * 返回 active(201)/queued(202)/already(200) 三种状态。
 */
export async function start(): Promise<AgentState> {
  // 容器创建是同步慢操作（docker run + launcher 初始化，可达 60-150s），
  // 必须大于后端 managerCreateTimeout(150s)，避免首次启动被默认 30s 超时误杀（#320）
  const { data } = await apiClient.post<AgentState>('/agent/start', undefined, { timeout: 180000 })
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
