<template>
  <AppLayout>
  <!-- 根容器：max-w-7xl 放宽（WebUI 需要大画幅），iframe 尺寸由 aspect-video 按宽驱动 -->
  <div class="agent-service-container p-6">
    <div class="max-w-7xl mx-auto w-full">
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
            v-if="agentStatus === 'running' || agentStatus === 'not_started' || agentStatus === 'retained'"
            class="px-4 py-2 bg-gray-200 hover:bg-gray-300 dark:bg-dark-600 dark:hover:bg-dark-500 text-gray-800 dark:text-gray-200 rounded-md transition-colors"
          >
            {{ t('agentService.downloadButton') }}
          </button>
        </div>
        <p class="mt-3 text-xs text-gray-500 dark:text-gray-400">{{ t('agentService.launchNote') }}</p>

        <!-- Queued / Countdown Info -->
        <div v-if="agentStatus === 'queued'" class="mt-4 p-3 rounded-md bg-yellow-50 dark:bg-yellow-900/20 text-yellow-800 dark:text-yellow-200 text-sm">
          <span v-if="position > 0">{{ t('agentService.queuedHint') }}（#{{ position }}）</span>
          <span v-else>{{ t('agentService.queuedProvisioning') }}</span>
        </div>

        <!-- 生命周期倒计时（running 或 retained 时显示，#issue3：关闭后仍显示保留期+硬顶倒计时） -->
        <div v-if="(agentStatus === 'running' || agentStatus === 'retained') && (hardcapCountdown || retainCountdown)" class="mt-4 grid grid-cols-1 md:grid-cols-3 gap-3 text-sm">
          <div v-if="agentStatus === 'running' && idleCountdown" class="bg-gray-50 dark:bg-dark-700 rounded-md p-3">
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

        <!-- 8 位专属访问密码（running 时显示） -->
        <div v-if="agentStatus === 'running' && accessPassword" class="mt-4 flex flex-wrap items-center gap-3 bg-gray-50 dark:bg-dark-700 rounded-md p-3 text-sm">
          <span class="text-gray-500 dark:text-gray-400">{{ t('agentService.passwordLabel') }}</span>
          <code class="font-mono text-base break-all">{{ accessPassword }}</code>
          <button
            @click="copyPassword"
            class="px-3 py-1 bg-primary-600 hover:bg-primary-700 text-white rounded-md text-xs transition-colors"
          >
            {{ copied ? t('agentService.copied') : t('agentService.copyButton') }}
          </button>
        </div>
      </div>

      <!-- Agent WebUI：同源 iframe 直接嵌入（不暴露任何 IP/URL）。
           16:9 大画幅：aspect-video 按容器宽驱动高度（max-w-7xl 下 ≈1216×684），
           min-h 兜底小屏，max-h 防超高屏幕溢出视口。 -->
      <div v-if="agentStatus === 'running'" class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-4 mb-6">
        <h3 class="text-lg font-medium mb-3">{{ t('agentService.agentUiTitle') }}</h3>
        <iframe
          :src="`/api/v1/agent/ui/?cz_token=${uiSessionToken}`"
          class="w-full aspect-video min-h-[540px] max-h-[calc(100vh-14rem)] border rounded-md"
          title="PicoClaw Agent"
        ></iframe>
      </div>

      <!-- Retained State（已关闭但数据保留中，#issue3） -->
      <div v-else-if="agentStatus === 'retained'" class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-8 text-center">
        <div class="text-4xl mb-4">📦</div>
        <h3 class="text-lg font-medium mb-2">{{ t('agentService.retainedTitle') }}</h3>
        <p class="text-gray-600 dark:text-gray-400 mb-6">{{ t('agentService.retainedHint') }}</p>
        <button
          @click="startAgent"
          class="px-6 py-2 bg-primary-600 hover:bg-primary-700 text-white rounded-md transition-colors"
        >
          {{ t('agentService.restartButton') }}
        </button>
      </div>

      <!-- Empty State -->
      <div v-else-if="agentStatus === 'not_started'" class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-12 text-center">
        <div class="text-4xl mb-4">🤖</div>
        <h3 class="text-lg font-medium mb-2">{{ t('agentService.emptyTitle') }}</h3>
        <p class="text-gray-600 dark:text-gray-400 mb-4">{{ t('agentService.emptyHint') }}</p>
        <p class="text-sm text-gray-500 dark:text-gray-500">
          {{ t('agentService.emptyNote', { X: idleTimeoutMinutes, Y: retainHours, Z: hardcapHours }) }}
        </p>
      </div>
    </div>
  </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import { agentAPI } from '@/api/agent'
