<template>
  <div class="agent-service-container p-6">
    <div class="max-w-4xl mx-auto">
      <h1 class="text-2xl font-bold mb-2">{{ t('agentService.title') }}</h1>
      <p class="text-gray-600 dark:text-gray-400 mb-6 text-sm">{{ t('agentService.description') }}</p>

      <!-- Status Card -->
      <div class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-6 mb-6">
        <div class="flex items-center justify-between mb-4">
          <h2 class="text-xl font-semibold">{{ t('agentService.statusTitle') }}</h2>
          <span :class="badgeClass">
            {{ statusText }}
          </span>
        </div>

        <div v-if="errorMessage" class="mb-4 p-3 rounded-md bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-300 text-sm">
          {{ errorMessage }}
        </div>

        <div class="flex gap-3">
          <button
            @click="startAgent"
            :disabled="agentStatus === 'starting' || agentStatus === 'running' || agentStatus === 'queued'"
            class="px-4 py-2 bg-primary-600 hover:bg-primary-700 text-white rounded-md disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
          >
            <span v-if="agentStatus === 'starting'" class="inline-block animate-spin mr-2">⚪</span>
            {{ t('agentService.startButton') }}
          </button>

          <button
            @click="stopAgent"
            :disabled="agentStatus !== 'running' && agentStatus !== 'queued'"
            class="px-4 py-2 bg-red-600 hover:bg-red-700 text-white rounded-md disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
          >
            <span v-if="agentStatus === 'stopping'" class="inline-block animate-spin mr-2">⚪</span>
            {{ t('agentService.stopButton') }}
          </button>

          <button
            @click="downloadArchive"
            v-if="agentStatus === 'running' || agentStatus === 'not_started'"
            class="px-4 py-2 bg-gray-200 hover:bg-gray-300 dark:bg-dark-600 dark:hover:bg-dark-500 text-gray-800 dark:text-gray-200 rounded-md transition-colors"
          >
            {{ t('agentService.downloadButton') }}
          </button>
        </div>

        <!-- Queued / Countdown Info -->
        <div v-if="agentStatus === 'queued'" class="mt-4 p-3 rounded-md bg-yellow-50 dark:bg-yellow-900/20 text-yellow-800 dark:text-yellow-200 text-sm">
          <span v-if="position > 0">{{ t('agentService.queuedHint') }}（#{{ position }}）</span>
          <span v-else>{{ t('agentService.queuedProvisioning') }}</span>
        </div>

        <!-- 生命周期倒计时（active 时显示） -->
        <div v-if="agentStatus === 'running' && (hardcapCountdown || retainCountdown)" class="mt-4 grid grid-cols-1 md:grid-cols-3 gap-3 text-sm">
          <div v-if="idleCountdown" class="bg-gray-50 dark:bg-dark-700 rounded-md p-3">
            <div class="text-gray-500 dark:text-gray-400 mb-1">{{ t('agentService.idleLabel') }}</div>
            <code class="font-mono text-base">{{ idleCountdown }}</code>
          </div>
          <div v-if="retainCountdown" class="bg-gray-50 dark:bg-dark-700 rounded-md p-3">
            <div class="text-gray-500 dark:text-gray-400 mb-1">{{ t('agentService.retainLabel') }}</div>
            <code class="font-mono text-base">{{ retainCountdown }}</code>
          </div>
          <div v-if="hardcapCountdown" class="bg-gray-50 dark:bg-dark-700 rounded-md p-3">
            <div class="text-gray-500 dark:text-gray-400 mb-1">{{ t('agentService.hardcapLabel') }}</div>
            <code class="font-mono text-base">{{ hardcapCountdown }}</code>
          </div>
        </div>
      </div>

      <!-- Agent UI: iframe (if gateway has HTML) or status panel (fallback) -->
      <div v-if="agentUrl && agentStatus === 'running'" class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-4">
        <!-- Has Web UI: show iframe -->
        <template v-if="hasWebUI">
          <div class="flex justify-between items-center mb-3">
            <h3 class="text-lg font-medium">{{ t('agentService.agentUiTitle') }}</h3>
            <a
              :href="agentUrl"
              target="_blank"
              rel="noopener noreferrer"
              class="text-primary-600 hover:text-primary-700 dark:text-primary-400 dark:hover:text-primary-300 text-sm"
            >
              {{ t('agentService.openInNewWindow') }}
            </a>
          </div>
          <iframe
            :src="agentUrl"
            class="w-full h-[600px] border rounded-md"
            title="PicoClaw Agent"
          ></iframe>
        </template>

        <!-- No Web UI: show status panel with connection info -->
        <template v-else>
          <div class="flex justify-between items-center mb-3">
            <h3 class="text-lg font-medium">{{ t('agentService.agentInfoTitle') }}</h3>
            <a
              :href="agentUrl"
              target="_blank"
              rel="noopener noreferrer"
              class="text-primary-600 hover:text-primary-700 dark:text-primary-400 dark:hover:text-primary-300 text-sm"
            >
              {{ t('agentService.openGateway') }}
            </a>
          </div>
          <div class="space-y-4">
            <!-- Connection Info -->
            <div class="bg-gray-50 dark:bg-dark-700 rounded-md p-4">
              <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
                <div>
                  <div class="text-sm text-gray-500 dark:text-gray-400 mb-1">{{ t('agentService.apiEndpoint') }}</div>
                  <code class="text-sm font-mono break-all">{{ agentUrl }}</code>
                </div>
                <div v-if="accessHost">
                  <div class="text-sm text-gray-500 dark:text-gray-400 mb-1">{{ t('agentService.accessHost') }}</div>
                  <code class="text-sm font-mono break-all">{{ accessHost }}</code>
                </div>
                <div v-if="accessPassword">
                  <div class="text-sm text-gray-500 dark:text-gray-400 mb-1">{{ t('agentService.accessPassword') }}</div>
                  <code class="text-sm font-mono break-all">{{ accessPassword }}</code>
                </div>
                <div>
                  <div class="text-sm text-gray-500 dark:text-gray-400 mb-1">{{ t('agentService.healthCheck') }}</div>
                  <code class="text-sm font-mono break-all">{{ agentUrl }}/health</code>
                </div>
              </div>
            </div>

            <!-- Quick Start Guide -->
            <div class="bg-blue-50 dark:bg-blue-900/20 rounded-md p-4">
              <h4 class="text-sm font-semibold text-blue-700 dark:text-blue-300 mb-2">{{ t('agentService.quickStartTitle') }}</h4>
              <ol class="list-decimal list-inside space-y-1 text-sm text-gray-700 dark:text-gray-300">
                <li>{{ t('agentService.quickStartStep1') }}</li>
                <li>{{ t('agentService.quickStartStep2') }}</li>
                <li>{{ t('agentService.quickStartStep3') }}</li>
              </ol>
            </div>

            <!-- Health Status -->
            <div class="flex items-center gap-2 text-sm">
              <span v-if="healthOk" class="text-green-600 dark:text-green-400">✓ {{ t('agentService.healthOk') }}</span>
              <span v-else class="text-yellow-600 dark:text-yellow-400">⚪ {{ t('agentService.healthChecking') }}</span>
            </div>
          </div>
        </template>
      </div>

      <!-- Empty State -->
      <div v-else-if="agentStatus === 'not_started'" class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-12 text-center">
        <div class="text-4xl mb-4">🤖</div>
        <h3 class="text-lg font-medium mb-2">{{ t('agentService.emptyTitle') }}</h3>
        <p class="text-gray-600 dark:text-gray-400 mb-4">{{ t('agentService.emptyHint') }}</p>
        <p class="text-sm text-gray-500 dark:text-gray-500">{{ t('agentService.emptyNote') }}</p>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, onBeforeUnmount, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { agentAPI } from '@/api/agent'
