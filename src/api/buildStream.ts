/** Authenticated Server-Sent Events client for build logs (P3).
 *
 * EventSource cannot attach an Authorization header, so the stream is read from
 * a fetch body and frames are parsed here. Callers keep the last event id and
 * reconnect when the server asks them to (lifetime cap) or when a transport
 * error happens; the polling endpoint remains the fallback. */
import { ApiError } from '@/api/http'
import { accessTokenForRequest } from '@/stores/session'
import { createFrameParser } from '@/api/sseParser'
import type { Build, LogEvent } from '@/api/builds'

const baseURL = (import.meta.env.VITE_API_BASE_URL ?? '/api/v1').replace(/\/$/, '')

/** Why the server ended the stream: terminal build, or ask the client to reconnect. */
export type BuildStreamEnd = 'terminal' | 'reconnect'

export interface BuildStreamCallbacks {
  onOpen?: () => void
  onStatus: (build: Build) => void
  onLogs: (events: LogEvent[]) => void
}

/** streamBuildEvents reads one SSE response until it ends, forwarding frames. */
export async function streamBuildEvents(
  buildID: string,
  after: number,
  signal: AbortSignal,
  callbacks: BuildStreamCallbacks,
): Promise<BuildStreamEnd> {
  const token = await accessTokenForRequest()
  const response = await fetch(
    `${baseURL}/builds/${encodeURIComponent(buildID)}/events?after=${after}`,
    {
      headers: { Accept: 'text/event-stream', Authorization: `Bearer ${token}` },
      credentials: 'include',
      signal,
    },
  )
  if (!response.ok || !response.body) {
    throw new ApiError(response.status, 'stream_failed', '无法建立构建日志流')
  }
  callbacks.onOpen?.()

  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let end: BuildStreamEnd = 'reconnect'
  let finished = false
  const parser = createFrameParser((event, data) => {
    if (event === 'status') {
      callbacks.onStatus(JSON.parse(data) as Build)
    } else if (event === 'logs') {
      callbacks.onLogs(JSON.parse(data) as LogEvent[])
    } else if (event === 'done') {
      const payload = JSON.parse(data) as { reason?: string }
      end = payload.reason === 'terminal' ? 'terminal' : 'reconnect'
      finished = true
    }
  })

  while (!finished) {
    const { done, value } = await reader.read()
    if (done) break
    parser.push(decoder.decode(value, { stream: true }))
  }
  if (finished) await reader.cancel().catch(() => undefined)
  return end
}
