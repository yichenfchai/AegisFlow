import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, httpRequest } from './http'
import { FALLBACK_ERROR_TEXT, NETWORK_ERROR_TEXT, TIMEOUT_ERROR_TEXT } from './errors'

function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json', ...headers },
  })
}

function errorEnvelope(code: string, message = 'internal detail') {
  return { error: { code, message }, requestId: 'req-test' }
}

/** 取出被拒绝的 ApiError；不是 ApiError 或根本没有拒绝都视为测试失败。 */
async function captureApiError(promise: Promise<unknown>): Promise<ApiError> {
  let caught: unknown
  let rejected = false
  try {
    await promise
  } catch (cause) {
    rejected = true
    caught = cause
  }
  if (!rejected) throw new Error('expected the request to reject')
  if (!(caught instanceof ApiError)) throw new Error('expected an ApiError')
  return caught
}

function stubFetch(implementation: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>) {
  const mock = vi.fn(implementation)
  vi.stubGlobal('fetch', mock)
  return mock
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('httpRequest error localization', () => {
  it('replaces the backend English message with a Chinese code-based message', async () => {
    stubFetch(async () =>
      jsonResponse(500, errorEnvelope('internal_error', 'pq: connection refused at 10.0.0.8')),
    )
    const error = await captureApiError(httpRequest('/builds'))
    expect(error.message).toBe('服务器内部错误，请稍后重试')
    expect(error.message).not.toContain('pq:')
    expect(error.code).toBe('internal_error')
    expect(error.status).toBe(500)
    expect(error.requestId).toBe('req-test')
  })

  it('never renders the backend message for an unknown code', async () => {
    stubFetch(async () => jsonResponse(418, errorEnvelope('teapot', 'secret internal detail')))
    const error = await captureApiError(httpRequest('/builds'))
    expect(error.code).toBe('teapot')
    expect(error.message).toBe(FALLBACK_ERROR_TEXT)
    expect(error.message).not.toContain('secret')
  })

  it('maps contract codes such as validation_failed and account_disabled', async () => {
    stubFetch(async () => jsonResponse(400, errorEnvelope('validation_failed', 'name must be 3-64 bytes')))
    expect((await captureApiError(httpRequest('/projects'))).message).toBe('提交的内容不合法，请检查后重试')

    stubFetch(async () => jsonResponse(403, errorEnvelope('account_disabled', 'account is disabled')))
    expect((await captureApiError(httpRequest('/auth/login'))).message).toBe('账号已被禁用，请联系管理员')
  })

  it('adds the Retry-After seconds to rate_limited responses', async () => {
    stubFetch(async () =>
      jsonResponse(429, errorEnvelope('rate_limited', 'too many login attempts'), { 'Retry-After': '7' }),
    )
    const error = await captureApiError(httpRequest('/auth/login'))
    expect(error.retryAfter).toBe(7)
    expect(error.message).toBe('操作过于频繁，请 7 秒后重试')
  })

  it('falls back to the generic rate-limit message when Retry-After is not a number', async () => {
    stubFetch(async () =>
      jsonResponse(429, errorEnvelope('rate_limited'), { 'Retry-After': 'Wed, 21 Oct 2026 07:28:00 GMT' }),
    )
    const error = await captureApiError(httpRequest('/auth/login'))
    expect(error.retryAfter).toBeUndefined()
    expect(error.message).toBe('操作过于频繁，请稍后重试')
  })

  it('localizes network failures and timeouts as client ApiErrors', async () => {
    stubFetch(() => Promise.reject(new TypeError('fetch failed')))
    const network = await captureApiError(httpRequest('/builds'))
    expect(network.status).toBe(0)
    expect(network.code).toBe('network_error')
    expect(network.message).toBe(NETWORK_ERROR_TEXT)

    stubFetch(() => Promise.reject(new DOMException('The operation timed out.', 'TimeoutError')))
    const timeout = await captureApiError(httpRequest('/builds'))
    expect(timeout.status).toBe(0)
    expect(timeout.code).toBe('timeout')
    expect(timeout.message).toBe(TIMEOUT_ERROR_TEXT)
  })

  it('reports caller aborts separately from network failures', async () => {
    stubFetch(() => Promise.reject(new DOMException('aborted', 'AbortError')))
    const error = await captureApiError(httpRequest('/builds'))
    expect(error.code).toBe('request_aborted')
    expect(error.message).toBe('请求已取消')
  })

  it('rejects malformed response bodies with a Chinese invalid_response message', async () => {
    stubFetch(async () => new Response('<html>gateway error</html>', { status: 502 }))
    const error = await captureApiError(httpRequest('/builds'))
    expect(error.code).toBe('invalid_response')
    expect(error.message).toBe('服务返回了无效响应，请稍后重试')
    expect(error.message).not.toContain('gateway')
  })

  it('returns unwrapped data and sends the bearer token', async () => {
    const fetchMock = stubFetch(async () =>
      jsonResponse(200, { data: { ok: true }, requestId: 'req-test' }),
    )
    await expect(httpRequest<{ ok: boolean }>('/ping', {}, 'token-1')).resolves.toEqual({ ok: true })
    const call = fetchMock.mock.calls[0]
    expect(call?.[0]).toBe('/api/v1/ping')
    expect(new Headers(call?.[1]?.headers).get('Authorization')).toBe('Bearer token-1')
  })
})
