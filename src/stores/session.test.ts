import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

/**
 * session store 边界行为：单飞刷新、401 最多重试一次、revision 守卫、
 * 403 不触发刷新、恢复失败区分 401 与网络错误、跨标签页广播。
 * 每个用例通过 vi.resetModules 获得干净的模块级状态。
 */

interface TestUser {
  id: string
  username: string
  displayName: string
  role: 'admin' | 'member' | 'viewer'
  tenant: { id: string; slug: string; name: string }
}

const user: TestUser = {
  id: 'u1',
  username: 'admin',
  displayName: 'Admin',
  role: 'admin',
  tenant: { id: 'tenant_default_0000000000000001', slug: 'default', name: 'Default' },
}
const sessionPayload = { user, accessToken: 'at-1', expiresIn: 900 }
const expiredPayload = { user, accessToken: 'at-1', expiresIn: -1 }

type FetchHandler = (url: string, init?: RequestInit) => Response

let fetchMock: FetchHandler
let posted: Array<{ type: string }>
let refreshCalls: number

class FakeBroadcastChannel {
  postMessage(data: { type: string }) {
    posted.push(data)
  }
  addEventListener() {}
  close() {}
}

function jsonResponse(status: number, data: unknown, code = 'unauthorized') {
  const body =
    status >= 400
      ? JSON.stringify({ error: { code, message: 'api message' }, requestId: 'r' })
      : JSON.stringify({ data, requestId: 'r' })
  return new Response(body, { status, headers: { 'content-type': 'application/json' } })
}

async function freshStore() {
  vi.resetModules()
  posted = []
  refreshCalls = 0
  fetchMock = () => jsonResponse(200, {})
  vi.stubGlobal(
    'fetch',
    (async (url: string, init?: RequestInit) => fetchMock(url, init)) as unknown as typeof fetch,
  )
  vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
  // Web Locks is required by the store; a pass-through lock keeps tests single-tab.
  vi.stubGlobal('navigator', {
    locks: { request: (_name: string, action: () => Promise<unknown>) => action() },
  })
  return await import('@/stores/session')
}

