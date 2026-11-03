export interface PoolStats {
  workers: number
  queue_capacity: number
  queued: number
  active: number
  submitted: number
  completed: number
  failed: number
  canceled: number
  rejected: number
  max_queued: number
  total_queue_wait_ms: number
}

export interface BackendInfo {
  backend: 'mock' | 'dynamo'
  model: string
  streaming: boolean
}

interface ApiErrorBody { error?: string }

export class ApiError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

export interface RequestMetrics {
  requestId: string
  queueMs: number
  durationMs: number
  model?: string
}

export interface ChatMessage {
  role: 'user' | 'assistant'
  content: string
}

export interface AskResult {
  reply: string
  requestId: string
  queueMs: number
  durationMs: number
  model: string
}

export interface ConversationSummary {
  id: string
  title: string
  created_at: string
  updated_at: string
  message_count: number
}

export interface CurrentUser {
  id: string
  username: string
  email: string
  role: string
  status: string
  created_at: string
}

interface AuthTokens {
  access_token: string
  refresh_token: string
  token_type: string
  expires_in: number
  user: CurrentUser
}

export type StreamEvent =
  | { type: 'token'; text: string }
  | { type: 'done'; evalCount?: number; requestId?: string; queueMs?: number; durationMs?: number; model?: string }
  | { type: 'error'; message: string; requestId?: string; durationMs?: number }

const apiKey = (import.meta.env.VITE_API_KEY ?? '').trim()
const clientIDStorageKey = 'go-ai-studio-client-id'
const accessTokenStorageKey = 'go-ai-studio-access-token'
const refreshTokenStorageKey = 'go-ai-studio-refresh-token'

function getClientID(): string {
  try {
    const existing = localStorage.getItem(clientIDStorageKey)?.trim()
    if (existing) return existing
    const value = crypto.randomUUID()
    localStorage.setItem(clientIDStorageKey, value)
    return value
  } catch {
    return `client-${Date.now()}-${Math.random().toString(16).slice(2)}`
  }
}

const clientID = getClientID()

function apiHeaders(contentType = false): HeadersInit {
  const headers: Record<string, string> = { 'X-Client-ID': clientID }
  if (contentType) headers['Content-Type'] = 'application/json'
  if (apiKey) headers['X-API-Key'] = apiKey
  const accessToken = localStorage.getItem(accessTokenStorageKey)?.trim()
  if (accessToken) headers.Authorization = `Bearer ${accessToken}`
  return headers
}

async function requireOK(response: Response): Promise<void> {
  if (response.ok) return
  let message = `HTTP ${response.status}`
  try {
    const result = (await response.json()) as ApiErrorBody
    if (result.error) message = result.error
  } catch {
    // Preserve HTTP error if server returns non-JSON.
  }
  throw new ApiError(response.status, message)
}

function saveTokens(tokens: AuthTokens): CurrentUser {
  localStorage.setItem(accessTokenStorageKey, tokens.access_token)
  localStorage.setItem(refreshTokenStorageKey, tokens.refresh_token)
  return tokens.user
}

function clearTokens(): void {
  localStorage.removeItem(accessTokenStorageKey)
  localStorage.removeItem(refreshTokenStorageKey)
}

let refreshInFlight: Promise<boolean> | null = null

async function refreshAccessToken(): Promise<boolean> {
  const refreshToken = localStorage.getItem(refreshTokenStorageKey)?.trim()
  if (!refreshToken) return false
  if (refreshInFlight) return refreshInFlight

  refreshInFlight = (async () => {
    try {
      const response = await fetch('/api/auth/refresh', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: refreshToken }),
      })
      if (!response.ok) {
        clearTokens()
        return false
      }
      saveTokens((await response.json()) as AuthTokens)
      return true
    } catch {
      return false
    } finally {
      refreshInFlight = null
    }
  })()
  return refreshInFlight
}

async function apiFetch(input: RequestInfo | URL, init: RequestInit = {}, retry = true): Promise<Response> {
  const response = await fetch(input, { ...init, headers: { ...apiHeaders(Boolean(init.body)), ...init.headers } })
  if (response.status === 401 && retry && !apiKey && await refreshAccessToken()) {
    return apiFetch(input, init, false)
  }
  return response
}

export async function getCurrentUser(): Promise<{ enabled: boolean; user: CurrentUser | null }> {
  // An explicitly configured API key is an alternative machine-client mode;
  // it should not force a browser login screen.
  if (apiKey) return { enabled: false, user: null }
  const response = await apiFetch('/api/auth/me')
  if (response.status === 404) return { enabled: false, user: null }
  if (response.status === 401) return { enabled: true, user: null }
  await requireOK(response)
  return { enabled: true, user: (await response.json()) as CurrentUser }
}

