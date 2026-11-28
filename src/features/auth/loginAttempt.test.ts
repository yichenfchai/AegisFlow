import { describe, expect, it, vi } from 'vitest'
import { ApiError, apiErrorText } from '@/api/errors'
import {
  TENANT_REQUIRED_TEXT,
  TENANT_SLUG_INVALID_TEXT,
  TENANT_SLUG_REQUIRED_TEXT,
  isTenantRequiredError,
  isValidTenantSlug,
  normalizeTenantSlug,
  runLoginAttempt,
  tenantSlugFromSearch,
} from './loginAttempt'

/**
 * 登录页工作空间逻辑（阶段 F3）：tenant_required 后提示补填、提交时带上
 * tenantSlug、slug 格式校验与 URL 预填；不依赖 DOM 或 session store。
 */

function okLogin() {
  return vi.fn(async () => {})
}

function attempt(overrides: Partial<Parameters<typeof runLoginAttempt>[0]> = {}) {
  return { username: 'user@example.com', password: 'secret-password', remember: true, tenantSlug: '', tenantSlugRequired: false, ...overrides }
}

describe('tenant slug validation', () => {
  it('accepts the same 1-32 character slug format as the backend', () => {
    expect(isValidTenantSlug('a')).toBe(true)
    expect(isValidTenantSlug('acme')).toBe(true)
    expect(isValidTenantSlug('acme-corp-2')).toBe(true)
    expect(isValidTenantSlug('a'.repeat(32))).toBe(true)
  })

  it('rejects values the backend would refuse or that look like typos', () => {
    expect(isValidTenantSlug('')).toBe(false)
    expect(isValidTenantSlug('a'.repeat(33))).toBe(false)
    expect(isValidTenantSlug('Acme')).toBe(false)
    expect(isValidTenantSlug('acme_corp')).toBe(false)
    expect(isValidTenantSlug('acme corp')).toBe(false)
    expect(isValidTenantSlug('-acme')).toBe(false)
    expect(isValidTenantSlug('acme-')).toBe(false)
  })

  it('trims the submitted value', () => {
    expect(normalizeTenantSlug('  acme  ')).toBe('acme')
  })

  it('prefills only a valid ?tenant= slug and ignores anything else', () => {
    expect(tenantSlugFromSearch('?tenant=acme')).toBe('acme')
    expect(tenantSlugFromSearch('?tenant=%20acme%20')).toBe('acme')
    expect(tenantSlugFromSearch('?returnTo=%2Fbuilds&tenant=acme')).toBe('acme')
    expect(tenantSlugFromSearch('?tenant=Acme')).toBe('')
    expect(tenantSlugFromSearch('?tenant=')).toBe('')
    expect(tenantSlugFromSearch('')).toBe('')
  })
})

describe('login attempt', () => {
  it('submits the trimmed tenantSlug when the user fills the workspace field', async () => {
    const login = okLogin()
    await expect(runLoginAttempt(attempt({ tenantSlug: '  acme-corp ' }), login)).resolves.toEqual({ kind: 'success' })
    expect(login).toHaveBeenCalledTimes(1)
    expect(login).toHaveBeenCalledWith('user@example.com', 'secret-password', true, 'acme-corp')
  })

  it('omits tenantSlug for a single-workspace login', async () => {
    const login = okLogin()
    await expect(runLoginAttempt(attempt(), login)).resolves.toEqual({ kind: 'success' })
    expect(login).toHaveBeenCalledWith('user@example.com', 'secret-password', true, undefined)
  })

  it('trims the username but keeps the password exactly as typed', async () => {
    const login = okLogin()
    await runLoginAttempt(attempt({ username: '  e2e-admin  ', password: ' password ' }), login)
    expect(login).toHaveBeenCalledWith('e2e-admin', ' password ', true, undefined)
  })

  it('surfaces tenant_required separately so the page can open the workspace field', async () => {
    const error = new ApiError(400, 'tenant_required', apiErrorText('tenant_required'))
    const login = vi.fn(async () => {
      throw error
    })
    await expect(runLoginAttempt(attempt(), login)).resolves.toEqual({ kind: 'tenant_required', error })
    expect(isTenantRequiredError(error)).toBe(true)
    // The page maps the failure through the shared Chinese code table.
    expect(apiErrorText('tenant_required')).toBe(TENANT_REQUIRED_TEXT)
  })

  it('keeps other failures on the generic login error path', async () => {
    const error = new ApiError(401, 'unauthorized', apiErrorText('unauthorized'))
    const login = vi.fn(async () => {
      throw error
    })
    await expect(runLoginAttempt(attempt(), login)).resolves.toEqual({ kind: 'failed', error })
    expect(isTenantRequiredError(error)).toBe(false)
  })

  it('does not call the API for invalid input', async () => {
    const login = okLogin()
    await expect(runLoginAttempt(attempt({ username: '   ' }), login)).resolves.toEqual({ kind: 'invalid', message: '用户名不能为空' })
    await expect(runLoginAttempt(attempt({ password: '' }), login)).resolves.toEqual({ kind: 'invalid', message: '密码不能为空' })
    await expect(runLoginAttempt(attempt({ tenantSlug: 'Acme' }), login)).resolves.toEqual({
      kind: 'invalid',
      message: TENANT_SLUG_INVALID_TEXT,
    })
    expect(login).not.toHaveBeenCalled()
  })

  it('requires the workspace field again after tenant_required', async () => {
    const login = okLogin()
    await expect(runLoginAttempt(attempt({ tenantSlugRequired: true }), login)).resolves.toEqual({
      kind: 'invalid',
      message: TENANT_SLUG_REQUIRED_TEXT,
    })
    expect(login).not.toHaveBeenCalled()
  })
})
