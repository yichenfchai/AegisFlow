/** Real project registry API (P2). Business demo APIs stay in ./index. */
import { apiRequest } from '@/stores/session'
import type { Page } from '@/api/account'

export type ProjectVisibility = 'public' | 'private'
export type ProjectMemberRole = 'maintainer' | 'viewer'

export interface Project {
  id: string
  name: string
  description: string
  repoUrl: string
  defaultBranch: string
  /** 构建命令快照来源；为空的项目不能触发构建 */
  buildCommand: string
  status: 'active' | 'archived'
  /** public 对所有角色可见；private 仅 admin、创建者与项目成员可见 */
  visibility: ProjectVisibility
  createdBy: string
  createdAt: number
  updatedAt: number
}

/** 项目成员：maintainer 可读写，viewer 只读。 */
export interface ProjectMember {
  projectId: string
  userId: string
  username: string
  role: ProjectMemberRole
  createdBy: string
  createdAt: number
}

export type ProjectInput = {
  name: string
  description?: string
  repoUrl?: string
  defaultBranch?: string
  /** 单行 shell 命令，最多 512 字节；触发构建时固化到构建行 */
  buildCommand?: string
  /** 省略时后端按 public 处理 */
  visibility?: ProjectVisibility
}

export type ProjectPatch = Partial<
  Pick<Project, 'name' | 'description' | 'repoUrl' | 'defaultBranch' | 'buildCommand' | 'visibility'>
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
  members: (id: string) => apiRequest<ProjectMember[]>(`/projects/${encodeURIComponent(id)}/members`),
  addMember: (id: string, userId: string, role: ProjectMemberRole) =>
    apiRequest<ProjectMember>(`/projects/${encodeURIComponent(id)}/members`, {
      method: 'POST',
      body: JSON.stringify({ userId, role }),
    }),
  updateMember: (id: string, userId: string, role: ProjectMemberRole) =>
    apiRequest<ProjectMember>(`/projects/${encodeURIComponent(id)}/members/${encodeURIComponent(userId)}`, {
      method: 'PATCH',
      body: JSON.stringify({ role }),
    }),
  removeMember: (id: string, userId: string) =>
    apiRequest<{ removed: boolean }>(`/projects/${encodeURIComponent(id)}/members/${encodeURIComponent(userId)}`, {
      method: 'DELETE',
    }),
}

export const PROJECT_VISIBILITY_TEXT: Record<ProjectVisibility, string> = {
  public: '公开',
  private: '私有',
}

export const PROJECT_MEMBER_ROLE_TEXT: Record<ProjectMemberRole, string> = {
  maintainer: '维护者',
  viewer: '查看者',
}
