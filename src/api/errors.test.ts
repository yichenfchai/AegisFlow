import { describe, expect, it } from 'vitest'
import {
  ApiError,
  API_ERROR_TEXT,
  FALLBACK_ERROR_TEXT,
  NETWORK_ERROR_TEXT,
  TIMEOUT_ERROR_TEXT,
  apiErrorText,
  errorText,
} from './errors'

/** 鉴权契约 / backend/README.md 中列出的错误码，一个都不能退化成兜底文案。 */
const CONTRACT_CODES = [
  'unauthorized',
  'forbidden',
  'not_found',
  'validation_failed',
  'conflict',
  'rate_limited',
  'account_disabled',
  'origin_denied',
  'not_ready',
  'dependencies_unavailable',
  'internal_error',
  'unsupported_media_type',
] as const

describe('apiErrorText', () => {
  it('maps every contract error code to a non-empty Chinese message', () => {
    for (const code of CONTRACT_CODES) {
      const text = apiErrorText(code)
      expect(text, code).toMatch(/[\u4e00-\u9fff]/)
      expect(text).not.toBe(FALLBACK_ERROR_TEXT)
    }
  })

  it('also localizes the documented invalid_request code', () => {
    expect(apiErrorText('invalid_request')).toBe('请求参数不合法，请检查后重试')
  })

  it('appends the Retry-After seconds to rate_limited', () => {
    expect(apiErrorText('rate_limited', 7)).toBe('操作过于频繁，请 7 秒后重试')
  })

  it('rounds fractional seconds and clamps oversized Retry-After values', () => {
    expect(apiErrorText('rate_limited', 1.2)).toBe('操作过于频繁，请 2 秒后重试')
    expect(apiErrorText('rate_limited', 999_999)).toContain('3600 秒后重试')
  })

  it('ignores Retry-After for non rate-limit codes and invalid values', () => {
    expect(apiErrorText('internal_error', 10)).toBe('服务器内部错误，请稍后重试')
    expect(apiErrorText('rate_limited', 0)).toBe(API_ERROR_TEXT.rate_limited)
    expect(apiErrorText('rate_limited', Number.NaN)).toBe(API_ERROR_TEXT.rate_limited)
  })

  it('falls back to a generic Chinese message for unknown codes', () => {
    const text = apiErrorText('a_future_backend_code')
    expect(text).toBe(FALLBACK_ERROR_TEXT)
    expect(text).toMatch(/[\u4e00-\u9fff]/)
  })
})

describe('ApiError', () => {
  it('keeps status, code, requestId and retryAfter for programmatic branching', () => {
    const error = new ApiError(429, 'rate_limited', apiErrorText('rate_limited', 5), 'req-1', 5)
    expect(error).toBeInstanceOf(Error)
    expect(error.name).toBe('ApiError')
    expect(error.status).toBe(429)
    expect(error.code).toBe('rate_limited')
    expect(error.requestId).toBe('req-1')
    expect(error.retryAfter).toBe(5)
  })
})

describe('errorText', () => {
  it('returns the already localized ApiError message', () => {
    const error = new ApiError(403, 'origin_denied', apiErrorText('origin_denied'))
    expect(errorText(error)).toBe('当前请求来源不被允许，请从本站页面重试')
  })

  it('keeps frontend-generated Chinese messages such as the Web Locks requirement', () => {
    const error = new Error('请使用支持 Web Locks 的现代浏览器，并通过 HTTPS 或 localhost 访问')
    expect(errorText(error)).toContain('Web Locks')
  })

  it('uses the fallback for non-Error values and caller aborts', () => {
    expect(errorText('boom', '操作失败，请稍后重试')).toBe('操作失败，请稍后重试')
    expect(errorText(new DOMException('aborted', 'AbortError'), '已取消')).toBe('已取消')
  })

  it('exposes Chinese transport messages for network errors and timeouts', () => {
    expect(NETWORK_ERROR_TEXT).toMatch(/[\u4e00-\u9fff]/)
    expect(TIMEOUT_ERROR_TEXT).toMatch(/[\u4e00-\u9fff]/)
    expect(apiErrorText('network_error')).toBe(NETWORK_ERROR_TEXT)
    expect(apiErrorText('timeout')).toBe(TIMEOUT_ERROR_TEXT)
  })
})
