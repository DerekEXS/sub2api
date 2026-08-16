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

    <!-- 全局配置 -->
    <div class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-6 mb-6">
      <h2 class="text-xl font-semibold mb-4">{{ t('admin.agent.globalConfig') }}</h2>
      <div v-if="configLoading" class="text-gray-500 text-sm">{{ t('admin.agent.loading') }}</div>
      <div v-else class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-5 gap-4">
        <div>
          <label class="block text-sm text-gray-500 dark:text-gray-400 mb-1">{{ t('admin.agent.retainHours') }}</label>
          <input
            v-model.number="configForm.retain_hours"
            type="number"
            min="1"
            class="w-full px-3 py-2 border border-gray-300 dark:border-dark-600 rounded-md bg-white dark:bg-dark-700 text-sm"
          />
        </div>
        <div>
          <label class="block text-sm text-gray-500 dark:text-gray-400 mb-1">{{ t('admin.agent.hardcapHours') }}</label>
          <input
            v-model.number="configForm.hardcap_hours"
            type="number"
            min="1"
            class="w-full px-3 py-2 border border-gray-300 dark:border-dark-600 rounded-md bg-white dark:bg-dark-700 text-sm"
          />
        </div>
        <div>
          <label class="block text-sm text-gray-500 dark:text-gray-400 mb-1">{{ t('admin.agent.quotaMB') }}</label>
          <input
            v-model.number="configForm.workspace_quota_mb"
            type="number"
            min="1"
            class="w-full px-3 py-2 border border-gray-300 dark:border-dark-600 rounded-md bg-white dark:bg-dark-700 text-sm"
          />
        </div>
        <div>
          <label class="block text-sm text-gray-500 dark:text-gray-400 mb-1">{{ t('admin.agent.memoryMB') }}</label>
          <input
            v-model.number="configForm.memory_mb"
            type="number"
            min="16"
            class="w-full px-3 py-2 border border-gray-300 dark:border-dark-600 rounded-md bg-white dark:bg-dark-700 text-sm"
          />
        </div>
        <div>
          <label class="block text-sm text-gray-500 dark:text-gray-400 mb-1">{{ t('admin.agent.idleMinutes') }}</label>
          <input
            v-model.number="configForm.idle_timeout_minutes"
            type="number"
            min="5"
            class="w-full px-3 py-2 border border-gray-300 dark:border-dark-600 rounded-md bg-white dark:bg-dark-700 text-sm"
          />
        </div>
      </div>
      <div v-if="!configLoading" class="mt-4 flex items-center gap-3">
        <button
          @click="saveGlobalConfig"
          class="px-4 py-2 bg-primary-600 hover:bg-primary-700 text-white rounded-md text-sm transition-colors"
        >
          {{ t('admin.agent.save') }}
        </button>
        <span v-if="configSaved" class="text-green-600 text-sm">✓ {{ t('admin.agent.saved') }}</span>
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
          <template v-for="a in agents" :key="a.user_id">
          <tr class="border-b border-gray-100 dark:border-dark-700">
            <td class="py-2 pr-4 font-mono">{{ a.user_id }}</td>
            <td class="py-2 pr-4">
              <span :class="statusBadgeClass(a.status)" class="px-2 py-0.5 rounded-full text-xs">{{ a.status }}</span>
            </td>
            <td class="py-2 pr-4 font-mono text-xs">{{ a.access_host || '—' }}</td>
            <td class="py-2 pr-4 font-mono text-xs">{{ fmtDeadline(a.hardcap_deadline) }}</td>
            <td class="py-2">
              <button
                @click="toggleUserConfig(a.user_id || 0)"
                class="px-2 py-1 text-xs bg-blue-100 hover:bg-blue-200 dark:bg-blue-900/30 dark:hover:bg-blue-900/50 text-blue-700 dark:text-blue-300 rounded mr-2"
              >
                {{ t('admin.agent.userConfig') }}
              </button>
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
          <!-- 每用户配置行内编辑 -->
          <tr v-if="editUserId === a.user_id" class="bg-blue-50/50 dark:bg-blue-900/10 border-b border-gray-100 dark:border-dark-700">
            <td colspan="5" class="py-3 px-2">
              <div class="grid grid-cols-1 md:grid-cols-3 lg:grid-cols-6 gap-3 items-end">
                <div>
                  <label class="block text-xs text-gray-500 dark:text-gray-400 mb-1">{{ t('admin.agent.retainHours') }}</label>
                  <input
                    v-model.number="userForm.retain_hours"
                    type="number"
                    min="1"
                    class="w-full px-2 py-1.5 border border-gray-300 dark:border-dark-600 rounded-md bg-white dark:bg-dark-700 text-xs"
                  />
                </div>
                <div>
                  <label class="block text-xs text-gray-500 dark:text-gray-400 mb-1">{{ t('admin.agent.hardcapHours') }}</label>
                  <input
                    v-model.number="userForm.hardcap_hours"
                    type="number"
                    min="1"
                    class="w-full px-2 py-1.5 border border-gray-300 dark:border-dark-600 rounded-md bg-white dark:bg-dark-700 text-xs"
                  />
                </div>
                <div>
                  <label class="block text-xs text-gray-500 dark:text-gray-400 mb-1">{{ t('admin.agent.quotaMB') }}</label>
                  <input
                    v-model.number="userForm.workspace_quota_mb"
                    type="number"
                    min="1"
                    class="w-full px-2 py-1.5 border border-gray-300 dark:border-dark-600 rounded-md bg-white dark:bg-dark-700 text-xs"
                  />
                </div>
                <div>
                  <label class="block text-xs text-gray-500 dark:text-gray-400 mb-1">{{ t('admin.agent.memoryMB') }}</label>
                  <input
                    v-model.number="userForm.memory_mb"
                    type="number"
                    min="16"
                    class="w-full px-2 py-1.5 border border-gray-300 dark:border-dark-600 rounded-md bg-white dark:bg-dark-700 text-xs"
                  />
                </div>
                <div>
                  <label class="block text-xs text-gray-500 dark:text-gray-400 mb-1">{{ t('admin.agent.idleMinutes') }}</label>
                  <input
                    v-model.number="userForm.idle_timeout_minutes"
                    type="number"
                    min="5"
                    class="w-full px-2 py-1.5 border border-gray-300 dark:border-dark-600 rounded-md bg-white dark:bg-dark-700 text-xs"
                  />
                </div>
                <div class="flex gap-2">
                  <button
                    @click="saveUserConfig"
                    class="px-3 py-1.5 bg-primary-600 hover:bg-primary-700 text-white rounded-md text-xs transition-colors"
                  >
                    {{ t('admin.agent.save') }}
                  </button>
                  <button
                    @click="clearUserConfig"
                    class="px-3 py-1.5 bg-gray-200 hover:bg-gray-300 dark:bg-dark-600 dark:hover:bg-dark-500 rounded-md text-xs"
                  >
                    {{ t('admin.agent.clearOverride') }}
                  </button>
                </div>
              </div>
              <div v-if="userForm.note" class="mt-1 text-xs text-gray-500">{{ userForm.note }}</div>
            </td>
          </tr>
          </template>
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
import type { AgentPoolStats, AgentConfig } from '@/api/admin/agents'
import AppLayout from '@/components/layout/AppLayout.vue'

