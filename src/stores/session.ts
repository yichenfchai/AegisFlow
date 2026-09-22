import { create } from 'zustand'

interface SessionUser {
  id: string
  username: string
  displayName: string
  role: 'admin' | 'member' | 'viewer'
}

interface SessionState {
  user: SessionUser | null
  accessToken: string | null
  login: (user: SessionUser, token: string) => void
  logout: () => void
}

const SESSION_KEY = 'gp_session'

/** 解析 JWT payload 并校验 exp；过期/损坏返回 null */
function parseToken(token: string): { exp: number } | null {
  try {
    const [, payloadB64] = token.split('.')
    const payload = JSON.parse(atob(payloadB64))
    if (typeof payload.exp !== 'number' || payload.exp * 1000 < Date.now()) return null
    return payload
  } catch {
    return null
  }
}

function restore(): { user: SessionUser | null; accessToken: string | null } {
  try {
    const raw = localStorage.getItem(SESSION_KEY)
    if (!raw) return { user: null, accessToken: null }
    const { user, accessToken } = JSON.parse(raw)
    // token 缺失或已过期 → 丢弃会话
    if (!accessToken || !parseToken(accessToken)) {
      localStorage.removeItem(SESSION_KEY)
      return { user: null, accessToken: null }
    }
    return { user, accessToken }
  } catch {
    return { user: null, accessToken: null }
  }
}

/**
 * 会话存储：localStorage 持久化 JWT 形状，恢复时校验 exp。
 * 真实后端就绪后：login() 由 /auth/login 响应驱动，refresh 走 /auth/refresh。
 */
export const useSession = create<SessionState>((set) => ({
  ...restore(),
  login: (user, accessToken) => {
    localStorage.setItem(SESSION_KEY, JSON.stringify({ user, accessToken, ts: Date.now() }))
    set({ user, accessToken })
  },
  logout: () => {
    localStorage.removeItem(SESSION_KEY)
    set({ user: null, accessToken: null })
  },
}))

/** mock JWT 签发（形状对齐后端：header.payload.signature 的 payload 部分） */
export function mockJwtFor(username: string, ttlSec = 60 * 60 * 24 * 30): string {
  const payload = {
    sub: username,
    iss: 'gopulse',
    exp: Math.floor(Date.now() / 1000) + ttlSec,
    scope: 'read write approve',
  }
  return `mock.${btoa(JSON.stringify(payload))}.sig`
}
