/** Authenticated Server-Sent Events client for build logs.
 *
 * EventSource cannot attach an Authorization header, so the stream is read from
 * a fetch body and frames are parsed here. Callers keep the last event id and
 * reconnect when the server asks them to (lifetime cap) or when a transport
 * error happens; the polling endpoint remains the fallback. */
import { ApiError } from '@/api/http'
import { apiErrorText } from '@/api/errors'
import { accessTokenForRequest } from '@/stores/session'
import { createFrameParser } from '@/api/sseParser'
import type { Build, LogEvent } from '@/api/builds'

const baseURL = (import.meta.env.VITE_API_BASE_URL ?? '/api/v1').replace(/\/$/, '')

/**
 * The server writes a status frame as soon as the stream is established, so a
 * response that delivers no bytes for this long is a hung connection, not a
 * quiet build.
 */
export const STREAM_FIRST_FRAME_TIMEOUT_MS = 8_000
/**
 * The server sends a `: ping` comment every 15s; any byte (including that
 * heartbeat) refreshes this window, so 30s of silence means the stream died.
 */
export const STREAM_IDLE_TIMEOUT_MS = 30_000

/** Why the server ended the stream: terminal build, or ask the client to reconnect. */
export type BuildStreamEnd = 'terminal' | 'reconnect'

/** Test seam: production callers use the defaults above. */
export interface BuildStreamTimeouts {
  firstFrameMs?: number
  idleMs?: number
}

export interface BuildStreamCallbacks {
  onOpen?: () => void
  onStatus: (build: Build) => void
  onLogs: (events: LogEvent[]) => void
}

/**
 * streamBuildEvents reads one SSE response until it ends, forwarding frames.
 *
 * A first-frame or idle timeout aborts the internal fetch/reader, clears its
 * timer, and throws an ApiError with code `stream_timeout`; the caller's existing
 * single retry then falls back to cursor polling. Caller aborts (component
 * unmount) are forwarded unchanged and never counted as timeouts.
 */
export async function streamBuildEvents(
  buildID: string,
  after: number,
  signal: AbortSignal,
  callbacks: BuildStreamCallbacks,
  timeouts: BuildStreamTimeouts = {},
): Promise<BuildStreamEnd> {
  const firstFrameMs = timeouts.firstFrameMs ?? STREAM_FIRST_FRAME_TIMEOUT_MS
  const idleMs = timeouts.idleMs ?? STREAM_IDLE_TIMEOUT_MS
  const token = await accessTokenForRequest()
  const internal = new AbortController()
  let timedOut: 'first-frame' | 'idle' | null = null
  let timer: ReturnType<typeof setTimeout> | undefined
  let sawChunk = false
  const clearTimer = () => {
    if (timer !== undefined) {
      clearTimeout(timer)
      timer = undefined
    }
  }
  const armTimer = () => {
    clearTimer()
    timer = setTimeout(() => {
      timedOut = sawChunk ? 'idle' : 'first-frame'
      internal.abort()
    }, sawChunk ? idleMs : firstFrameMs)
  }
  const forwardAbort = () => internal.abort()
  if (signal.aborted) throw new DOMException('The build stream was aborted', 'AbortError')
  signal.addEventListener('abort', forwardAbort, { once: true })
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined
  try {
    armTimer()
    const response = await fetch(
      `${baseURL}/builds/${encodeURIComponent(buildID)}/events?after=${after}`,
      {
        headers: { Accept: 'text/event-stream', Authorization: `Bearer ${token}` },
        credentials: 'include',
        signal: internal.signal,
      },
    )
    if (!response.ok || !response.body) {
      throw new ApiError(response.status, 'stream_failed', '无法建立构建日志流')
    }
    callbacks.onOpen?.()

    reader = response.body.getReader()
    const decoder = new TextDecoder()
    let end: BuildStreamEnd = 'reconnect'
    let finished = false
    const parser = createFrameParser((event, data) => {
      try {
        if (event === 'status') {
          callbacks.onStatus(JSON.parse(data) as Build)
        } else if (event === 'logs') {
          callbacks.onLogs(JSON.parse(data) as LogEvent[])
        } else if (event === 'done') {
          const payload = JSON.parse(data) as { reason?: string }
          end = payload.reason === 'terminal' ? 'terminal' : 'reconnect'
          finished = true
        }
      } catch {
        // 帧内容损坏属于服务端 / 传输异常：按统一中文文案抛出，不暴露解析细节。
        throw new ApiError(0, 'invalid_response', apiErrorText('invalid_response'))
      }
    })

    while (!finished) {
      const { done, value } = await reader.read()
      if (done) break
      // Any byte proves the connection is alive, including `: ping` heartbeats
      // that the frame parser deliberately ignores.
      sawChunk = true
      armTimer()
      parser.push(decoder.decode(value, { stream: true }))
    }
    if (finished) await reader.cancel().catch(() => undefined)
    return end
  } catch (cause) {
    if (timedOut) {
      throw new ApiError(
        0,
        'stream_timeout',
        timedOut === 'first-frame'
          ? '构建日志流首帧超时，已回退轮询'
          : '构建日志流空闲超时，已回退轮询',
      )
    }
    throw cause
  } finally {
    clearTimer()
    signal.removeEventListener('abort', forwardAbort)
    if (reader) {
      try {
        reader.releaseLock()
      } catch {
        // The stream is already errored or closed; nothing left to release.
      }
    }
  }
}
