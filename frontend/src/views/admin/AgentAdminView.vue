<template>
  <AppLayout>
  <div class="agent-admin-container p-6">
    <h1 class="text-2xl font-bold mb-2">{{ t('admin.agent.title') }}</h1>
    <p class="text-gray-600 dark:text-gray-400 mb-6 text-sm">{{ t('admin.agent.description') }}</p>

    <!-- 容量池仪表 -->
    <div class="grid grid-cols-2 md:grid-cols-4 gap-4 mb-6">
      <div class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-4">
        <div class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.agent.poolFree') }}</div>
        <div class="text-2xl font-bold mt-1">{{ pool.free_gb }}<span class="text-sm font-normal text-gray-500"> GB</span></div>
      </div>
      <div class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-4">
        <div class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.agent.poolActive') }}</div>
        <div class="text-2xl font-bold mt-1 text-green-600">{{ pool.active }}</div>
      </div>
      <div class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-4">
        <div class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.agent.poolQueued') }}</div>
        <div class="text-2xl font-bold mt-1 text-yellow-600">{{ pool.queued }}</div>
      </div>
      <div class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-4">
        <div class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.agent.poolArchived') }}</div>
        <div class="text-2xl font-bold mt-1 text-gray-600">{{ pool.archived }}</div>
      </div>
    </div>

    <!-- 实例列表 -->
    <div class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-6 mb-6">
      <div class="flex items-center justify-between mb-4">
        <h2 class="text-xl font-semibold">{{ t('admin.agent.instanceList') }}</h2>
        <button
          @click="refresh"
          class="px-3 py-1.5 bg-primary-600 hover:bg-primary-700 text-white rounded-md text-sm transition-colors"
        >
          {{ t('admin.agent.refresh') }}
        </button>
      </div>

      <div v-if="loading" class="text-center py-8 text-gray-500">{{ t('admin.agent.loading') }}</div>
      <div v-else-if="agents.length === 0" class="text-center py-8 text-gray-500">{{ t('admin.agent.noInstances') }}</div>
      <table v-else class="w-full text-sm">
        <thead>
          <tr class="text-left text-gray-500 dark:text-gray-400 border-b border-gray-200 dark:border-dark-600">
            <th class="py-2 pr-4">{{ t('admin.agent.colUser') }}</th>
            <th class="py-2 pr-4">{{ t('admin.agent.colStatus') }}</th>
            <th class="py-2 pr-4">{{ t('admin.agent.colHost') }}</th>
            <th class="py-2 pr-4">{{ t('admin.agent.colHardcap') }}</th>
            <th class="py-2">{{ t('admin.agent.colActions') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="a in agents" :key="a.user_id" class="border-b border-gray-100 dark:border-dark-700">
            <td class="py-2 pr-4 font-mono">{{ a.user_id }}</td>
            <td class="py-2 pr-4">
              <span :class="statusBadgeClass(a.status)" class="px-2 py-0.5 rounded-full text-xs">{{ a.status }}</span>
            </td>
            <td class="py-2 pr-4 font-mono text-xs">{{ a.access_host || '—' }}</td>
            <td class="py-2 pr-4 font-mono text-xs">{{ fmtDeadline(a.hardcap_deadline) }}</td>
            <td class="py-2">
              <button
                @click="downloadArchive(a.user_id || 0)"
                class="px-2 py-1 text-xs bg-gray-200 hover:bg-gray-300 dark:bg-dark-600 dark:hover:bg-dark-500 rounded mr-2"
              >
                {{ t('admin.agent.download') }}
              </button>
              <button
                @click="destroyAgent(a.user_id || 0)"
                class="px-2 py-1 text-xs bg-red-600 hover:bg-red-700 text-white rounded"
              >
                {{ t('admin.agent.destroy') }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- 注册风险审计名单 -->
    <div class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-6">
      <div class="flex items-center justify-between mb-4">
        <h2 class="text-xl font-semibold">{{ t('admin.agent.auditTitle') }}（{{ auditCount }}）</h2>
        <button
          @click="refreshAudit"
          class="px-3 py-1.5 bg-gray-200 hover:bg-gray-300 dark:bg-dark-600 dark:hover:bg-dark-500 text-gray-800 dark:text-gray-200 rounded-md text-sm transition-colors"
        >
          {{ t('admin.agent.refresh') }}
        </button>
      </div>

      <div v-if="auditLoading" class="text-center py-8 text-gray-500">{{ t('admin.agent.loading') }}</div>
      <div v-else-if="auditItems.length === 0" class="text-center py-8 text-gray-500">{{ t('admin.agent.noAudit') }}</div>
      <table v-else class="w-full text-sm">
        <thead>
          <tr class="text-left text-gray-500 dark:text-gray-400 border-b border-gray-200 dark:border-dark-600">
            <th class="py-2 pr-4">{{ t('admin.agent.colUser') }}</th>
            <th class="py-2 pr-4">{{ t('admin.agent.colScore') }}</th>
            <th class="py-2 pr-4">{{ t('admin.agent.colStrong') }}</th>
            <th class="py-2 pr-4">{{ t('admin.agent.colInvited') }}</th>
            <th class="py-2 pr-4">{{ t('admin.agent.colIp') }}</th>
            <th class="py-2">{{ t('admin.agent.colTime') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="item in auditItems" :key="item.user_id" class="border-b border-gray-100 dark:border-dark-700">
            <td class="py-2 pr-4 font-mono">{{ item.user_id }}</td>
            <td class="py-2 pr-4">
              <span :class="item.score >= 60 ? 'text-red-600 font-bold' : item.score >= 40 ? 'text-yellow-600' : 'text-gray-600'">
                {{ item.score }}
              </span>
            </td>
            <td class="py-2 pr-4">{{ item.strong ? '⚠️' : '—' }}</td>
            <td class="py-2 pr-4">{{ item.invited ? '✓' : '—' }}</td>
            <td class="py-2 pr-4 font-mono text-xs">{{ item.ip }}</td>
            <td class="py-2 font-mono text-xs">{{ fmtTs(item.created_at) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { agentAdminAPI } from '@/api/admin/agents'
import type { AgentState } from '@/api/agent'
import type { AgentPoolStats, RegistrationAuditItem } from '@/api/admin/agents'
import AppLayout from '@/components/layout/AppLayout.vue'

const { t } = useI18n()

const agents = ref<AgentState[]>([])
const pool = ref<AgentPoolStats>({ free_gb: 0, active: 0, queued: 0, archived: 0 })
const auditItems = ref<RegistrationAuditItem[]>([])
const auditCount = ref(0)
const loading = ref(false)
const auditLoading = ref(false)

const statusBadgeClass = (status: string): string => {
  switch (status) {
    case 'active':
    case 'running':
      return 'bg-green-100 text-green-800 dark:bg-green-900/30 dark:text-green-200'
    case 'queued':
    case 'provisioning':
      return 'bg-yellow-100 text-yellow-800 dark:bg-yellow-900/30 dark:text-yellow-200'
    case 'retained':
      return 'bg-gray-100 text-gray-700 dark:bg-dark-600 dark:text-gray-300'
    case 'over_quota':
    case 'error':
      return 'bg-red-100 text-red-800 dark:bg-red-900/30 dark:text-red-200'
    default:
      return 'bg-gray-100 text-gray-600 dark:bg-dark-600 dark:text-gray-400'
  }
}

const fmtDeadline = (ts?: number): string => {
  if (!ts) return '—'
  return new Date(ts * 1000).toLocaleString()
}

const fmtTs = (s?: string): string => {
  if (!s) return '—'
  const n = Number(s)
  if (Number.isFinite(n) && n > 0) {
    return new Date(n * 1000).toLocaleString()
  }
  return s
}

const refresh = async () => {
  loading.value = true
  try {
    const data = await agentAdminAPI.listAgents()
    agents.value = data.agents || []
    pool.value = data.pool || { free_gb: 0, active: 0, queued: 0, archived: 0 }
  } catch (e) {
    console.error('Failed to list agents:', e)
  } finally {
    loading.value = false
  }
}

const refreshAudit = async () => {
  auditLoading.value = true
  try {
    const data = await agentAdminAPI.listRegistrationAudit()
    auditItems.value = data.items || []
    auditCount.value = data.count ?? auditItems.value.length
  } catch (e) {
    console.error('Failed to list registration audit:', e)
  } finally {
    auditLoading.value = false
  }
}

const destroyAgent = async (userId: number) => {
  if (!window.confirm(`${t('admin.agent.destroyConfirm')} ${userId}?`)) return
  try {
    await agentAdminAPI.deleteAgent(userId)
    await refresh()
  } catch (e) {
    console.error('Failed to destroy agent:', e)
  }
}

const downloadArchive = async (userId: number) => {
  try {
    await agentAdminAPI.downloadAgentArchive(userId)
  } catch (e) {
    console.error('Failed to download archive:', e)
  }
}

onMounted(() => {
  refresh()
  refreshAudit()
})
</script>

<style scoped>
.agent-admin-container {
  min-height: calc(100vh - 4rem);
}
</style>
