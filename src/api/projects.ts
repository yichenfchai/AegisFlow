/** Real project registry API (P2). Business demo APIs stay in ./index. */
import { apiRequest } from '@/stores/session'
import type { Page } from '@/api/account'

export interface Project {
  id: string
  name: string
  description: string
  repoUrl: string
  defaultBranch: string
  /** 构建命令快照来源；为空的项目不能触发构建 */
  buildCommand: string
  status: 'active' | 'archived'
  createdBy: string
  createdAt: number
  updatedAt: number
}

export type ProjectInput = {
  name: string
  description?: string
  repoUrl?: string
  defaultBranch?: string
  /** 单行 shell 命令，最多 512 字节；触发构建时固化到构建行 */
  buildCommand?: string
}

export type ProjectPatch = Partial<
  Pick<Project, 'name' | 'description' | 'repoUrl' | 'defaultBranch' | 'buildCommand'>
> & {
  archived?: boolean
}

export const projectsAPI = {
  projects: (limit = 20, offset = 0, status?: 'active' | 'archived') =>
    apiRequest<Page<Project>>(`/projects?limit=${limit}&offset=${offset}${status ? `&status=${status}` : ''}`),
  project: (id: string) => apiRequest<Project>(`/projects/${encodeURIComponent(id)}`),
  create: (input: ProjectInput) =>
    apiRequest<Project>('/projects', { method: 'POST', body: JSON.stringify(input) }),
  update: (id: string, patch: ProjectPatch) =>
    apiRequest<Project>(`/projects/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify(patch) }),
}
