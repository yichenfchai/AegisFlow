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
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly requestId?: string
  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
  }
}
// A relative URL keeps refresh cookies on the application's origin, including in development.
const baseURL = (import.meta.env.VITE_API_BASE_URL ?? '/api/v1').replace(/\/$/, '')
export async function httpRequest<T>(path: string, init: RequestInit = {}, token?: string): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('Accept', 'application/json')
  if (init.body !== undefined) headers.set('Content-Type', 'application/json')
  if (token) headers.set('Authorization', `Bearer ${token}`)
  let response: Response
  try {
    response = await fetch(`${baseURL}${path}`, { ...init, headers, credentials: 'include', signal: init.signal ?? AbortSignal.timeout(15_000) })
  } catch {
    throw new ApiError(0, 'network_error', '无法连接服务，请检查网络或稍后重试')
  }
  let payload: { data?: T; error?: { code: string; message: string }; requestId?: string }
  try { payload = await response.json() } catch { throw new ApiError(response.status, 'invalid_response', '服务返回了无效响应') }
  if (!response.ok) throw new ApiError(response.status, payload.error?.code ?? 'request_failed', payload.error?.message ?? '请求失败', payload.requestId)
  if (!('data' in payload)) throw new ApiError(response.status, 'invalid_response', '服务响应缺少数据', payload.requestId)
  return payload.data as T
}
export const authAPI = {
  login: (username: string, password: string, remember: boolean) => httpRequest<AuthSession>('/auth/login', { method: 'POST', body: JSON.stringify({ username, password, remember }) }),
  refresh: () => httpRequest<AuthSession>('/auth/refresh', { method: 'POST', body: '{}' }),
  logout: () => httpRequest<{ loggedOut: boolean }>('/auth/logout', { method: 'POST', body: '{}' }),
  me: (token: string) => httpRequest<SessionUser>('/auth/me', {}, token),
}
