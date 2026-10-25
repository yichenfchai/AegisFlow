import { create } from 'zustand'
import { ApiError, authAPI, httpRequest } from '@/api/http'
import type { AuthSession, SessionUser } from '@/api/http'

export type { SessionUser }
interface SessionState {
  user: SessionUser | null
  accessToken: string | null
  status: 'loading' | 'authenticated' | 'anonymous' | 'error'
  error: string | null
  login: (username: string, password: string, remember: boolean) => Promise<void>
  restore: () => Promise<void>
  logout: () => Promise<void>
}
let refreshFlight: Promise<void> | null = null
let restoreFlight: Promise<void> | null = null
let revision = 0
let initialized = false
let expiresAt = 0
let refreshTimer: ReturnType<typeof setTimeout> | undefined
function scheduleRefresh(delay: number) {
  clearTimeout(refreshTimer)
  refreshTimer = setTimeout(() => {
    if (useSession.getState().status !== 'authenticated') return
    void refreshSession().catch(error => {
      if (!isUnauthorized(error) && useSession.getState().status === 'authenticated') scheduleRefresh(30_000)
    })
  }, delay)
}
const channel = typeof BroadcastChannel !== 'undefined' ? new BroadcastChannel('aegisflow-session') : null

// The old demo session must never be treated as an authenticated server session.
try { localStorage.removeItem('gp_session') } catch { /* Storage may be disabled. */ }

export const useSession = create<SessionState>(() => ({
  user: null, accessToken: null, status: 'loading', error: null,
  login: async (username, password, remember) => {
    const expected = ++revision
    await withSessionLock(async () => {
      const session = await authAPI.login(username, password, remember)
      if (expected !== revision) return
      initialized = true
      apply(session)
      channel?.postMessage({ type: 'changed' })
    })
  },
  restore: () => {
    if (restoreFlight) return restoreFlight
    if (initialized) return Promise.resolve()
    useSession.setState({ status: 'loading', error: null })
    const restoringRevision = revision
    restoreFlight = (async () => {
      try {
        await refreshSession()
        const expected = revision
        const token = useSession.getState().accessToken!
        const user = await authAPI.me(token)
        if (expected === revision) useSession.setState({ user })
        initialized = true
      } catch (error) {
        if (restoringRevision !== revision && useSession.getState().status === 'authenticated') return
        if (isUnauthorized(error)) { clearSession(); initialized = true }
        else if (restoringRevision === revision) useSession.setState({ status: 'error', error: errorMessage(error) })
      }
    })().finally(() => { restoreFlight = null })
    return restoreFlight
  },
  logout: async () => {
    // Wait for ongoing rotation, then revoke the latest cookie. Keep state on network failure.
    await withSessionLock(async () => {
      await authAPI.logout()
      revision++
      initialized = true
      clearSession()
      channel?.postMessage({ type: 'logout' })
    })
  },
}))
function apply(session: AuthSession) {
  expiresAt = Date.now() + session.expiresIn * 1000
  scheduleRefresh(Math.max(1000, session.expiresIn * 1000 - Math.min(30_000, session.expiresIn * 200)))
  useSession.setState({ user: session.user, accessToken: session.accessToken, status: 'authenticated', error: null })
}
function clearSession() {
  clearTimeout(refreshTimer)
  expiresAt = 0
  useSession.setState({ user: null, accessToken: null, status: 'anonymous', error: null })
}
export function errorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === 'account_disabled') return '账号已被禁用，请联系管理员'
  if (error instanceof ApiError && error.status === 401) return '账号、密码错误或会话已过期，请重新登录'
  return error instanceof Error ? error.message : '请求失败，请稍后重试'
}
function isUnauthorized(error: unknown) { return error instanceof ApiError && error.status === 401 }
async function withSessionLock<T>(action: () => Promise<T>): Promise<T> {
  // Without Web Locks, concurrent tabs could rotate the same cookie and trigger replay detection.
  // Fail explicitly rather than silently weakening session coordination.
  if (!navigator.locks) throw new Error('请使用支持 Web Locks 的现代浏览器，并通过 HTTPS 或 localhost 访问')
  return navigator.locks.request('aegisflow-auth-cookie', action)
}
export function refreshSession(): Promise<void> {
  if (refreshFlight) return refreshFlight
  const expected = revision
  refreshFlight = withSessionLock(async () => {
    if (expected !== revision) throw new ApiError(401, 'session_changed', '会话已变更，请重新登录')
    try {
      const session = await authAPI.refresh()
      if (expected !== revision) throw new ApiError(401, 'session_changed', '会话已变更，请重新登录')
      apply(session)
    } catch (error) {
      if (expected === revision && isUnauthorized(error)) {
        revision++
        clearSession()
        channel?.postMessage({ type: 'logout' })
      }
      throw error
    }
  }).finally(() => { refreshFlight = null })
  return refreshFlight
}
// One refresh and at most one retry per request. A late 401 reuses a token already refreshed by a peer request.
export async function apiRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  if (useSession.getState().status === 'loading') await useSession.getState().restore()
  if (useSession.getState().status !== 'authenticated') throw new ApiError(401, 'unauthorized', '请先登录')
  if (Date.now() >= expiresAt - 5000) await refreshSession()
  const token = useSession.getState().accessToken!
  const expected = revision
  try { return await httpRequest<T>(path, init, token) } catch (error) {
    if (!isUnauthorized(error)) throw error
    if (expected !== revision) throw error
    if (useSession.getState().accessToken === token) await refreshSession()
    try { return await httpRequest<T>(path, init, useSession.getState().accessToken!) } catch (retryError) {
      if (expected === revision && isUnauthorized(retryError)) { revision++; clearSession(); channel?.postMessage({ type: 'logout' }) }
      throw retryError
    }
  }
}
channel?.addEventListener('message', (event: MessageEvent) => {
  if (event.data?.type !== 'logout' && event.data?.type !== 'changed') return
  revision++
  clearSession()
  if (event.data.type === 'changed') {
    initialized = false
    // Allow an old refresh flight to settle before restoring the new account.
    void (refreshFlight ?? Promise.resolve()).catch(() => {}).then(() => useSession.getState().restore())
  }
})
