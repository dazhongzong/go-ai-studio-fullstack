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

export type StreamEvent =
  | { type: 'token'; text: string }
  | { type: 'done'; evalCount?: number; requestId?: string; queueMs?: number; durationMs?: number; model?: string }
  | { type: 'error'; message: string; requestId?: string; durationMs?: number }

async function requireOK(response: Response): Promise<void> {
  if (response.ok) return
  let message = `HTTP ${response.status}`
  try {
    const result = (await response.json()) as ApiErrorBody
    if (result.error) message = result.error
  } catch {
    // Preserve HTTP error if server returns non-JSON.
  }
  throw new Error(message)
}

export async function getStats(): Promise<PoolStats> {
  const response = await fetch('/api/stats', { cache: 'no-store' })
  await requireOK(response)
  return (await response.json()) as PoolStats
}

export async function getInfo(): Promise<BackendInfo> {
  const response = await fetch('/api/info', { cache: 'no-store' })
  await requireOK(response)
  return (await response.json()) as BackendInfo
}

export async function getHealth(): Promise<boolean> {
  try {
    const response = await fetch('/api/healthz', { cache: 'no-store' })
    return response.ok
  } catch {
    return false
  }
}

export async function ask(conversationId: string, prompt: string, messages: ChatMessage[], signal: AbortSignal): Promise<AskResult> {
  const response = await fetch('/api/ask', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ conversation_id: conversationId, prompt, messages }),
    signal,
  })
  await requireOK(response)
  const body = (await response.json()) as {
    reply: string
    request_id: string
    queue_ms: number
    duration_ms: number
    model: string
  }
  return {
    reply: body.reply,
    requestId: body.request_id,
    queueMs: body.queue_ms,
    durationMs: body.duration_ms,
    model: body.model,
  }
}

// POST + SSE cannot use EventSource; we parse fetch ReadableStream.
// The buffer handles both UTF-8 splits and SSE frames crossing network chunks.
export async function askStream(
  conversationId: string,
  prompt: string,
  messages: ChatMessage[],
  signal: AbortSignal,
  onEvent: (event: StreamEvent) => void,
): Promise<void> {
  const response = await fetch('/api/ask/stream', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ conversation_id: conversationId, prompt, messages }),
    signal,
  })
  await requireOK(response)
  if (!response.body) throw new Error('浏览器不支持流式响应')

  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffered = ''
  let done = false

  const parseFrame = (frame: string): void => {
    let eventName = 'message'
    const parts: string[] = []
    for (const line of frame.split('\n')) {
      if (line.startsWith('event:')) eventName = line.slice(6).trim()
      if (line.startsWith('data:')) parts.push(line.slice(5).trimStart())
    }
    if (!parts.length) return
    const payload = JSON.parse(parts.join('\n')) as Record<string, unknown>
    if (eventName === 'token') {
      onEvent({ type: 'token', text: String(payload.text ?? '') })
    } else if (eventName === 'done') {
      done = true
      onEvent({
        type: 'done',
        evalCount: Number(payload.eval_count ?? 0),
        requestId: String(payload.request_id ?? ''),
        queueMs: Number(payload.queue_ms ?? 0),
        durationMs: Number(payload.duration_ms ?? 0),
        model: String(payload.model ?? ''),
      })
    } else if (eventName === 'error') {
      done = true
      onEvent({
        type: 'error',
        message: String(payload.message ?? '流式生成失败'),
        requestId: String(payload.request_id ?? ''),
        durationMs: Number(payload.duration_ms ?? 0),
      })
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
