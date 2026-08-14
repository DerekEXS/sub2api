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
            :disabled="agentStatus === 'starting' || agentStatus === 'running'"
            class="px-4 py-2 bg-primary-600 hover:bg-primary-700 text-white rounded-md disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
          >
            <span v-if="agentStatus === 'starting'" class="inline-block animate-spin mr-2">⚪</span>
            {{ t('agentService.startButton') }}
          </button>

          <button
            @click="stopAgent"
            :disabled="agentStatus !== 'running'"
            class="px-4 py-2 bg-red-600 hover:bg-red-700 text-white rounded-md disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
          >
            <span v-if="agentStatus === 'stopping'" class="inline-block animate-spin mr-2">⚪</span>
            {{ t('agentService.stopButton') }}
          </button>
        </div>
      </div>

      <!-- Agent UI Frame -->
      <div v-if="agentUrl && agentStatus === 'running'" class="bg-white dark:bg-dark-800 rounded-lg shadow-md p-4">
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
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import { agentAPI } from '@/api/agent'

const { t } = useI18n()

type AgentUiStatus = 'not_started' | 'starting' | 'running' | 'stopping' | 'error'

const agentStatus = ref<AgentUiStatus>('not_started')
const agentUrl = ref('')
const errorMessage = ref('')
let pollTimer: ReturnType<typeof setInterval> | null = null

const statusText = computed(() => {
  switch (agentStatus.value) {
    case 'not_started':
      return t('agentService.statusNotStarted')
    case 'starting':
      return t('agentService.statusStarting')
    case 'running':
      return t('agentService.statusRunning')
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

const stopPolling = () => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

const startPolling = () => {
  stopPolling()
  pollTimer = setInterval(async () => {
    try {
      const state = await agentAPI.status()
      if (state.status === 'running') {
        agentStatus.value = 'running'
        agentUrl.value = state.agent_url || ''
      } else if (state.status === 'stopped' || state.status === 'not_started') {
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
    if (state.status === 'running') {
      agentStatus.value = 'running'
      agentUrl.value = state.agent_url || ''
      startPolling()
    } else if (state.status === 'stopped') {
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
    if (state.status === 'running') {
      agentStatus.value = 'running'
      agentUrl.value = state.agent_url || ''
      startPolling()
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
  if (agentStatus.value !== 'running') return

  agentStatus.value = 'stopping'
  errorMessage.value = ''

  try {
    await agentAPI.stop()
    agentStatus.value = 'not_started'
    agentUrl.value = ''
    stopPolling()
  } catch (error: any) {
    console.error('Failed to stop agent:', error)
    agentStatus.value = 'running'
    errorMessage.value = error?.message || t('agentService.stopError')
  }
}

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
