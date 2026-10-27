/** Real account-management and administration APIs (P1). Business demo APIs stay in ./index. */
import { apiRequest } from '@/stores/session'
import type { SessionUser } from '@/api/http'

export interface AccountUser extends SessionUser {
  disabled: boolean
  createdAt: number
}

export interface SessionInfo {
  id: string
  createdAt: number
  expiresAt: number
  remember: boolean
  current: boolean
}

export interface AuditEntry {
  id: string
  createdAt: number
  actorId: string
  actorUsername: string
  action: string
  objectType: string
  objectId: string
  result: 'success' | 'failure'
  detail: string
  requestId: string
}

export interface Page<T> {
  items: T[]
  total: number
  limit: number
  offset: number
}

export const accountAPI = {
  changePassword: (currentPassword: string, newPassword: string) =>
    apiRequest<{ changed: boolean }>('/auth/password', {
      method: 'POST',
      body: JSON.stringify({ currentPassword, newPassword }),
    }),
  sessions: () => apiRequest<SessionInfo[]>('/auth/sessions'),
  revokeSession: (id: string) =>
    apiRequest<{ revoked: boolean }>(`/auth/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  revokeOtherSessions: () => apiRequest<{ revoked: number }>('/auth/sessions', { method: 'DELETE' }),
}

export const adminAPI = {
  users: (limit = 20, offset = 0) =>
    apiRequest<Page<AccountUser>>(`/users?limit=${limit}&offset=${offset}`),
  createUser: (input: { username: string; password: string; displayName?: string; role: string }) =>
    apiRequest<AccountUser>('/users', { method: 'POST', body: JSON.stringify(input) }),
  updateUser: (id: string, patch: { role?: string; disabled?: boolean }) =>
    apiRequest<{ updated: boolean }>(`/users/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    }),
  resetPassword: (id: string, password: string) =>
    apiRequest<{ reset: boolean }>(`/users/${encodeURIComponent(id)}/password-reset`, {
      method: 'POST',
      body: JSON.stringify({ password }),
    }),
  audit: (limit = 20, offset = 0) =>
    apiRequest<Page<AuditEntry>>(`/audit?limit=${limit}&offset=${offset}`),
}

export const AUDIT_ACTION_TEXT: Record<string, string> = {
  'auth.login': '登录',
  'user.create': '创建用户',
  'user.password_change': '修改密码',
  'user.password_reset': '重置密码',
  'user.disable': '禁用账号',
  'user.enable': '启用账号',
  'user.role_change': '变更角色',
  'session.revoke': '撤销会话',
  'session.revoke_all': '撤销其他会话',
  'project.create': '创建项目',
  'project.update': '更新项目',
  'project.archive': '归档项目',
  'project.unarchive': '恢复项目',
  'build.trigger': '触发构建',
  'build.cancel': '取消构建',
}

export const AUDIT_OBJECT_TEXT: Record<string, string> = {
  user: '用户',
  session: '会话',
  project: '项目',
  build: '构建',
}