describe('session store', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
    vi.resetModules()
  })

  it('login applies the session and notifies other tabs', async () => {
    const { useSession } = await freshStore()
    fetchMock = (url) => (url.includes('/auth/login') ? jsonResponse(200, sessionPayload) : jsonResponse(404, null))
    await useSession.getState().login('admin', 'pw', true)
    const state = useSession.getState()
    expect(state.status).toBe('authenticated')
    expect(state.accessToken).toBe('at-1')
    expect(state.user?.username).toBe('admin')
    // The tenant metadata returned by login is kept for the app shell.
    expect(state.user?.tenant).toEqual({ id: 'tenant_default_0000000000000001', slug: 'default', name: 'Default' })
    expect(posted).toEqual([{ type: 'changed' }])
  })

  it('login sends tenantSlug only when a workspace was resolved', async () => {
    const { useSession } = await freshStore()
    const bodies: Array<Record<string, unknown>> = []
    fetchMock = (url, init) => {
      if (url.includes('/auth/login')) {
        bodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>)
        return jsonResponse(200, sessionPayload)
      }
      return jsonResponse(404, null)
    }
    await useSession.getState().login('admin', 'pw', true)
    await useSession.getState().login('admin', 'pw', true, 'acme')
    expect(bodies[0]).toEqual({ username: 'admin', password: 'pw', remember: true })
    expect(bodies[1]).toEqual({ username: 'admin', password: 'pw', remember: true, tenantSlug: 'acme' })
  })

  it('restore saves the tenant metadata returned by /auth/me', async () => {
    const { useSession } = await freshStore()
    fetchMock = (url) => (url.includes('/auth/refresh') ? jsonResponse(200, sessionPayload) : jsonResponse(200, user))
    await useSession.getState().restore()
    expect(useSession.getState().user?.tenant.name).toBe('Default')
  })

  it('failed login stays anonymous and surfaces the API error', async () => {
    const { useSession } = await freshStore()
    fetchMock = () => jsonResponse(401, null)
    // The app always restores before the login form is usable; its 401 also
    // broadcasts logout to other tabs, which is expected here.
    await useSession.getState().restore()
    posted.length = 0
    await expect(useSession.getState().login('admin', 'bad', false)).rejects.toMatchObject({ status: 401 })
    expect(useSession.getState().status).toBe('anonymous')
    expect(posted).toEqual([])
  })

  it('restore treats a 401 refresh as anonymous without an error screen', async () => {
    const { useSession } = await freshStore()
    fetchMock = (url) => (url.includes('/auth/refresh') ? jsonResponse(401, null) : jsonResponse(200, user))
    await useSession.getState().restore()
    expect(useSession.getState().status).toBe('anonymous')
  })

  it('restore keeps an error state on network failure instead of logging out', async () => {
    const { useSession } = await freshStore()
    fetchMock = () => {
      throw new TypeError('fetch failed')
    }
    await useSession.getState().restore()
    expect(useSession.getState().status).toBe('error')
    expect(useSession.getState().error).toContain('无法连接服务')
  })

  it('shares one refresh across concurrent expired requests', async () => {
    const { useSession, apiRequest } = await freshStore()
    fetchMock = (url) => {
      if (url.includes('/auth/refresh')) {
        refreshCalls++
        return jsonResponse(200, { ...expiredPayload, accessToken: `at-${refreshCalls + 1}` })
      }
      if (url.includes('/auth/me')) return jsonResponse(200, user)
      return jsonResponse(200, { ok: true })
    }
    await useSession.getState().restore()
    const before = refreshCalls
    const results = await Promise.all(Array.from({ length: 5 }, () => apiRequest<{ ok: boolean }>('/jobs')))
    expect(results.every((r) => r.ok)).toBe(true)
    expect(refreshCalls).toBe(before + 1)
  })

  it('does not rotate the cookie on every request when the token is short-lived', async () => {
    const { useSession, apiRequest } = await freshStore()
    fetchMock = (url) => {
      if (url.includes('/auth/login')) return jsonResponse(200, { ...sessionPayload, expiresIn: 3 })
      if (url.includes('/auth/refresh')) {
        refreshCalls++
        return jsonResponse(200, { ...sessionPayload, expiresIn: 3 })
      }
      return jsonResponse(200, { ok: true })
    }
    await useSession.getState().login('admin', 'pw', true)
    const before = refreshCalls
    for (let i = 0; i < 3; i++) await apiRequest('/jobs')
    // A fixed five-second margin would have refreshed (and rotated) on every call.
    expect(refreshCalls).toBe(before)
  })

  it('retries a 401 exactly once after rotating the token', async () => {
    const { useSession, apiRequest } = await freshStore()
    let jobCalls = 0
    fetchMock = (url) => {
      if (url.includes('/auth/refresh')) {
        refreshCalls++
        return jsonResponse(200, { ...sessionPayload, accessToken: 'at-2' })
      }
      if (url.includes('/auth/me')) return jsonResponse(200, user)
      if (url.includes('/jobs')) {
        jobCalls++
        return jobCalls === 1 ? jsonResponse(401, null) : jsonResponse(200, { ok: true })
      }
      return jsonResponse(404, null)
    }
    await useSession.getState().restore()
    const before = refreshCalls
    const result = await apiRequest<{ ok: boolean }>('/jobs')
    expect(result.ok).toBe(true)
    expect(jobCalls).toBe(2)
    expect(refreshCalls).toBe(before + 1)
  })

  it('does not refresh on 403 responses', async () => {
    const { useSession, apiRequest } = await freshStore()
    fetchMock = (url) => {
      if (url.includes('/auth/refresh')) {
        refreshCalls++
        return jsonResponse(200, sessionPayload)
      }
      if (url.includes('/auth/me')) return jsonResponse(200, user)
      return jsonResponse(403, null, 'forbidden')
    }
    await useSession.getState().restore()
    const before = refreshCalls
    await expect(apiRequest('/jobs')).rejects.toMatchObject({ status: 403, code: 'forbidden' })
    expect(refreshCalls).toBe(before)
  })

  it('a failed refresh after login clears the session and broadcasts logout', async () => {
    const { useSession, refreshSession } = await freshStore()
    fetchMock = (url) => (url.includes('/auth/login') ? jsonResponse(200, sessionPayload) : jsonResponse(401, null))
    await useSession.getState().login('admin', 'pw', true)
    posted.length = 0
    await expect(refreshSession()).rejects.toMatchObject({ status: 401 })
    expect(useSession.getState().status).toBe('anonymous')
    expect(posted).toEqual([{ type: 'logout' }])
  })

  it('logout revokes server-side first and clears state only on success', async () => {
    const { useSession } = await freshStore()
    let logoutFails = true
    fetchMock = (url) => {
      if (url.includes('/auth/login')) return jsonResponse(200, sessionPayload)
      if (url.includes('/auth/logout')) {
        if (logoutFails) {
          logoutFails = false
          throw new TypeError('fetch failed')
        }
        return jsonResponse(200, { loggedOut: true })
      }
      return jsonResponse(404, null)
    }
    await useSession.getState().login('admin', 'pw', false)
    await expect(useSession.getState().logout()).rejects.toMatchObject({ status: 0 })
    expect(useSession.getState().status).toBe('authenticated')
    expect(posted).toEqual([{ type: 'changed' }])
    await useSession.getState().logout()
    expect(useSession.getState().status).toBe('anonymous')
    // Logout clears the user object, so the tenant metadata disappears with it.
    expect(useSession.getState().user).toBeNull()
    expect(posted).toEqual([{ type: 'changed' }, { type: 'logout' }])
  })

  it('apiRequest fails fast before any request when logged out', async () => {
    const { useSession, apiRequest } = await freshStore()
    fetchMock = (url) => (url.includes('/auth/refresh') ? jsonResponse(401, null) : jsonResponse(200, user))
    await useSession.getState().restore()
    let called = false
    fetchMock = () => {
      called = true
      return jsonResponse(200, {})
    }
    await expect(apiRequest('/jobs')).rejects.toMatchObject({ status: 401, code: 'unauthorized' })
    expect(called).toBe(false)
  })

  it('login surfaces the Chinese account_disabled message instead of the backend message', async () => {
    const { useSession, errorMessage } = await freshStore()
    fetchMock = () => jsonResponse(403, null, 'account_disabled')
    const error = await useSession.getState().login('admin', 'pw', false).catch((cause: unknown) => cause)
    expect(errorMessage(error)).toBe('账号已被禁用，请联系管理员')
    expect(errorMessage(error)).not.toContain('api message')
  })

  it('login surfaces the workspace prompt for a tenant_required failure', async () => {
    const { useSession, errorMessage } = await freshStore()
    fetchMock = () => jsonResponse(400, null, 'tenant_required')
    const error = await useSession.getState().login('admin', 'pw', false).catch((cause: unknown) => cause)
    expect(errorMessage(error)).toBe('该邮箱存在于多个工作空间，请输入工作空间标识')
    // No half-authenticated state: the failed attempt keeps the user empty.
    expect(useSession.getState().user).toBeNull()
    expect(useSession.getState().accessToken).toBeNull()
  })

  it('restore localizes a backend error instead of showing its English message', async () => {
    const { useSession } = await freshStore()
    fetchMock = () => jsonResponse(500, null, 'internal_error')
    await useSession.getState().restore()
    expect(useSession.getState().status).toBe('error')
    expect(useSession.getState().error).toBe('服务器内部错误，请稍后重试')
    expect(useSession.getState().error).not.toContain('api message')
  })

  it('loginLDAP stores the session and tenant returned by the directory endpoint', async () => {
    const { useSession } = await freshStore()
    const bodies: Array<Record<string, unknown>> = []
    fetchMock = (url, init) => {
      if (url.includes('/auth/ldap/login')) {
        bodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>)
        return jsonResponse(200, sessionPayload)
      }
      return jsonResponse(404, null)
    }
    await useSession.getState().loginLDAP('alice', 'directory-password')
    await useSession.getState().loginLDAP('alice', 'directory-password', 'acme')
    const state = useSession.getState()
    expect(state.status).toBe('authenticated')
    expect(state.accessToken).toBe('at-1')
    expect(state.user?.tenant).toEqual({ id: 'tenant_default_0000000000000001', slug: 'default', name: 'Default' })
    expect(posted).toEqual([{ type: 'changed' }, { type: 'changed' }])
    // No remember field: the directory form has no persistent-session choice.
    expect(bodies[0]).toEqual({ username: 'alice', password: 'directory-password' })
    expect(bodies[1]).toEqual({ username: 'alice', password: 'directory-password', tenantSlug: 'acme' })
  })

  it('loginLDAP surfaces the localized directory error codes', async () => {
    const { useSession, errorMessage } = await freshStore()
    fetchMock = () => jsonResponse(401, null)
    const unauthorized = await useSession.getState().loginLDAP('alice', 'bad').catch((cause: unknown) => cause)
    expect(errorMessage(unauthorized)).toContain('账号、密码错误')
    expect(useSession.getState().user).toBeNull()

    fetchMock = () => jsonResponse(403, null, 'account_not_found')
    const unlinked = await useSession.getState().loginLDAP('alice', 'pw').catch((cause: unknown) => cause)
    expect(errorMessage(unlinked)).toBe('账号未开通，请联系管理员')
    expect(useSession.getState().accessToken).toBeNull()
  })
})