import type { AgentState } from '@/api/agent'

const { t } = useI18n()

type AgentUiStatus =
  | 'not_started'
  | 'starting'
  | 'running'
  | 'queued'
  | 'stopping'
  | 'error'

const agentStatus = ref<AgentUiStatus>('not_started')
const agentUrl = ref('')
const agentPort = ref(0)
const accessHost = ref('')
const accessPassword = ref('')
const position = ref(0)
const idleDeadline = ref(0)
const retainDeadline = ref(0)
const hardcapDeadline = ref(0)
const errorMessage = ref('')
const hasWebUI = ref(false)
const healthOk = ref(false)
const nowTs = ref(Math.floor(Date.now() / 1000))
let pollTimer: ReturnType<typeof setInterval> | null = null
let clockTimer: ReturnType<typeof setInterval> | null = null

const statusText = computed(() => {
  switch (agentStatus.value) {
    case 'not_started':
      return t('agentService.statusNotStarted')
    case 'starting':
      return t('agentService.statusStarting')
    case 'running':
      return t('agentService.statusRunning')
    case 'queued':
      return t('agentService.statusQueued')
    case 'stopping':
      return t('agentService.statusStopping')
    case 'error':
      return t('agentService.statusError')
    default:
      return t('agentService.statusUnknown')
  }
})

