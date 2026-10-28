import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { KeyboardEvent } from 'react'
import { ask, askStream, getHealth, getInfo, getStats } from './api/client'
import type { BackendInfo, ChatMessage, PoolStats, RequestMetrics } from './api/client'

type Message = {
  id: string
  role: 'user' | 'assistant'
  content: string
  state?: 'streaming' | 'done' | 'error' | 'stopped'
  createdAt: number
  metrics?: RequestMetrics
}

type Conversation = {
  id: string
  title: string
  createdAt: number
  messages: Message[]
}

const STORAGE_KEY = 'go-ai-studio-conversations'

function newID(): string {
  return crypto.randomUUID()
}

function loadChats(): Conversation[] {
  try {
    const raw = JSON.parse(localStorage.getItem(STORAGE_KEY) || '[]') as unknown
    return Array.isArray(raw) ? raw as Conversation[] : []
  } catch {
    return []
  }
}

function formatTime(timestamp: number): string {
  return new Date(timestamp).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
}

const examples = [
  { tag: 'Go 并发', title: '解释 Goroutine 和 C++20 协程的区别', icon: '⌁' },
  { tag: '代码分析', title: '如何使用 Context 实现超时取消？', icon: '{ }' },
  { tag: '推理服务', title: '设计一个高并发 LLM 推理网关', icon: '⚡' },
  { tag: '架构实践', title: '解释 Worker Pool 的背压机制', icon: '◇' },
]

const storedChats = loadChats()
const initialConversations = storedChats.length
  ? storedChats
  : [{ id: newID(), title: '新对话', createdAt: Date.now(), messages: [] }]

