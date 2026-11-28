import { create } from 'zustand'
import { ApiError, authAPI, httpBlobRequest, httpRequest, ldapAPI } from '@/api/http'
import type { AuthSession, BlobPayload, SessionUser } from '@/api/http'
import { errorText } from '@/api/errors'

export type { SessionUser }
interface SessionState {
  user: SessionUser | null
  accessToken: string | null
  status: 'loading' | 'authenticated' | 'anonymous' | 'error'
  error: string | null
  login: (username: string, password: string, remember: boolean, tenantSlug?: string) => Promise<void>
  // LDAP 目录登录：服务端校验目录口令并签发与密码登录相同的会话；没有
  // remember 选项（目录表单不提供保持登录），因此使用非持久会话 Cookie。
  loginLDAP: (username: string, password: string, tenantSlug?: string) => Promise<void>
  restore: () => Promise<void>
  logout: () => Promise<void>
}
let refreshFlight: Promise<void> | null = null
let restoreFlight: Promise<void> | null = null
let revision = 0
let initialized = false
let expiresAt = 0
let accessLifetimeMs = 0
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
  login: async (username, password, remember, tenantSlug) => {
    const expected = ++revision
    await withSessionLock(async () => {
      const session = await authAPI.login(username, password, remember, tenantSlug)
      completeLogin(expected, session)
    })
  },
  loginLDAP: async (username, password, tenantSlug) => {
    const expected = ++revision
    await withSessionLock(async () => {
      const session = await ldapAPI.login(username, password, tenantSlug)
      completeLogin(expected, session)
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
  accessLifetimeMs = session.expiresIn * 1000
  expiresAt = Date.now() + accessLifetimeMs
  scheduleRefresh(Math.max(1000, session.expiresIn * 1000 - Math.min(30_000, session.expiresIn * 200)))
  useSession.setState({ user: session.user, accessToken: session.accessToken, status: 'authenticated', error: null })
}
/**
 * 密码登录与 LDAP 登录共用的收尾逻辑：只有本次尝试仍是最新 revision 时才应用
 * 会话并广播账号变化，避免并发登录互相覆盖。
 */
function completeLogin(expected: number, session: AuthSession) {
  if (expected !== revision) return
  initialized = true
  apply(session)
  channel?.postMessage({ type: 'changed' })
}
function clearSession() {
  clearTimeout(refreshTimer)
  expiresAt = 0
  accessLifetimeMs = 0
  useSession.setState({ user: null, accessToken: null, status: 'anonymous', error: null })
}
export function errorMessage(error: unknown): string {
  // 登录表单里 401 既可能是密码错误也可能是会话过期，给出更贴近登录场景的提示；
  // 其余错误（含 account_disabled、网络错误等）都走统一的错误码 → 中文映射。
  if (error instanceof ApiError && error.status === 401) return '账号、密码错误或会话已过期，请重新登录'
  return errorText(error, '请求失败，请稍后重试')
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
// Returns a token that is valid for the next request, restoring the session or
// refreshing first when needed. Streaming callers need the raw token because
// EventSource cannot attach an Authorization header.
export async function accessTokenForRequest(): Promise<string> {
  if (useSession.getState().status === 'loading') await useSession.getState().restore()
  if (useSession.getState().status !== 'authenticated') throw new ApiError(401, 'unauthorized', '请先登录')
  // A fixed five-second margin refreshes on every request whenever the server
  // issues short-lived tokens, and each refresh rotates the cookie; an aborted
  // rotation (for example a page reload mid-flight) then looks like a replay.
  // Scale the margin to the token lifetime instead.
  const refreshMargin = Math.min(5000, Math.max(1000, Math.floor(accessLifetimeMs / 2)))
  if (Date.now() >= expiresAt - refreshMargin) await refreshSession()
  return useSession.getState().accessToken!
}
// One refresh and at most one retry per request. A late 401 reuses a token already refreshed by a peer request.
// The send callback lets JSON and Blob calls share the same retry/session-expiry semantics.
async function withAuthRetry<T>(send: (token: string) => Promise<T>): Promise<T> {
  const token = await accessTokenForRequest()
  const expected = revision
  try { return await send(token) } catch (error) {
    if (!isUnauthorized(error)) throw error
    if (expected !== revision) throw error
    if (useSession.getState().accessToken === token) await refreshSession()
    try { return await send(useSession.getState().accessToken!) } catch (retryError) {
      if (expected === revision && isUnauthorized(retryError)) { revision++; clearSession(); channel?.postMessage({ type: 'logout' }) }
      throw retryError
    }
  }
}
export function apiRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  return withAuthRetry((token) => httpRequest<T>(path, init, token))
}
/**
 * 下载型请求（审计 CSV 导出）：成功时返回 Blob 与响应头，失败时仍归一为
 * ApiError，并复用同一刷新/过期语义。
 */
export function apiBlobRequest(path: string, init: RequestInit = {}): Promise<BlobPayload> {
  return withAuthRetry((token) => httpBlobRequest(path, init, token))
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