const badgeClass = computed(() => {
  const base = 'px-3 py-1 rounded-full text-sm font-medium'
  switch (agentStatus.value) {
    case 'starting':
    case 'queued':
      return `${base} bg-yellow-100 text-yellow-800 dark:bg-yellow-900/30 dark:text-yellow-200`
    case 'running':
      return `${base} bg-green-100 text-green-800 dark:bg-green-900/30 dark:text-green-200`
    case 'stopping':
    case 'error':
      return `${base} bg-red-100 text-red-800 dark:bg-red-900/30 dark:text-red-200`
    default:
      return `${base} bg-gray-100 text-gray-800 dark:bg-dark-600 dark:text-gray-200`
  }
})

const fmtCountdown = (deadline: number): string => {
  if (!deadline) return ''
  const diff = Math.max(0, deadline - nowTs.value)
  const h = Math.floor(diff / 3600)
  const m = Math.floor((diff % 3600) / 60)
  const s = diff % 60
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(h)}:${pad(m)}:${pad(s)}`
}

const idleCountdown = computed(() => fmtCountdown(idleDeadline.value))
const retainCountdown = computed(() => fmtCountdown(retainDeadline.value))
const hardcapCountdown = computed(() => fmtCountdown(hardcapDeadline.value))

const stopPolling = () => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
  if (clockTimer) {
    clearInterval(clockTimer)
    clockTimer = null
  }
}

const applyState = (state: AgentState) => {
  agentPort.value = state.port || 0
  accessHost.value = state.access_host || ''
  accessPassword.value = state.access_password || ''
  position.value = state.position || 0
  idleDeadline.value = state.idle_deadline || 0
  retainDeadline.value = state.retain_deadline || 0
  hardcapDeadline.value = state.hardcap_deadline || 0
}

/**
 * 探测网关是否有 HTML 响应（PicoClaw gateway 无内嵌 Web UI，根路径返回 JSON/404）。
 * 如果不是 HTML，前端显示状态面板+接入指引替代空白 iframe。
 */
const probeWebUI = async (url: string) => {
  if (!url) {
    hasWebUI.value = false
    return
  }
  try {
    const resp = await fetch(url, { signal: AbortSignal.timeout(5000) })
    const ct = resp.headers.get('content-type') || ''
    hasWebUI.value = ct.includes('text/html')
    healthOk.value = resp.ok || resp.status === 404 // 404 说明网关在线但没有根页面
  } catch {
    hasWebUI.value = false
    healthOk.value = false
  }
}

const startClock = () => {
  nowTs.value = Math.floor(Date.now() / 1000)
  if (!clockTimer) {
    clockTimer = setInterval(() => {
      nowTs.value = Math.floor(Date.now() / 1000)
    }, 1000)
  }
}

const startPolling = () => {
  stopPolling()
  pollTimer = setInterval(async () => {
    try {
      const state = await agentAPI.status()
      if (state.status === 'active' || state.status === 'running') {
        agentStatus.value = 'running'
        agentUrl.value = state.agent_url || `https://${state.access_host || ''}`
        applyState(state)
      } else if (state.status === 'queued' || state.status === 'provisioning') {
        agentStatus.value = 'queued'
        applyState(state)
      } else if (state.status === 'stopped' || state.status === 'not_started' || state.status === 'retained') {
        agentStatus.value = 'not_started'
        agentUrl.value = ''
        stopPolling()
      }
    } catch {
      // 轮询失败保持现状，下一轮再试
    }
  }, 10000)
}

