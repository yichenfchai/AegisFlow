/** Real build pipeline API (P3). Business demo APIs stay in ./index. */
import { apiRequest } from '@/stores/session'
import type { Page } from '@/api/account'

export type BuildStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled' | 'interrupted'

export interface Build {
  id: string
  projectId: string
  buildNumber: number
  status: BuildStatus
  branch: string
  command: string
  triggeredBy: string
  createdAt: number
  startedAt: number
  finishedAt: number
}

export interface LogEvent {
  id: number
  buildId: string
  stream: 'stdout' | 'stderr' | 'system'
  message: string
  createdAt: number
}

export const buildsAPI = {
  builds: (limit = 20, offset = 0, projectId?: string) =>
    apiRequest<Page<Build>>(`/builds?limit=${limit}&offset=${offset}${projectId ? `&projectId=${encodeURIComponent(projectId)}` : ''}`),
  build: (id: string) => apiRequest<Build>(`/builds/${encodeURIComponent(id)}`),
  trigger: (projectId: string, branch?: string) =>
    apiRequest<Build>(`/projects/${encodeURIComponent(projectId)}/builds`, {
      method: 'POST',
      body: JSON.stringify(branch ? { branch } : {}),
    }),
  cancel: (id: string) =>
    apiRequest<Build>(`/builds/${encodeURIComponent(id)}/cancel`, { method: 'POST', body: '{}' }),
  logs: (id: string, after = 0, limit = 200) =>
    apiRequest<{ items: LogEvent[] }>(`/builds/${encodeURIComponent(id)}/logs?after=${after}&limit=${limit}`),
}

export const BUILD_STATUS_TEXT: Record<BuildStatus, string> = {
  queued: '排队中',
  running: '运行中',
  succeeded: '成功',
  failed: '失败',
  cancelled: '已取消',
  interrupted: '已中断',
}

export const BUILD_STATUS_TONE: Record<BuildStatus, 'success' | 'error' | 'running' | 'pending'> = {
  queued: 'pending',
  running: 'running',
  succeeded: 'success',
  failed: 'error',
  cancelled: 'pending',
  interrupted: 'pending',
}
