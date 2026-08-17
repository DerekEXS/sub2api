import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import router from './router'
import i18n, { initI18n } from './i18n'
import { useAppStore } from '@/stores/app'
import { updateFavicon } from '@/utils/branding'
import { isIOSDevice } from '@/utils/device'
import './style.css'

function initIOSViewportZoomFix() {
  // iOS Safari 在输入框字号小于 16px 时聚焦会自动放大页面，且失焦后不会恢复。
  // 限制 maximum-scale 可阻止该行为；iOS 10+ 用户仍可双指手动缩放，不影响可访问性。
  // 仅在 iOS 设备上注入，避免影响 Android Chrome 的手动缩放能力。
  if (!isIOSDevice()) return

  const viewport = document.querySelector('meta[name="viewport"]')
  if (!viewport) return

  const content = viewport.getAttribute('content') || ''
  if (/maximum-scale/i.test(content)) return
  viewport.setAttribute('content', `${content}, maximum-scale=1.0`)
}

function initThemeClass() {
  const savedTheme = localStorage.getItem('theme')
  const shouldUseDark =
    savedTheme === 'dark' ||
    (!savedTheme && window.matchMedia('(prefers-color-scheme: dark)').matches)
  document.documentElement.classList.toggle('dark', shouldUseDark)
}

async function bootstrap() {
  // Apply theme class globally before app mount to keep all routes consistent.
  initThemeClass()
  initIOSViewportZoomFix()

  const app = createApp(App)
  const pinia = createPinia()
  app.use(pinia)

  // #issue4：全局错误处理器——任何组件渲染/方法抛错时显式记录到 console + body，
  // 避免静默"黑屏"（Vue 默认吞掉渲染异常只留空白页）。便于用户 F12 定位根因。
  app.config.errorHandler = (err, _instance, info) => {
    console.error('[Vue errorHandler]', info, err)
    // 在页面顶部贴一条错误提示，让用户看到而非黑屏
    try {
      const banner = document.createElement('div')
      banner.style.cssText =
        'position:fixed;top:0;left:0;right:0;z-index:99999;background:#b91c1c;color:#fff;padding:8px 12px;font:13px/1.5 monospace;white-space:pre-wrap;max-height:40vh;overflow:auto'
      banner.textContent = `[页面渲染错误] ${info}\n${err instanceof Error ? err.stack || err.message : String(err)}\n请截图此信息反馈，或刷新页面重试。`
      document.body.appendChild(banner)
    } catch { /* ignore */ }
  }

  // Initialize settings from injected config BEFORE mounting (prevents flash)
  // This must happen after pinia is installed but before router and i18n
  const appStore = useAppStore()
  appStore.initFromInjectedConfig()

  // Set document title immediately after config is loaded
  if (appStore.siteName && appStore.siteName !== 'Sub2API') {
    document.title = `${appStore.siteName} - AI API Gateway`
  }
  updateFavicon(appStore.siteLogo)

  await initI18n()

  app.use(router)
  app.use(i18n)

  // 等待路由器完成初始导航后再挂载，避免竞态条件导致的空白渲染
  await router.isReady()
  app.mount('#app')
}

bootstrap()