import type { AgentState } from '@/api/agent'
import AppLayout from '@/components/layout/AppLayout.vue'

const { t } = useI18n()

// iframe 会话认证 token（#326）：iframe 内请求不带 Authorization header，
// 通过 query cz_token 由后端建立 cookie session（12h）。
const uiSessionToken = computed(() => localStorage.getItem('auth_token') || '')

type AgentUiStatus =
  | 'not_started'
  | 'starting'
  | 'running'
  | 'queued'
  | 'stopping'
  | 'retained'
  | 'error'

const agentStatus = ref<AgentUiStatus>('not_started')
const accessPassword = ref('')
const position = ref(0)
const idleDeadline = ref(0)
const retainDeadline = ref(0)
const hardcapDeadline = ref(0)
const idleTimeoutMinutes = ref(30) // 后端未提供时默认 30 分钟
const retainHours = ref(72) // 关闭后保留期（小时），后端未提供时默认 72
const hardcapHours = ref(168) // 硬顶（小时，自首次启动起算），后端未提供时默认 168
const errorMessage = ref('')
const copied = ref(false)
const nowTs = ref(Math.floor(Date.now() / 1000))
let pollTimer: ReturnType<typeof setInterval> | null = null
let clockTimer: ReturnType<typeof setInterval> | null = null
let copyTimer: ReturnType<typeof setTimeout> | null = null

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
    case 'retained':
      return t('agentService.statusRetained')
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

/**
 * 相对秒数（< 1e10）一次性归一化为绝对 unix 时间戳（仅在 applyState 时锚定，
 * 之后由 nowTs 时钟逐秒递减）；>= 1e10 视为绝对时间戳直接使用
 * （后端 S5 并行改造后直接返回绝对时间戳，前端归一化兼容两者）。
 */
// 阈值 1e9：manager 返回绝对 unix 秒（~1.7e9 > 1e9）原样用；小值视为相对秒数锚定。
// ⚠️ 此前写 1e10 把绝对时间戳误判为相对秒又加一遍 now → 倒计时显示 ~50 万小时（#328）。
const normalizeDeadline = (v?: number): number => {
  if (!v || v <= 0) return 0
  return v < 1e9 ? Math.floor(Date.now() / 1000) + v : v
}

