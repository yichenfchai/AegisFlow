import { afterEach, describe, expect, it, vi } from 'vitest'
import { streamBuildEvents } from './buildStream'

vi.mock('@/stores/session', () => ({
  accessTokenForRequest: vi.fn(async () => 'test-token'),
}))

const encoder = new TextEncoder()

/** Build a fetch response whose SSE body is driven by the test. */
function stubFetch(streamBody: (controller: ReadableStreamDefaultController<Uint8Array>) => void) {
  const fetchMock = vi.fn((_url: string, init: RequestInit) => {
    let body!: ReadableStream<Uint8Array>
    body = new ReadableStream<Uint8Array>({
      start(controller) {
        init.signal?.addEventListener('abort', () => {
          try {
            controller.error(new DOMException('The operation was aborted.', 'AbortError'))
          } catch {
            // Stream already closed by the test.
          }
        })
        streamBody(controller)
      },
    })
    return Promise.resolve(new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }))
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const statusFrame = 'event: status\ndata: {"id":"b1","status":"running"}\n\n'
const doneFrame = 'event: done\ndata: {"reason":"terminal"}\n\n'
const callbacks = () => ({ onStatus: vi.fn(), onLogs: vi.fn(), onOpen: vi.fn() })

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('streamBuildEvents timeouts', () => {
  it('aborts a hung connection with a first-frame timeout', async () => {
    vi.useFakeTimers()
    // The request never receives response headers; aborting must reject it.
    const fetchMock = vi.fn(
      (_url: string, init: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          init.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
        }),
    )
    vi.stubGlobal('fetch', fetchMock)
    const controller = new AbortController()
    const pending = streamBuildEvents('b1', 0, controller.signal, callbacks(), {
      firstFrameMs: 20,
      idleMs: 1000,
    })
    let settled = false
    pending.catch(() => {
      settled = true
    })
    await vi.advanceTimersByTimeAsync(19)
    expect(settled).toBe(false)
    await vi.advanceTimersByTimeAsync(2)
    await expect(pending).rejects.toMatchObject({ code: 'stream_timeout', message: expect.stringContaining('首帧') })
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('aborts an idle stream after the first chunk and clears the timer', async () => {
    vi.useFakeTimers()
    stubFetch((controller) => {
      controller.enqueue(encoder.encode(statusFrame))
      // Keep the stream open with no heartbeat: only the idle timer can end it.
    })
    const pending = streamBuildEvents('b1', 0, new AbortController().signal, callbacks(), {
      firstFrameMs: 20,
      idleMs: 40,
    })
    await vi.advanceTimersByTimeAsync(0)
    await vi.advanceTimersByTimeAsync(39)
    let settled = false
    pending.catch(() => {
      settled = true
    })
    await vi.advanceTimersByTimeAsync(0)
    expect(settled).toBe(false)
    await vi.advanceTimersByTimeAsync(2)
    await expect(pending).rejects.toMatchObject({ code: 'stream_timeout', message: expect.stringContaining('空闲') })
  })

  it('treats a heartbeat comment as liveness and still reaches the done frame', async () => {
    vi.useFakeTimers()
    stubFetch((controller) => {
      controller.enqueue(encoder.encode(statusFrame))
      // A `: ping` comment arrives before the idle deadline and must refresh it.
      setTimeout(() => controller.enqueue(encoder.encode(': ping\n\n')), 20)
      setTimeout(() => {
        controller.enqueue(encoder.encode(doneFrame))
        controller.close()
      }, 40)
    })
    const pending = streamBuildEvents('b1', 0, new AbortController().signal, callbacks(), {
      firstFrameMs: 1000,
      idleMs: 30,
    })
    await vi.advanceTimersByTimeAsync(20)
    await vi.advanceTimersByTimeAsync(25)
    await expect(pending).resolves.toBe('terminal')
    expect(vi.getTimerCount()).toBe(0)
  })

  it('forwards caller aborts without reporting a timeout', async () => {
    vi.useFakeTimers()
    stubFetch((controller) => {
      controller.enqueue(encoder.encode(statusFrame))
    })
    const controller = new AbortController()
    const pending = streamBuildEvents('b1', 0, controller.signal, callbacks(), {
      firstFrameMs: 1000,
      idleMs: 1000,
    })
    await vi.advanceTimersByTimeAsync(0)
    controller.abort()
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' })
    expect(vi.getTimerCount()).toBe(0)
  })

  it('clears its timers after the server ends the stream', async () => {
    vi.useFakeTimers()
    stubFetch((controller) => {
      controller.enqueue(encoder.encode(statusFrame + doneFrame))
      controller.close()
    })
    const pending = streamBuildEvents('b1', 0, new AbortController().signal, callbacks(), {
      firstFrameMs: 1000,
      idleMs: 1000,
    })
    await expect(pending).resolves.toBe('terminal')
    expect(vi.getTimerCount()).toBe(0)
  })

  it('localizes a malformed frame instead of leaking the parser error', async () => {
    vi.useFakeTimers()
    stubFetch((controller) => {
      controller.enqueue(encoder.encode('event: status\ndata: not-json\n\n'))
    })
    const pending = streamBuildEvents('b1', 0, new AbortController().signal, callbacks(), {
      firstFrameMs: 1000,
      idleMs: 1000,
    })
    await expect(pending).rejects.toMatchObject({
      code: 'invalid_response',
      message: '服务返回了无效响应，请稍后重试',
    })
    expect(vi.getTimerCount()).toBe(0)
  })
})
