<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { ask, askStream, getHealth, getInfo, getStats } from './api/client'
import type { BackendInfo, ChatMessage, PoolStats, RequestMetrics } from './api/client'

type Message = { id: string; role: 'user' | 'assistant'; content: string; state?: 'streaming' | 'done' | 'error' | 'stopped'; createdAt: number; metrics?: RequestMetrics }
type Conversation = { id: string; title: string; createdAt: number; messages: Message[] }

function loadChats(): Conversation[] {
  try {
    const raw = JSON.parse(localStorage.getItem('go-ai-studio-conversations') || '[]') as unknown
    return Array.isArray(raw) ? raw as Conversation[] : []
  } catch { return [] }
}

const stored = loadChats()
const chats = ref<Conversation[]>(stored.length ? stored : [{ id: crypto.randomUUID(), title: '新对话', createdAt: Date.now(), messages: [] }])
const activeId = ref(chats.value[0].id)
const activeChat = computed(() => chats.value.find(chat => chat.id === activeId.value) ?? chats.value[0])
const prompt = ref('')
const isLoading = ref(false)
const useStreaming = ref(true)
const info = ref<BackendInfo | null>(null)
const stats = ref<PoolStats | null>(null)
const healthy = ref(false)
const sidebarOpen = ref(false)
const notice = ref('')
const history = ref<HTMLElement | null>(null)
let controller: AbortController | null = null
let timer: ReturnType<typeof setInterval> | undefined
let noticeTimer: ReturnType<typeof setTimeout> | undefined

const backendLabel = computed(() => info.value?.backend === 'dynamo' ? info.value.model : 'Mock Backend')
const streamAvailable = computed(() => info.value?.streaming === true)
const canSend = computed(() => !!prompt.value.trim() && !isLoading.value)
const examples = [
  { tag: 'Go 并发', title: '解释 Goroutine 和 C++20 协程的区别', icon: '⌁' },
  { tag: '代码分析', title: '如何使用 Context 实现超时取消？', icon: '{ }' },
  { tag: '推理服务', title: '设计一个高并发 LLM 推理网关', icon: '⚡' },
  { tag: '架构实践', title: '解释 Worker Pool 的背压机制', icon: '◇' },
]

watch(chats, next => {
  try { localStorage.setItem('go-ai-studio-conversations', JSON.stringify(next)) } catch { /* quota/private mode */ }
}, { deep: true })

function createChat(): void {
  const chat: Conversation = { id: crypto.randomUUID(), title: '新对话', createdAt: Date.now(), messages: [] }
  chats.value.unshift(chat)
  activeId.value = chat.id
  sidebarOpen.value = false
  prompt.value = ''
}

function switchChat(id: string): void {
  activeId.value = id
  sidebarOpen.value = false
  scrollToBottom()
}

function deleteChat(id: string): void {
  chats.value = chats.value.filter(chat => chat.id !== id)
  if (!chats.value.length) {
    createChat()
  } else if (activeId.value === id) {
    activeId.value = chats.value[0].id
  }
}

function scrollToBottom(): void {
  void nextTick(() => {
    if (history.value) history.value.scrollTop = history.value.scrollHeight
  })
}

function flash(message: string): void {
  notice.value = message
  if (noticeTimer) clearTimeout(noticeTimer)
  noticeTimer = setTimeout(() => { notice.value = '' }, 3200)
}

async function refreshStatus(): Promise<void> {
  const [health, pool] = await Promise.allSettled([getHealth(), getStats()])
  healthy.value = health.status === 'fulfilled' ? health.value : false
  stats.value = pool.status === 'fulfilled' ? pool.value : null
  try { info.value = await getInfo() } catch { info.value = null }
}

async function copyText(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    flash('已复制到剪贴板')
  } catch {
    flash('复制失败，请手动选择文本')
  }
}

function stopGeneration(): void {
  controller?.abort()
}

async function submit(): Promise<void> {
  const text = prompt.value.trim()
  if (!text || isLoading.value) return
  const chat = activeChat.value
  if (!chat) return
  if (chat.messages.length === 0) chat.title = text.slice(0, 26) + (text.length > 26 ? '…' : '')
  chat.messages.push({ id: crypto.randomUUID(), role: 'user', content: text, createdAt: Date.now() })
  const context: ChatMessage[] = chat.messages
    .filter(message => message.content.trim() && (message.role === 'user' || message.state === 'done'))
    .map(message => ({ role: message.role, content: message.content.trim() }))
  const answer: Message = { id: crypto.randomUUID(), role: 'assistant', content: '', state: 'streaming', createdAt: Date.now() }
  chat.messages.push(answer)
  prompt.value = ''
  isLoading.value = true
  controller = new AbortController()
  scrollToBottom()

  try {
    if (useStreaming.value && streamAvailable.value) {
      await askStream(chat.id, text, context, controller.signal, (event) => {
        if (event.type === 'token') {
          answer.content += event.text
          scrollToBottom()
        } else if (event.type === 'done') {
          answer.metrics = {
            requestId: event.requestId ?? '',
            queueMs: event.queueMs ?? 0,
            durationMs: event.durationMs ?? 0,
            model: event.model,
          }
        } else if (event.type === 'error') {
          throw new Error(event.message)
        }
      })
    } else {
      const result = await ask(chat.id, text, context, controller.signal)
      answer.content = result.reply
      answer.metrics = {
        requestId: result.requestId,
        queueMs: result.queueMs,
        durationMs: result.durationMs,
        model: result.model,
      }
    }
    answer.state = 'done'
  } catch (error) {
    if (controller.signal.aborted) {
      answer.state = 'stopped'
      if (!answer.content) answer.content = '生成已取消。'
    } else {
      answer.state = 'error'
      const message = error instanceof Error ? error.message : '未知错误'
      answer.content += `${answer.content ? '\n\n' : ''}请求失败：${message}`
    }
  } finally {
    isLoading.value = false
    controller = null
    scrollToBottom()
    void refreshStatus()
  }
}