/**
 * 倒计时格式化：deadline 一律为绝对时间戳（applyState 已归一化相对秒数）。
 * diff = deadline - nowTs，随 nowTs 时钟每秒递减。
 */
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
  accessPassword.value = state.access_password || ''
  position.value = state.position || 0
  // 相对秒数只在 applyState 时归一化一次，避免 fmtCountdown 每次 tick 都
  // now+deadline-now 恒等导致倒计时永远不走（#324 改动 1）
  idleDeadline.value = normalizeDeadline(state.idle_deadline)
  retainDeadline.value = normalizeDeadline(state.retain_deadline)
  hardcapDeadline.value = normalizeDeadline(state.hardcap_deadline)
  idleTimeoutMinutes.value = state.idle_timeout_minutes || 30
  // S5 新字段优先；旧字段 data_retention_hours 兜底兼容
  retainHours.value = state.retain_hours ?? state.data_retention_hours ?? 72
  hardcapHours.value = state.hardcap_hours || 168
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
        applyState(state)
      } else if (state.status === 'queued' || state.status === 'provisioning') {
        agentStatus.value = 'queued'
        applyState(state)
      } else if (state.status === 'retained') {
        // #issue3：关闭后仍显示倒计时（保留期+硬顶），让用户掌握数据清理时间
        agentStatus.value = 'retained'
        applyState(state)
        startClock()
      } else if (state.status === 'stopped' || state.status === 'not_started') {
        agentStatus.value = 'not_started'
        // 未启动也读取配置字段（空闲超时/保留时长）供空状态文案展示 #issue2
        applyState(state)
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
      applyState(state)
      startPolling()
      startClock()
    } else if (state.status === 'queued' || state.status === 'provisioning') {
      agentStatus.value = 'queued'
      applyState(state)
      startPolling()
      startClock()
    } else if (state.status === 'retained') {
      // #issue3：关闭后仍显示倒计时
      agentStatus.value = 'retained'
      applyState(state)
      startClock()
    } else {
      agentStatus.value = 'not_started'
      // 未启动也读取配置字段（空闲超时/保留时长）供空状态文案展示 #issue2
      applyState(state)
    }
  } catch {
    // 未配置/后端不可达：保持 not_started
    agentStatus.value = 'not_started'
  }
}

const startAgent = async () => {
  // #issue3：retained 状态也允许重新启动（之前 guard 拦截了 retained → 点"重新启动"无反应）
  if (agentStatus.value !== 'not_started' && agentStatus.value !== 'retained') return

  agentStatus.value = 'starting'
  errorMessage.value = ''

  try {
    const state = await agentAPI.start()
    if (state.status === 'active' || state.status === 'running') {
      agentStatus.value = 'running'
      applyState(state)
      startPolling()
      startClock()
    } else if (state.status === 'queued' || state.status === 'provisioning') {
      agentStatus.value = 'queued'
      applyState(state)
      startPolling()
      startClock()
    } else {
      agentStatus.value = 'not_started'
      applyState(state)
    }
  } catch (error: any) {
    console.error('Failed to start agent:', error)
    agentStatus.value = 'error'
    errorMessage.value = t('agentService.startError')
  }
}

const stopAgent = async () => {
  if (agentStatus.value !== 'running' && agentStatus.value !== 'queued') return

  agentStatus.value = 'stopping'
  errorMessage.value = ''

  try {
    await agentAPI.stop()
    agentStatus.value = 'not_started'
    applyState({ status: 'not_started' })
    stopPolling()
  } catch (error: any) {
    console.error('Failed to stop agent:', error)
    agentStatus.value = 'running'
    errorMessage.value = t('agentService.stopError')
  }
}

const downloadArchive = async () => {
  errorMessage.value = ''
  try {
    await agentAPI.downloadArchive()
  } catch (error: any) {
    console.error('Failed to download archive:', error)
    errorMessage.value = t('agentService.downloadError')
  }
}

/** 复制访问密码到剪贴板，成功显示 ✓ 提示 2 秒 */
const copyPassword = async () => {
  if (!accessPassword.value) return
  try {
    await navigator.clipboard.writeText(accessPassword.value)
  } catch {
    // 非安全上下文兜底：textarea + execCommand
    const ta = document.createElement('textarea')
    ta.value = accessPassword.value
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    try {
      document.execCommand('copy')
    } finally {
      document.body.removeChild(ta)
    }
  }
  copied.value = true
  if (copyTimer) clearTimeout(copyTimer)
  copyTimer = setTimeout(() => {
    copied.value = false
  }, 2000)
}

onMounted(() => {
  syncStatus()
})

onBeforeUnmount(() => {
  stopPolling()
  if (copyTimer) clearTimeout(copyTimer)
})
</script>

<style scoped>
.agent-service-container {
  /* 与 AppLayout main 的 p-4/md:p-6/lg:p-8 配合：main 高度 = 视口 - 顶栏 - 上下内边距 */
  min-height: calc(100vh - 8rem);
}
</style>