const { t } = useI18n()

const agents = ref<AgentState[]>([])
const pool = ref<AgentPoolStats>({ free_gb: 0, active: 0, queued: 0, archived: 0 })
const loading = ref(false)

// 全局配置
const configLoading = ref(false)
const configSaved = ref(false)
const configForm = ref<AgentConfig>({ retain_hours: 72, hardcap_hours: 168, workspace_quota_mb: 250, memory_mb: 96, idle_timeout_minutes: 30 })

// 每用户配置
const editUserId = ref(0)
const userForm = ref<{ retain_hours: number; hardcap_hours: number; workspace_quota_mb: number; memory_mb: number; idle_timeout_minutes: number; note: string }>({
  retain_hours: 0,
  hardcap_hours: 0,
  workspace_quota_mb: 0,
  memory_mb: 0,
  idle_timeout_minutes: 0,
  note: '',
})

const loadGlobalConfig = async () => {
  configLoading.value = true
  try {
    const cfg = await agentAdminAPI.getAgentConfig()
    configForm.value = {
      retain_hours: cfg.retain_hours ?? 72,
      hardcap_hours: cfg.hardcap_hours ?? 168,
      workspace_quota_mb: cfg.workspace_quota_mb ?? 250,
      memory_mb: cfg.memory_mb ?? 96,
      idle_timeout_minutes: cfg.idle_timeout_minutes ?? 30,
    }
  } catch (e) {
    console.error('Failed to load agent config:', e)
  } finally {
    configLoading.value = false
  }
}

