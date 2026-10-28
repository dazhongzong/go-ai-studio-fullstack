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

const apiKey = (import.meta.env.VITE_API_KEY ?? '').trim()

function apiHeaders(contentType = false): HeadersInit {
  const headers: Record<string, string> = {}
  if (contentType) headers['Content-Type'] = 'application/json'
  if (apiKey) headers.Authorization = `Bearer ${apiKey}`
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
  throw new Error(message)
}

export async function getStats(): Promise<PoolStats> {
  const response = await fetch('/api/stats', { cache: 'no-store', headers: apiHeaders() })
  await requireOK(response)
  return (await response.json()) as PoolStats
}

export async function getInfo(): Promise<BackendInfo> {
  const response = await fetch('/api/info', { cache: 'no-store', headers: apiHeaders() })
  await requireOK(response)
  return (await response.json()) as BackendInfo
}

export async function getHealth(): Promise<boolean> {
  try {
    const response = await fetch('/api/readyz', { cache: 'no-store', headers: apiHeaders() })
    return response.ok
  } catch {
    return false
  }
}

export async function ask(model: string, conversationId: string, messages: ChatMessage[], signal: AbortSignal): Promise<AskResult> {
	const response = await fetch('/api/v1/chat/completions', {
		method: 'POST',
		headers: apiHeaders(true),
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
	const response = await fetch('/api/v1/chat/completions', {
		method: 'POST',
		headers: apiHeaders(true),
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
