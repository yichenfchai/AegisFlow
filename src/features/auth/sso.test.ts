import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  SSO_UNAVAILABLE_TEXT,
  loadOIDCConfig,
  ssoButtonLabel,
  ssoEnabled,
  ssoErrorMessage,
  ssoStartURL,
} from './sso'

/**
 * OIDC 登录页逻辑：探针加载（启用/未启用/失败降级）、按钮显示与隐藏、
 * start 链接的站内 redirectTo、以及 ?sso_error= 的中文映射。
 */

function jsonResponse(status: number, data: unknown, code = 'not_found') {
  const body =
    status >= 400
      ? JSON.stringify({ error: { code, message: 'api message' }, requestId: 'r' })
      : JSON.stringify({ data, requestId: 'r' })
  return new Response(body, { status, headers: { 'content-type': 'application/json' } })
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('OIDC config loading', () => {
  it('loads the enabled probe from the real endpoint', async () => {
    let requestedURL = ''
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      requestedURL = String(input)
      return jsonResponse(200, { enabled: true, label: '公司单点登录' })
    })
    vi.stubGlobal('fetch', fetchMock)
    await expect(loadOIDCConfig()).resolves.toEqual({ enabled: true, label: '公司单点登录' })
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(requestedURL).toBe('/api/v1/auth/oidc/config')
  })

  it('keeps the feature disabled when the backend reports it', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse(200, { enabled: false, label: '' })))
    await expect(loadOIDCConfig()).resolves.toEqual({ enabled: false, label: '' })
  })

  it('degrades an API failure to disabled instead of surfacing an error', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse(503, null, 'dependencies_unavailable')))
    await expect(loadOIDCConfig()).resolves.toEqual({ enabled: false, label: '' })
  })

  it('degrades a network failure to disabled', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new TypeError('fetch failed')
      }),
    )
    await expect(loadOIDCConfig()).resolves.toEqual({ enabled: false, label: '' })
  })

  it('degrades a malformed payload to disabled', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse(200, { label: 42 })))
    await expect(loadOIDCConfig()).resolves.toEqual({ enabled: false, label: '' })
  })
})

describe('SSO button visibility', () => {
  it('renders nothing before the probe resolves or when disabled', () => {
    expect(ssoButtonLabel(null)).toBeNull()
    expect(ssoButtonLabel({ enabled: false, label: '企业单点登录' })).toBeNull()
    expect(ssoEnabled(null)).toBe(false)
    expect(ssoEnabled({ enabled: false, label: '企业单点登录' })).toBe(false)
  })

  it('shows the configured label when enabled', () => {
    expect(ssoButtonLabel({ enabled: true, label: '公司单点登录' })).toBe('公司单点登录')
    expect(ssoEnabled({ enabled: true, label: '公司单点登录' })).toBe(true)
    // An empty label still yields a usable default rather than a blank button.
    expect(ssoButtonLabel({ enabled: true, label: '   ' })).toBe('企业单点登录')
  })
})

describe('SSO start URL', () => {
  it('keeps only same-origin relative return paths', () => {
    vi.stubGlobal('window', { location: { origin: 'http://localhost:5173' } })
    const prefix = '/api/v1/auth/oidc/start?redirectTo='
    expect(ssoStartURL('/builds?tab=logs')).toBe(prefix + encodeURIComponent('/builds?tab=logs'))
    expect(ssoStartURL(null)).toBe(prefix + '%2F')
    expect(ssoStartURL('https://evil.example/steal')).toBe(prefix + '%2F')
    expect(ssoStartURL('//evil.example/steal')).toBe(prefix + '%2F')
    expect(ssoStartURL('/\\evil.example')).toBe(prefix + '%2F')
  })
})

describe('sso_error mapping', () => {
  it('maps every callback failure code to Chinese text', () => {
    expect(ssoErrorMessage('state_expired')).toBe('单点登录状态已过期，请重试')
    expect(ssoErrorMessage('account_not_found')).toBe('账号未开通，请联系管理员')
    expect(ssoErrorMessage('account_disabled')).toBe('账号已被禁用，请联系管理员')
    expect(ssoErrorMessage('email_unverified')).toBe('单点登录账号邮箱未验证，请联系管理员')
    expect(ssoErrorMessage('token_exchange_failed')).toBe(SSO_UNAVAILABLE_TEXT)
    expect(ssoErrorMessage('invalid_token')).toBe('单点登录返回的身份信息无效，请联系管理员')
    expect(ssoErrorMessage('server_error')).toBe(SSO_UNAVAILABLE_TEXT)
  })

  it('never echoes an unknown code and tolerates a missing one', () => {
    expect(ssoErrorMessage('upstream secret detail')).toBe(SSO_UNAVAILABLE_TEXT)
    expect(ssoErrorMessage(null)).toBeNull()
    expect(ssoErrorMessage(undefined)).toBeNull()
    expect(ssoErrorMessage('')).toBeNull()
  })
})
