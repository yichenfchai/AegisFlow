import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '@/api/errors'
import {
  LDAP_INVALID_CREDENTIALS_TEXT,
  LDAP_UNAVAILABLE_TEXT,
  ldapButtonLabel,
  ldapEnabled,
  ldapLoginErrorMessage,
  loadLDAPConfig,
  runLDAPLoginAttempt,
} from './ldap'
import { TENANT_REQUIRED_TEXT, TENANT_SLUG_INVALID_TEXT, TENANT_SLUG_REQUIRED_TEXT } from './loginAttempt'

/**
 * LDAP 登录页逻辑（阶段 G）：探针归一、入口显隐、提交参数（trim / 省略空工作空间
 * 标识）、错误码中文映射与 tenant_required 流程；不依赖 DOM 或真实网络。
 */

function jsonResponse(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } })
}

function stubFetch(implementation: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>) {
  const mock = vi.fn(implementation)
  vi.stubGlobal('fetch', mock)
  return mock
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('LDAP config probe', () => {
  it('loads the enabled configuration and reads the label', async () => {
    stubFetch(async () => jsonResponse(200, { data: { enabled: true, label: '企业目录' }, requestId: 'r' }))
    const config = await loadLDAPConfig()
    expect(ldapEnabled(config)).toBe(true)
    expect(ldapButtonLabel(config)).toBe('企业目录')
  })

  it('hides the entry when the feature is disabled, empty-labelled or malformed', async () => {
    stubFetch(async () => jsonResponse(200, { data: { enabled: false, label: '' }, requestId: 'r' }))
    expect(ldapEnabled(await loadLDAPConfig())).toBe(false)
    expect(ldapButtonLabel({ enabled: false, label: '' })).toBeNull()
    expect(ldapButtonLabel({ enabled: true, label: '   ' })).toBe('LDAP 登录')
    expect(ldapButtonLabel(null)).toBeNull()

    stubFetch(async () => jsonResponse(200, { data: { enabled: 'yes', label: '' }, requestId: 'r' }))
    expect(ldapEnabled(await loadLDAPConfig())).toBe(false)
  })

  it('normalizes probe failures to disabled instead of breaking password login', async () => {
    stubFetch(async () => jsonResponse(503, { error: { code: 'dependencies_unavailable', message: 'x' }, requestId: 'r' }))
    expect(await loadLDAPConfig()).toEqual({ enabled: false, label: '' })
    stubFetch(async () => {
      throw new TypeError('fetch failed')
    })
    expect(await loadLDAPConfig()).toEqual({ enabled: false, label: '' })
  })
})

describe('LDAP error mapping', () => {
  it('maps directory credential failures to one generic Chinese message', () => {
    expect(ldapLoginErrorMessage(new ApiError(401, 'unauthorized', 'invalid credentials or expired session'))).toBe(
      LDAP_INVALID_CREDENTIALS_TEXT,
    )
  })

  it('maps directory unavailability to the dedicated retry message', () => {
    expect(ldapLoginErrorMessage(new ApiError(503, 'dependencies_unavailable', 'dependencies unavailable'))).toBe(
      LDAP_UNAVAILABLE_TEXT,
    )
  })

  it('maps account and tenant codes from the shared error table', () => {
    expect(ldapLoginErrorMessage(new ApiError(403, 'account_not_found', 'account is not provisioned; contact an administrator'))).toBe(
      '账号未开通，请联系管理员',
    )
    expect(ldapLoginErrorMessage(new ApiError(403, 'account_disabled', 'account is disabled; contact an administrator'))).toBe(
      '账号已被禁用，请联系管理员',
    )
    expect(ldapLoginErrorMessage(new ApiError(400, 'tenant_required', 'tenantSlug is required'))).toBe(TENANT_REQUIRED_TEXT)
    expect(ldapLoginErrorMessage(new ApiError(400, 'validation_failed', 'bad request'))).toBe('提交的内容不合法，请检查后重试')
  })

  it('never renders a raw backend message for unknown failures', () => {
    expect(ldapLoginErrorMessage(new ApiError(500, 'unknown_code', 'pq: connection refused at 10.0.0.8'))).toBe(
      '请求失败，请稍后重试',
    )
    expect(ldapLoginErrorMessage(new Error('') )).toBe('请求失败，请稍后重试')
  })
})

function loginFn() {
  return vi.fn(async () => {})
}

function attempt(overrides: Partial<Parameters<typeof runLDAPLoginAttempt>[0]> = {}) {
  return { username: 'alice', password: 'directory-password', tenantSlug: '', tenantSlugRequired: false, ...overrides }
}

describe('LDAP login attempt', () => {
  it('submits trimmed credentials and omits an empty workspace slug', async () => {
    const login = loginFn()
    const outcome = await runLDAPLoginAttempt(attempt({ username: '  alice  ', tenantSlug: '   ' }), login)
    expect(outcome).toEqual({ kind: 'success' })
    expect(login).toHaveBeenCalledWith('alice', 'directory-password', undefined)
  })

  it('submits the trimmed workspace slug when provided', async () => {
    const login = loginFn()
    const outcome = await runLDAPLoginAttempt(attempt({ tenantSlug: ' acme ' }), login)
    expect(outcome.kind).toBe('success')
    expect(login).toHaveBeenCalledWith('alice', 'directory-password', 'acme')
  })

  it('blocks empty credentials and malformed slugs before any request', async () => {
    const login = loginFn()
    expect(await runLDAPLoginAttempt(attempt({ username: '   ' }), login)).toMatchObject({ kind: 'invalid' })
    expect(await runLDAPLoginAttempt(attempt({ password: '' }), login)).toMatchObject({ kind: 'invalid' })
    expect(await runLDAPLoginAttempt(attempt({ tenantSlug: 'Acme' }), login)).toEqual({
      kind: 'invalid',
      message: TENANT_SLUG_INVALID_TEXT,
    })
    expect(login).not.toHaveBeenCalled()
  })

  it('requires the workspace slug after a tenant_required failure', async () => {
    const login = loginFn()
    expect(await runLDAPLoginAttempt(attempt({ tenantSlugRequired: true }), login)).toEqual({
      kind: 'invalid',
      message: TENANT_SLUG_REQUIRED_TEXT,
    })
    expect(login).not.toHaveBeenCalled()
  })

  it('returns a dedicated tenant_required outcome so the page can expand the field', async () => {
    const login = vi.fn(async () => {
      throw new ApiError(400, 'tenant_required', 'tenantSlug is required')
    })
    const outcome = await runLDAPLoginAttempt(attempt(), login)
    expect(outcome.kind).toBe('tenant_required')
  })

  it('keeps other failures for the localized mapping', async () => {
    const login = vi.fn(async () => {
      throw new ApiError(503, 'dependencies_unavailable', 'dependencies unavailable')
    })
    const outcome = await runLDAPLoginAttempt(attempt(), login)
    expect(outcome.kind).toBe('failed')
    if (outcome.kind === 'failed') expect(ldapLoginErrorMessage(outcome.error)).toBe(LDAP_UNAVAILABLE_TEXT)
  })
})