const saveGlobalConfig = async () => {
  configSaved.value = false
  try {
    const cfg = await agentAdminAPI.updateAgentConfig({ ...configForm.value })
    configForm.value = { ...cfg }
    configSaved.value = true
  } catch (e) {
    console.error('Failed to save agent config:', e)
  }
}

const toggleUserConfig = async (userId: number) => {
  if (editUserId.value === userId) {
    editUserId.value = 0
    return
  }
  editUserId.value = userId
  userForm.value = { retain_hours: 0, hardcap_hours: 0, workspace_quota_mb: 0, memory_mb: 0, idle_timeout_minutes: 0, note: '' }
  try {
    const cfg = await agentAdminAPI.getAgentUserConfig(userId)
    userForm.value.retain_hours = cfg.overrides?.retain_hours || 0
    userForm.value.hardcap_hours = cfg.overrides?.hardcap_hours || 0
    userForm.value.workspace_quota_mb = cfg.overrides?.workspace_quota_mb || 0
    userForm.value.memory_mb = cfg.overrides?.memory_mb || 0
    userForm.value.idle_timeout_minutes = cfg.overrides?.idle_timeout_minutes || 0
    userForm.value.note = t('admin.agent.effectiveHint', {
      retention: cfg.effective?.retain_hours ?? '—',
      hardcap: cfg.effective?.hardcap_hours ?? '—',
      quota: cfg.effective?.workspace_quota_mb ?? '—',
      memory: cfg.effective?.memory_mb ?? '—',
      idle: cfg.effective?.idle_timeout_minutes ?? '—',
    })
  } catch (e) {
    console.error('Failed to load user config:', e)
  }
}

const saveUserConfig = async () => {
  if (!editUserId.value) return
  try {
    const cfg = await agentAdminAPI.updateAgentUserConfig(editUserId.value, {
      retain_hours: userForm.value.retain_hours,
      hardcap_hours: userForm.value.hardcap_hours,
      workspace_quota_mb: userForm.value.workspace_quota_mb,
      memory_mb: userForm.value.memory_mb,
      idle_timeout_minutes: userForm.value.idle_timeout_minutes,
    })
    userForm.value.note = t('admin.agent.effectiveHint', {
      retention: cfg.effective?.retain_hours ?? '—',
      hardcap: cfg.effective?.hardcap_hours ?? '—',
      quota: cfg.effective?.workspace_quota_mb ?? '—',
      memory: cfg.effective?.memory_mb ?? '—',
      idle: cfg.effective?.idle_timeout_minutes ?? '—',
    })
  } catch (e) {
    console.error('Failed to save user config:', e)
  }
}

const clearUserConfig = async () => {
  if (!editUserId.value) return
  try {
    const cfg = await agentAdminAPI.updateAgentUserConfig(editUserId.value, {
      retain_hours: 0,
      hardcap_hours: 0,
      workspace_quota_mb: 0,
      memory_mb: 0,
      idle_timeout_minutes: 0,
    })
    userForm.value = { retain_hours: 0, hardcap_hours: 0, workspace_quota_mb: 0, memory_mb: 0, idle_timeout_minutes: 0, note: '' }
    userForm.value.note = t('admin.agent.effectiveHint', {
      retention: cfg.effective?.retain_hours ?? '—',
      hardcap: cfg.effective?.hardcap_hours ?? '—',
      quota: cfg.effective?.workspace_quota_mb ?? '—',
      memory: cfg.effective?.memory_mb ?? '—',
      idle: cfg.effective?.idle_timeout_minutes ?? '—',
    })
  } catch (e) {
    console.error('Failed to clear user config:', e)
  }
}

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
  loadGlobalConfig()
})
</script>

<style scoped>
.agent-admin-container {
  min-height: calc(100vh - 4rem);
}
</style>