const syncStatus = async () => {
  try {
    const state = await agentAPI.status()
    if (state.status === 'active' || state.status === 'running') {
      agentStatus.value = 'running'
      agentUrl.value = state.agent_url || `https://${state.access_host || ''}`
      applyState(state)
      await probeWebUI(state.agent_url || '')
      startPolling()
      startClock()
    } else if (state.status === 'queued' || state.status === 'provisioning') {
      agentStatus.value = 'queued'
      applyState(state)
      startPolling()
      startClock()
    } else if (state.status === 'stopped' || state.status === 'retained') {
      agentStatus.value = 'not_started'
    } else if (state.status === 'not_started') {
      agentStatus.value = 'not_started'
    }
  } catch {
    // 未配置/后端不可达：保持 not_started
    agentStatus.value = 'not_started'
  }
}

const startAgent = async () => {
  if (agentStatus.value !== 'not_started') return

  agentStatus.value = 'starting'
  errorMessage.value = ''

  try {
    const state = await agentAPI.start()
    if (state.status === 'active' || state.status === 'running') {
      agentStatus.value = 'running'
      agentUrl.value = state.agent_url || `https://${state.access_host || ''}`
      applyState(state)
      await probeWebUI(state.agent_url || '')
      startPolling()
      startClock()
    } else if (state.status === 'queued' || state.status === 'provisioning') {
      agentStatus.value = 'queued'
      applyState(state)
      startPolling()
      startClock()
    } else {
      agentStatus.value = 'not_started'
    }
  } catch (error: any) {
    console.error('Failed to start agent:', error)
    agentStatus.value = 'error'
    errorMessage.value = error?.message || t('agentService.startError')
  }
}

const stopAgent = async () => {
  if (agentStatus.value !== 'running' && agentStatus.value !== 'queued') return

  agentStatus.value = 'stopping'
  errorMessage.value = ''

  try {
    await agentAPI.stop()
    agentStatus.value = 'not_started'
    agentUrl.value = ''
    applyState({ status: 'not_started' })
    hasWebUI.value = false
    healthOk.value = false
    stopPolling()
  } catch (error: any) {
    console.error('Failed to stop agent:', error)
    agentStatus.value = 'running'
    errorMessage.value = error?.message || t('agentService.stopError')
  }
}

const downloadArchive = async () => {
  errorMessage.value = ''
  try {
    await agentAPI.downloadArchive()
  } catch (error: any) {
    errorMessage.value = error?.message || t('agentService.downloadError')
  }
}

// 当 agentUrl 变化时重新探测
watch(agentUrl, (url) => {
  if (url && agentStatus.value === 'running') {
    probeWebUI(url)
  }
})

onMounted(() => {
  syncStatus()
})

onBeforeUnmount(() => {
  stopPolling()
})
</script>

<style scoped>
.agent-service-container {
  min-height: calc(100vh - 4rem);
}
</style>