function onComposerKeydown(event: KeyboardEvent): void {
  if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
    event.preventDefault()
    void submit()
  }
}

function fillExample(text: string): void {
  prompt.value = text
  document.querySelector<HTMLTextAreaElement>('.prompt-input')?.focus()
}

onMounted(() => {
  void refreshStatus()
  timer = setInterval(() => { void refreshStatus() }, 5000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
  if (noticeTimer) clearTimeout(noticeTimer)
  controller?.abort()
})
</script>

<template>
  <div class="app-shell">
    <div v-if="sidebarOpen" class="mobile-overlay" @click="sidebarOpen = false"></div>
    <aside class="sidebar" :class="{ 'sidebar-open': sidebarOpen }">
      <div class="brand">
        <div class="brand-logo"><span>G</span><span class="logo-accent">.</span></div>
        <div class="brand-copy"><strong>GO AI STUDIO</strong><span>INFERENCE WORKSPACE</span></div>
      </div>

      <button class="new-chat" type="button" @click="createChat">
        <span class="plus-icon">＋</span><span>新建对话</span><span class="new-key">NEW</span>
      </button>

      <div class="sidebar-section-label">YOUR CONVERSATIONS <span>{{ chats.length }}</span></div>
      <div class="conversation-list">
        <div v-for="chat in chats" :key="chat.id" class="conversation-item" :class="{ active: chat.id === activeId }">
          <button type="button" class="conversation-select" @click="switchChat(chat.id)">
            <span class="chat-bubble-icon">▤</span>
            <span class="conversation-name">{{ chat.title }}</span>
          </button>
          <button type="button" class="remove-chat" title="删除对话" aria-label="删除对话" @click="deleteChat(chat.id)">×</button>
        </div>
      </div>

      <div class="sidebar-bottom">
        <div class="sidebar-bottom-divider"></div>
        <div class="stack-label">CURRENT STACK</div>
        <div class="stack-chip-row"><span>Go</span><span>Vue 3</span><span>Dynamo</span></div>
        <div class="sidebar-footer"><span class="footer-circle"></span> LOCAL DEVELOPMENT <span>v1.0</span></div>
      </div>
    </aside>

    <main class="workspace">
      <header class="topbar">
        <div class="topbar-left">
          <button class="mobile-menu" aria-label="打开菜单" @click="sidebarOpen = true">☰</button>
          <div class="breadcrumb"><span>Workspace</span><span class="slash">/</span><strong>AI Chat</strong></div>
        </div>
        <div class="topbar-right">
          <div class="model-pill"><span class="model-dot"></span><span>{{ backendLabel }}</span></div>
          <span class="topbar-separator"></span>
          <div class="health-pill" :class="{ offline: !healthy }"><span class="health-dot"></span>{{ healthy ? '服务运行中' : '服务未连接' }}</div>
        </div>
      </header>

      <div class="workarea">
        <section class="chat-pane">
          <div class="conversation-header">
            <div><div class="eyebrow">YOUR AI ASSISTANT</div><h1>AI 推理工作台<span class="title-star">✦</span></h1><p>由 Go 并发服务驱动的本地 AI 对话空间</p></div>
            <div class="conversation-header-icon">⌘</div>
          </div>

          <div ref="history" class="chat-scroll">
            <div v-if="activeChat?.messages.length === 0" class="empty-state">
              <div class="empty-mark"><span>G</span><i>✦</i></div>
              <div class="welcome-kicker">READY WHEN YOU ARE</div>
              <h2>今天想探索什么？</h2>
              <p>从 Go 并发，到推理架构。发出你的第一个问题，看看整个调用链如何运转。</p>
              <div class="example-grid">
                <button v-for="example in examples" :key="example.tag" type="button" class="example-card" @click="fillExample(example.title)">
                  <div class="example-top"><span class="example-icon">{{ example.icon }}</span><span class="example-tag">{{ example.tag }}</span></div>
                  <span class="example-title">{{ example.title }}</span>
                  <span class="example-arrow">↗</span>
                </button>
              </div>
            </div>
            <div v-else class="messages">
              <div v-for="message in activeChat.messages" :key="message.id" class="message" :class="message.role">
                <div class="avatar" :class="message.role">{{ message.role === 'user' ? 'U' : 'G' }}</div>
                <div class="message-content">
                  <div class="message-meta"><strong>{{ message.role === 'user' ? '你' : 'Go AI' }}</strong><span>{{ new Date(message.createdAt).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' }) }}</span></div>
                  <div class="message-body">{{ message.content }}<span v-if="message.state === 'streaming'" class="cursor">▍</span></div>
                  <div v-if="message.state === 'stopped'" class="message-footnote">已停止生成</div>
                  <div v-if="message.metrics" class="message-footnote">请求 {{ message.metrics.requestId }} · 排队 {{ message.metrics.queueMs }}ms · 总耗时 {{ message.metrics.durationMs }}ms</div>
                  <button v-if="message.role === 'assistant' && message.content" type="button" class="copy-action" @click="copyText(message.content)">⧉ 复制回复</button>
                </div>
              </div>
            </div>
          </div>

          <div class="composer-container">
            <div class="composer">
              <textarea v-model="prompt" class="prompt-input" placeholder="输入你想问的问题，例如：Go 的 GMP 调度模型是什么？" rows="2" :disabled="isLoading" @keydown="onComposerKeydown"></textarea>
              <div class="composer-toolbar">
                <div class="composer-options"><span class="prompt-indicator">⌁</span><span>{{ backendLabel }}</span><span class="toolbar-divider"></span><label class="stream-toggle"><input v-model="useStreaming" type="checkbox" :disabled="!streamAvailable || isLoading" /><span>流式输出</span></label></div>
                <button v-if="isLoading" type="button" class="stop-button" @click="stopGeneration">■ <span>停止生成</span></button>
                <button v-else type="button" class="send-button" :disabled="!canSend" @click="submit">发送消息 <span>↑</span></button>
              </div>
            </div>
            <div class="composer-help">Enter 发送 · Shift + Enter 换行 · 当前会话会自动携带历史上下文</div>
          </div>
        </section>

        <aside class="insights-pane">
          <div class="insights-header"><span>LIVE MONITORING</span><span class="live-point">● LIVE</span></div>
          <div class="insights-title">系统运行概况</div>
          <p class="insights-intro">实时查看 Go 任务池与模型服务的关键状态。</p>

          <div class="stat-card primary-stat"><div class="stat-card-title">WORKER POOL <span>↗</span></div><div class="stat-value">{{ stats?.workers ?? '—' }} <span>WORKERS</span></div><div class="mini-visual"><div v-for="i in 18" :key="i" :style="{ height: `${9 + ((i * 17) % 30)}px` }"></div></div><div class="stat-subtitle"><span class="legend-dot"></span>固定并发执行单元</div></div>
          <div class="stat-grid"><div class="stat-card"><div class="stat-label">待处理队列</div><div class="stat-number">{{ stats?.queued ?? '—' }}</div><div class="stat-caption">/ {{ stats?.queue_capacity ?? '—' }} QUEUE</div></div><div class="stat-card"><div class="stat-label">执行中任务</div><div class="stat-number">{{ stats?.active ?? '—' }}</div><div class="stat-caption">ACTIVE TASKS</div></div></div>
          <div class="stat-grid"><div class="stat-card"><div class="stat-label">已完成任务</div><div class="stat-number">{{ stats?.completed ?? '—' }}</div><div class="stat-caption">{{ stats?.submitted ?? '—' }} SUBMITTED</div></div><div class="stat-card"><div class="stat-label">取消 / 拒绝</div><div class="stat-number">{{ (stats?.canceled ?? 0) + (stats?.rejected ?? 0) }}</div><div class="stat-caption">{{ stats?.canceled ?? '—' }} / {{ stats?.rejected ?? '—' }}</div></div></div>
          <div class="stat-card errors-stat"><div class="stat-row"><span>执行失败</span><strong>{{ stats?.failed ?? '—' }}</strong></div><div class="stat-bar"><span :style="{ width: stats && stats.completed ? `${Math.min(100, stats.failed / stats.completed * 100)}%` : '0%' }"></span></div><div class="stat-caption">FAILED TASKS</div></div>

          <div class="insights-divider"></div>
          <div class="architecture-title">请求执行链路</div>
          <div class="pipeline"><div><span>01</span><strong>Vue / Fetch</strong><small>前端请求</small></div><div><span>02</span><strong>Go HTTP API</strong><small>路由 · 限流</small></div><div><span>03</span><strong>RabbitMQ</strong><small>任务分发</small></div><div><span>04</span><strong>Dynamo</strong><small>GPU 模型推理</small></div></div>
          <div class="insights-note">提示：流式请求通过 Go SSE Handler 直连后端，不进入当前 Worker Pool。后续可升级为统一调度器。</div>
        </aside>
      </div>
      <div v-if="notice" class="toast">{{ notice }}</div>
    </main>
  </div>
</template>
