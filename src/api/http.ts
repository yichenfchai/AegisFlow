import {
  ApiError,
  NETWORK_ERROR_TEXT,
  TIMEOUT_ERROR_TEXT,
  apiErrorText,
} from './errors'

export { ApiError } from './errors'

export interface SessionUser {
  id: string
  username: string
  displayName: string
  role: 'admin' | 'member' | 'viewer'
}
export interface AuthSession {
  user: SessionUser
  accessToken: string
  expiresIn: number
}
// A relative URL keeps refresh cookies on the application's origin, including in development.
const baseURL = (import.meta.env.VITE_API_BASE_URL ?? '/api/v1').replace(/\/$/, '')
/** 单次请求上限；超时与网络故障都归一为客户端 ApiError，文案由 errors.ts 统一提供。 */
const REQUEST_TIMEOUT_MS = 15_000

/** name 判定的结构化检查：DOMException 不是 Error 子类，浏览器里只能按 name 识别。 */
function isNamedError(cause: unknown, name: string): boolean {
  return (
    typeof cause === 'object' &&
    cause !== null &&
    'name' in cause &&
    (cause as { name: unknown }).name === name
  )
}

/** Retry-After 只接受非负整数秒；HTTP-date 等形式忽略，退回通用限流文案。 */
function parseRetryAfter(value: string | null): number | undefined {
  if (!value) return undefined
  const seconds = Number.parseInt(value, 10)
  return Number.isFinite(seconds) && seconds >= 0 ? seconds : undefined
}

export async function httpRequest<T>(path: string, init: RequestInit = {}, token?: string): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('Accept', 'application/json')
  if (init.body !== undefined) headers.set('Content-Type', 'application/json')
  if (token) headers.set('Authorization', `Bearer ${token}`)
  const timeoutSignal = init.signal ? undefined : AbortSignal.timeout(REQUEST_TIMEOUT_MS)
  let response: Response
  try {
    response = await fetch(`${baseURL}${path}`, {
      ...init,
      headers,
      credentials: 'include',
      signal: init.signal ?? timeoutSignal,
    })
  } catch (cause) {
    // 后端英文错误 message 不进入任何展示路径；这里只区分超时、主动取消与网络故障。
    if (isNamedError(cause, 'TimeoutError') || timeoutSignal?.aborted) {
      throw new ApiError(0, 'timeout', TIMEOUT_ERROR_TEXT)
    }
    if (init.signal?.aborted || isNamedError(cause, 'AbortError')) {
      throw new ApiError(0, 'request_aborted', apiErrorText('request_aborted'))
    }
    throw new ApiError(0, 'network_error', NETWORK_ERROR_TEXT)
  }
  let payload: { data?: T; error?: { code: string; message: string }; requestId?: string }
  try {
    payload = await response.json()
  } catch {
    throw new ApiError(response.status, 'invalid_response', apiErrorText('invalid_response'))
  }
  if (!response.ok) {
    // 只信任 code 与 requestId；后端 message 可能包含内部实现细节，一律不渲染。
    const code = payload.error?.code ?? 'request_failed'
    const retryAfter =
      response.status === 429 ? parseRetryAfter(response.headers.get('Retry-After')) : undefined
    throw new ApiError(
      response.status,
      code,
      apiErrorText(code, retryAfter),
      payload.requestId,
      retryAfter,
    )
  }
  if (!('data' in payload)) {
    throw new ApiError(response.status, 'invalid_response', apiErrorText('invalid_response'), payload.requestId)
  }
  return payload.data as T
}
export const authAPI = {
  login: (username: string, password: string, remember: boolean) => httpRequest<AuthSession>('/auth/login', { method: 'POST', body: JSON.stringify({ username, password, remember }) }),
  refresh: () => httpRequest<AuthSession>('/auth/refresh', { method: 'POST', body: '{}' }),
  logout: () => httpRequest<{ loggedOut: boolean }>('/auth/logout', { method: 'POST', body: '{}' }),
  me: (token: string) => httpRequest<SessionUser>('/auth/me', {}, token),
}