export async function login(loginValue: string, password: string): Promise<CurrentUser> {
  const response = await fetch('/api/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ login: loginValue, password }),
  })
  await requireOK(response)
  return saveTokens((await response.json()) as AuthTokens)
}

export async function register(username: string, email: string, password: string): Promise<CurrentUser> {
  const response = await fetch('/api/auth/register', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, email, password }),
  })
  await requireOK(response)
  return saveTokens((await response.json()) as AuthTokens)
}

export async function logout(): Promise<void> {
  const refreshToken = localStorage.getItem(refreshTokenStorageKey)?.trim()
  try {
    if (refreshToken) {
      await fetch('/api/auth/logout', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: refreshToken }),
      })
    }
  } finally {
    clearTokens()
  }
}

export async function getStats(): Promise<PoolStats> {
  const response = await apiFetch('/api/stats', { cache: 'no-store' })
  await requireOK(response)
  return (await response.json()) as PoolStats
}

export async function getInfo(): Promise<BackendInfo> {
  const response = await apiFetch('/api/info', { cache: 'no-store' })
  await requireOK(response)
  return (await response.json()) as BackendInfo
}

export async function getHealth(): Promise<boolean> {
  try {
    const response = await apiFetch('/api/readyz', { cache: 'no-store' })
    return response.ok
  } catch {
    return false
  }
}

export async function listConversations(): Promise<ConversationSummary[]> {
  const response = await apiFetch('/api/conversations', { cache: 'no-store' })
  await requireOK(response)
  return (await response.json()) as ConversationSummary[]
}

export async function deleteConversation(conversationId: string): Promise<void> {
  const response = await apiFetch(`/api/conversations/${encodeURIComponent(conversationId)}`, {
    method: 'DELETE',
  })
  await requireOK(response)
}

export async function ask(model: string, conversationId: string, messages: ChatMessage[], signal: AbortSignal): Promise<AskResult> {
  const response = await apiFetch('/api/v1/chat/completions', {
    method: 'POST',
    body: JSON.stringify({ conversation_id: conversationId, model, messages, stream: false }),
    signal,
  })
  await requireOK(response)
  const body = (await response.json()) as {
    id: string
    model: string
    choices: Array<{ message?: { content?: string } }>
  }
  return {
    reply: body.choices[0]?.message?.content ?? '',
    requestId: body.id,
    queueMs: 0,
    durationMs: 0,
    model: body.model,
  }
}

// POST + SSE cannot use EventSource; we parse fetch ReadableStream.
// The buffer handles both UTF-8 splits and SSE frames crossing network chunks.
export async function askStream(
  model: string,
  conversationId: string,
  messages: ChatMessage[],
  signal: AbortSignal,
  onEvent: (event: StreamEvent) => void,
): Promise<void> {
  const response = await apiFetch('/api/v1/chat/completions', {
    method: 'POST',
    body: JSON.stringify({ conversation_id: conversationId, model, messages, stream: true }),
    signal,
  })
  await requireOK(response)
  if (!response.body) throw new Error('浏览器不支持流式响应')

  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffered = ''
  let done = false

  const parseFrame = (frame: string): void => {
    const parts: string[] = []
    for (const line of frame.split('\n')) {
      if (line.startsWith('data:')) parts.push(line.slice(5).trimStart())
    }
    if (!parts.length) return
    const data = parts.join('\n')
    if (data === '[DONE]') {
      done = true
      return
    }
    const payload = JSON.parse(data) as {
      id?: string
      model?: string
      error?: { message?: string }
      choices?: Array<{
        delta?: { content?: string }
        finish_reason?: string | null
      }>
    }
    if (payload.error) {
      done = true
      onEvent({ type: 'error', message: payload.error.message ?? '流式生成失败', requestId: payload.id })
      return
    }
    const choice = payload.choices?.[0]
    const text = choice?.delta?.content ?? ''
    if (text) onEvent({ type: 'token', text })
    if (choice?.finish_reason) {
      done = true
      onEvent({ type: 'done', requestId: payload.id, model: payload.model })
    }
  }

  const consumeFrames = (): void => {
    // Normalize CRLF for network/proxy compatibility.
    // A trailing CR can be completed by LF in the next chunk.
    buffered = buffered.replace(/\r\n/g, '\n')
    let separator: number
    while ((separator = buffered.indexOf('\n\n')) !== -1) {
      const frame = buffered.slice(0, separator)
      buffered = buffered.slice(separator + 2)
      if (frame) parseFrame(frame)
    }
  }

  try {
    while (true) {
      const { done: eof, value } = await reader.read()
      if (eof) break
      buffered += decoder.decode(value, { stream: true })
      consumeFrames()
    }
    buffered += decoder.decode()
    consumeFrames()
    if (buffered.trim()) parseFrame(buffered.trim())
    if (!done && !signal.aborted) throw new Error('连接已结束，但没有收到 done 事件')
  } finally {
    reader.releaseLock()
  }
}
