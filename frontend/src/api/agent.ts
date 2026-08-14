/**
 * Agent 服务（PicoClaw 容器一键部署/销毁）
 * 用户级接口：每个用户只能操作自己的实例（后端按 user_id 隔离）
 */

import { apiClient } from './client'

export interface AgentState {
  status: 'not_started' | 'running' | 'stopped' | 'error'
  port?: number
  agent_url?: string
  created_at?: string
}

/**
 * 启动 Agent：创建用户专属 key + NY 部署独立 PicoClaw 容器（可等待 ~30s）
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
 * 查询当前用户 Agent 实例状态
 */
export async function status(): Promise<AgentState> {
  const { data } = await apiClient.get<AgentState>('/agent/status')
  return data
}

export const agentAPI = { start, stop, status }

export default agentAPI