export default function App() {
  const [chats, setChats] = useState<Conversation[]>(initialConversations)
  const [activeID, setActiveID] = useState(initialConversations[0].id)
  const [prompt, setPrompt] = useState('')
  const [isLoading, setIsLoading] = useState(false)
  const [useStreaming, setUseStreaming] = useState(true)
  const [info, setInfo] = useState<BackendInfo | null>(null)
  const [stats, setStats] = useState<PoolStats | null>(null)
  const [healthy, setHealthy] = useState(false)
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [notice, setNotice] = useState('')
  const [lastSynced, setLastSynced] = useState<number | null>(null)
  const historyRef = useRef<HTMLDivElement>(null)
  const controllerRef = useRef<AbortController | null>(null)
  const noticeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const activeChat = useMemo(
    () => chats.find(chat => chat.id === activeID) ?? chats[0],
    [activeID, chats],
  )
  const backendLabel = info?.backend === 'dynamo' ? info.model : 'Mock Backend'
  const requestModel = info?.model || 'Qwen/Qwen3-0.6B'
  const streamAvailable = info?.streaming === true
  const canSend = Boolean(prompt.trim()) && !isLoading && healthy

  useEffect(() => {
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(chats))
    } catch {
      // localStorage can be unavailable in private browsing or after quota exhaustion.
    }
  }, [chats])

  const scrollToBottom = useCallback(() => {
    window.requestAnimationFrame(() => {
      if (historyRef.current) historyRef.current.scrollTop = historyRef.current.scrollHeight
    })
  }, [])

  const refreshStatus = useCallback(async () => {
    const [health, pool, backend] = await Promise.allSettled([getHealth(), getStats(), getInfo()])
    setHealthy(health.status === 'fulfilled' ? health.value : false)
    setStats(pool.status === 'fulfilled' ? pool.value : null)
    setInfo(backend.status === 'fulfilled' ? backend.value : null)
    setLastSynced(Date.now())
  }, [])

  useEffect(() => {
    void refreshStatus()
    const timer = window.setInterval(() => { void refreshStatus() }, 5000)
    return () => window.clearInterval(timer)
  }, [refreshStatus])

  useEffect(() => () => {
    controllerRef.current?.abort()
    if (noticeTimerRef.current) window.clearTimeout(noticeTimerRef.current)
  }, [])

  function updateChat(id: string, updater: (chat: Conversation) => Conversation): void {
    setChats(previous => previous.map(chat => chat.id === id ? updater(chat) : chat))
  }

  function updateMessage(chatID: string, messageID: string, updater: (message: Message) => Message): void {
    updateChat(chatID, chat => ({
      ...chat,
      messages: chat.messages.map(message => message.id === messageID ? updater(message) : message),
    }))
  }

  function flash(message: string): void {
    setNotice(message)
    if (noticeTimerRef.current) window.clearTimeout(noticeTimerRef.current)
    noticeTimerRef.current = window.setTimeout(() => setNotice(''), 3200)
  }

  function createChat(): void {
    const chat: Conversation = { id: newID(), title: '新对话', createdAt: Date.now(), messages: [] }
    setChats(previous => [chat, ...previous])
    setActiveID(chat.id)
    setSidebarOpen(false)
    setPrompt('')
  }

  function switchChat(id: string): void {
    setActiveID(id)
    setSidebarOpen(false)
    scrollToBottom()
  }

  function deleteChat(id: string): void {
    setChats(previous => {
      const next = previous.filter(chat => chat.id !== id)
      if (!next.length) {
        const replacement: Conversation = { id: newID(), title: '新对话', createdAt: Date.now(), messages: [] }
        setActiveID(replacement.id)
        return [replacement]
      }
      if (activeID === id) setActiveID(next[0].id)
      return next
    })
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
    controllerRef.current?.abort()
  }

  async function submit(): Promise<void> {
    const text = prompt.trim()
    if (!text || isLoading || !activeChat) return

    const chatID = activeChat.id
    const userMessage: Message = { id: newID(), role: 'user', content: text, createdAt: Date.now() }
    const answer: Message = { id: newID(), role: 'assistant', content: '', state: 'streaming', createdAt: Date.now() }
    const nextMessages = [...activeChat.messages, userMessage]
    const context: ChatMessage[] = nextMessages
      .filter(message => message.content.trim() && (message.role === 'user' || message.state === 'done'))
      .map(message => ({ role: message.role, content: message.content.trim() }))

    updateChat(chatID, chat => ({
      ...chat,
      title: chat.messages.length === 0 ? text.slice(0, 26) + (text.length > 26 ? '…' : '') : chat.title,
      messages: [...chat.messages, userMessage, answer],
    }))
    setPrompt('')
    setIsLoading(true)
    const controller = new AbortController()
    controllerRef.current = controller
    let answerContent = ''
    scrollToBottom()

    try {
      if (useStreaming && streamAvailable) {
        await askStream(requestModel, chatID, context, controller.signal, event => {
          if (event.type === 'token') {
            answerContent += event.text
            updateMessage(chatID, answer.id, message => ({ ...message, content: answerContent }))
            scrollToBottom()
          } else if (event.type === 'done') {
            updateMessage(chatID, answer.id, message => ({
              ...message,
              metrics: {
                requestId: event.requestId ?? '',
                queueMs: event.queueMs ?? 0,
                durationMs: event.durationMs ?? 0,
                model: event.model,
              },
            }))
          } else if (event.type === 'error') {
            throw new Error(event.message)
          }
        })
      } else {
        const result = await ask(requestModel, chatID, context, controller.signal)
        answerContent = result.reply
        updateMessage(chatID, answer.id, message => ({
          ...message,
          content: result.reply,
          metrics: {
            requestId: result.requestId,
            queueMs: result.queueMs,
            durationMs: result.durationMs,
            model: result.model,
          },
        }))
      }
      updateMessage(chatID, answer.id, message => ({ ...message, state: 'done' }))
    } catch (error) {
      if (controller.signal.aborted) {
        updateMessage(chatID, answer.id, message => ({
          ...message,
          state: 'stopped',
          content: answerContent || '生成已取消。',
        }))
      } else {
        const message = error instanceof Error ? error.message : '未知错误'
        updateMessage(chatID, answer.id, current => ({
          ...current,
          state: 'error',
          content: `${answerContent ? `${answerContent}\n\n` : ''}请求失败：${message}`,
        }))
      }
    } finally {
      setIsLoading(false)
      controllerRef.current = null
      scrollToBottom()
      void refreshStatus()
    }
  }

  function onComposerKeydown(event: KeyboardEvent<HTMLTextAreaElement>): void {
    if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault()
      void submit()
    }
  }

  function fillExample(text: string): void {
    setPrompt(text)
    window.setTimeout(() => document.querySelector<HTMLTextAreaElement>('.prompt-input')?.focus(), 0)
  }

  return (
    <div className="app-shell">
      {sidebarOpen && <div className="mobile-overlay" onClick={() => setSidebarOpen(false)} />}
      <aside className={`sidebar${sidebarOpen ? ' sidebar-open' : ''}`}>
        <div className="brand">
          <div className="brand-logo"><span>G</span><span className="logo-accent">.</span></div>
          <div className="brand-copy"><strong>GO AI STUDIO</strong><span>INFERENCE WORKSPACE</span></div>
        </div>

        <button className="new-chat" type="button" onClick={createChat}>
          <span className="plus-icon">＋</span><span>新建对话</span><span className="new-key">NEW</span>
        </button>

        <div className="sidebar-section-label">YOUR CONVERSATIONS <span>{chats.length}</span></div>
        <div className="conversation-list">
          {chats.map(chat => (
            <div key={chat.id} className={`conversation-item${chat.id === activeID ? ' active' : ''}`}>
              <button type="button" className="conversation-select" onClick={() => switchChat(chat.id)}>
                <span className="chat-bubble-icon">▤</span>
                <span className="conversation-name">{chat.title}</span>
              </button>
              <button type="button" className="remove-chat" title="删除对话" aria-label="删除对话" onClick={() => deleteChat(chat.id)}>×</button>
            </div>
          ))}
        </div>

        <div className="sidebar-bottom">
          <div className="sidebar-bottom-divider" />
          <div className="stack-label">CURRENT STACK</div>
          <div className="stack-chip-row"><span>Go</span><span>React</span><span>Dynamo</span></div>
          <div className="sidebar-footer"><span className="footer-circle" /> LOCAL DEVELOPMENT <span>v1.0</span></div>
        </div>
      </aside>

      <main className="workspace">
        <header className="topbar">
          <div className="topbar-left">
            <button className="mobile-menu" aria-label="打开菜单" onClick={() => setSidebarOpen(true)}>☰</button>
            <div className="breadcrumb"><span>Workspace</span><span className="slash">/</span><strong>AI Chat</strong></div>
          </div>
          <div className="topbar-right">
            <div className="model-pill"><span className="model-dot" /><span>{backendLabel}</span></div>
            <span className="topbar-separator" />
            <div className={`health-pill${healthy ? '' : ' offline'}`}><span className="health-dot" />{healthy ? '服务运行中' : '服务未连接'}</div>
          </div>
        </header>

        <div className="workarea">
          <section className="chat-pane">
            <div className="conversation-header">
              <div><div className="eyebrow">YOUR AI ASSISTANT</div><h1>AI 推理工作台<span className="title-star">✦</span></h1><p>由 Go 并发服务驱动的本地 AI 对话空间</p></div>
              <div className="conversation-header-icon">⌘</div>
            </div>

            <div ref={historyRef} className="chat-scroll">
              {activeChat?.messages.length === 0 ? (
                <div className="empty-state">
                  <div className="empty-mark"><span>G</span><i>✦</i></div>
                  <div className="welcome-kicker">READY WHEN YOU ARE</div>
                  <h2>今天想探索什么？</h2>
                  <p>从 Go 并发，到推理架构。发出你的第一个问题，看看整个调用链如何运转。</p>
                  <div className="example-grid">
                    {examples.map(example => (
                      <button key={example.tag} type="button" className="example-card" onClick={() => fillExample(example.title)}>
                        <div className="example-top"><span className="example-icon">{example.icon}</span><span className="example-tag">{example.tag}</span></div>
                        <span className="example-title">{example.title}</span>
                        <span className="example-arrow">↗</span>
                      </button>
                    ))}
                  </div>
                </div>
              ) : (
                <div className="messages">
                  {activeChat?.messages.map(message => (
                    <div key={message.id} className={`message ${message.role}`}>
                      <div className={`avatar ${message.role}`}>{message.role === 'user' ? 'U' : 'G'}</div>
                      <div className="message-content">
                        <div className="message-meta"><strong>{message.role === 'user' ? '你' : 'Go AI'}</strong><span>{formatTime(message.createdAt)}</span></div>
                        <div className="message-body">{message.content}{message.state === 'streaming' && <span className="cursor">▍</span>}</div>
                        {message.state === 'stopped' && <div className="message-footnote">已停止生成</div>}
                        {message.metrics && <div className="message-footnote">请求 {message.metrics.requestId} · 排队 {message.metrics.queueMs}ms · 总耗时 {message.metrics.durationMs}ms</div>}
                        {message.role === 'assistant' && message.content && <button type="button" className="copy-action" onClick={() => void copyText(message.content)}>⧉ 复制回复</button>}
                      </div>
                    </div>
                  ))}
                </div>
              )}
            </div>

            <div className="composer-container">
              <div className="composer">
                <textarea value={prompt} className="prompt-input" placeholder={healthy ? '输入你想问的问题，例如：Go 的 GMP 调度模型是什么？' : '等待 Go API 服务就绪…'} rows={2} disabled={isLoading || !healthy} aria-label="输入问题" onChange={event => setPrompt(event.target.value)} onKeyDown={onComposerKeydown} />
                <div className="composer-toolbar">
                  <div className="composer-options"><span className="prompt-indicator">⌁</span><span>{backendLabel}</span><span className="toolbar-divider" /><label className="stream-toggle"><input checked={useStreaming} type="checkbox" disabled={!streamAvailable || isLoading || !healthy} onChange={event => setUseStreaming(event.target.checked)} /><span>流式输出</span></label><span className="prompt-count">{prompt.length}</span></div>
                  {isLoading ? <button type="button" className="stop-button" onClick={stopGeneration}>■ <span>停止生成</span></button> : <button type="button" className="send-button" disabled={!canSend} onClick={() => void submit()}>发送消息 <span>↑</span></button>}
                </div>
              </div>
              <div className="composer-help">Enter 发送 · Shift + Enter 换行 · 当前会话会自动携带历史上下文</div>
            </div>
          </section>

          <aside className="insights-pane">
            <div className="insights-header"><span>LIVE MONITORING</span><span className={`live-point${healthy ? '' : ' live-point-off'}`}>● {healthy ? 'LIVE' : 'OFFLINE'}</span></div>
            <div className="insights-title">系统运行概况</div>
            <p className="insights-intro">实时查看 Go 任务池与模型服务的关键状态。</p>
            <div className="sync-status"><span className={`sync-dot${healthy ? '' : ' sync-dot-off'}`} />{lastSynced ? `最近同步 ${formatTime(lastSynced)}` : '正在连接服务…'}</div>

            <div className="stat-card primary-stat"><div className="stat-card-title">WORKER POOL <span>↗</span></div><div className="stat-value">{stats?.workers ?? '—'} <span>WORKERS</span></div><div className="mini-visual">{Array.from({ length: 18 }, (_, index) => <div key={index} style={{ height: `${9 + ((index * 17) % 30)}px` }} />)}</div><div className="stat-subtitle"><span className="legend-dot" />固定并发执行单元</div></div>
            <div className="stat-grid"><div className="stat-card"><div className="stat-label">待处理队列</div><div className="stat-number">{stats?.queued ?? '—'}</div><div className="stat-caption">/ {stats?.queue_capacity ?? '—'} QUEUE</div></div><div className="stat-card"><div className="stat-label">执行中任务</div><div className="stat-number">{stats?.active ?? '—'}</div><div className="stat-caption">ACTIVE TASKS</div></div></div>
            <div className="stat-grid"><div className="stat-card"><div className="stat-label">已完成任务</div><div className="stat-number">{stats?.completed ?? '—'}</div><div className="stat-caption">{stats?.submitted ?? '—'} SUBMITTED</div></div><div className="stat-card"><div className="stat-label">取消 / 拒绝</div><div className="stat-number">{(stats?.canceled ?? 0) + (stats?.rejected ?? 0)}</div><div className="stat-caption">{stats?.canceled ?? '—'} / {stats?.rejected ?? '—'}</div></div></div>
            <div className="stat-card errors-stat"><div className="stat-row"><span>执行失败</span><strong>{stats?.failed ?? '—'}</strong></div><div className="stat-bar"><span style={{ width: stats && stats.completed ? `${Math.min(100, stats.failed / stats.completed * 100)}%` : '0%' }} /></div><div className="stat-caption">FAILED TASKS</div></div>

            <div className="insights-divider" />
            <div className="architecture-title">请求执行链路</div>
            <div className="pipeline"><div><span>01</span><strong>React / Fetch</strong><small>前端请求</small></div><div><span>02</span><strong>Go HTTP API</strong><small>路由 · 限流</small></div><div><span>03</span><strong>RabbitMQ</strong><small>任务分发</small></div><div><span>04</span><strong>Dynamo</strong><small>GPU 模型推理</small></div></div>
            <div className="insights-note">提示：流式请求通过 Go SSE Handler 直连后端，不进入当前 Worker Pool。后续可升级为统一调度器。</div>
          </aside>
        </div>
        {notice && <div className="toast">{notice}</div>}
      </main>
    </div>
  )
}
