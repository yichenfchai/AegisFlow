import { useMemo, useState } from 'react'
import { Link, useParams } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, DataTable, Field, Input, PageHeader, Select, type Column } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import {
  projectsAPI,
  PROJECT_MEMBER_ROLE_TEXT,
  PROJECT_VISIBILITY_TEXT,
  type ProjectMember,
  type ProjectMemberRole,
} from '@/api/projects'
import { adminAPI } from '@/api/account'
import { useSession } from '@/stores/session'
import { ApiError } from '@/api/http'
import { fmtRel } from '@/api/format'

type MemberRow = ProjectMember & { id: string }

function memberError(cause: unknown, action: string): string {
  if (cause instanceof ApiError) {
    if (cause.status === 403) return `没有${action}权限：私有项目需要是维护者`
    if (cause.status === 404) return '项目不可见，或成员不存在'
    if (cause.status === 409) return '该用户已经是项目成员'
    if (cause.status === 400) return '请求不合法：角色只能是维护者或查看者，且项目创建者不能被移除'
    if (cause.status === 0) return '无法连接服务，请检查网络后重试'
  }
  return cause instanceof Error ? cause.message : '操作失败，请稍后重试'
}

/**
 * 项目成员（真实接口）：私有项目的可见性与写权限都由成员决定。
 * 页面只是交互层——服务端逐请求校验，非成员连项目都读不到（404）。
 */
export function ProjectMembersPage() {
  const { id = '' } = useParams<{ id: string }>()
  const { user } = useSession()
  const queryClient = useQueryClient()
  const [actionError, setActionError] = useState('')
  const [busy, setBusy] = useState('')
  const [addRole, setAddRole] = useState<ProjectMemberRole>('viewer')
  const [addUser, setAddUser] = useState('')

  const project = useQuery({
    queryKey: ['project', id],
    queryFn: () => projectsAPI.project(id),
    enabled: id !== '',
    retry: false,
  })
  const members = useQuery({
    queryKey: ['project-members', id],
    queryFn: () => projectsAPI.members(id),
    enabled: id !== '',
    retry: false,
  })
  const isAdmin = user?.role === 'admin'
  // 只有管理员能列出全站用户；maintainer 通过用户 ID 添加成员（见下方说明）。
  const directory = useQuery({
    queryKey: ['users', 'member-picker'],
    queryFn: () => adminAPI.users(100, 0),
    enabled: isAdmin && !project.isError,
  })

  const rows: MemberRow[] = useMemo(
    () => (members.data ?? []).map((m) => ({ ...m, id: m.userId })),
    [members.data],
  )
  const myRole = rows.find((m) => m.userId === user?.id)?.role
  const canManage = isAdmin || myRole === 'maintainer'
  const candidates = (directory.data?.items ?? []).filter((u) => !rows.some((m) => m.userId === u.id))

  const refresh = () => queryClient.invalidateQueries({ queryKey: ['project-members', id] })
  const run = async (key: string, action: string, fn: () => Promise<unknown>) => {
    setActionError('')
    setBusy(key)
    try {
      await fn()
      await refresh()
    } catch (cause) {
      setActionError(memberError(cause, action))
    } finally {
      setBusy('')
    }
  }

  const add = async (e: React.FormEvent) => {
    e.preventDefault()
    const userId = addUser.trim()
    if (!userId) {
      setActionError('请选择或填写用户 ID')
      return
    }
    await run('add', '添加成员', () => projectsAPI.addMember(id, userId, addRole))
    setAddUser('')
  }

  const columns: Column<MemberRow>[] = [
    {
      key: 'username',
      header: '成员',
      render: (m) => (
        <div>
          <div className="font-medium">
            {m.username || '（账号已不存在）'}
            {m.userId === user?.id && <span className="text-label-sm text-outline"> · 你</span>}
            {m.userId === project.data?.createdBy && <span className="text-label-sm text-outline"> · 创建者</span>}
          </div>
          <div className="font-mono text-label-sm text-outline break-all">{m.userId}</div>
        </div>
      ),
    },
    {
      key: 'role',
      header: '角色',
      render: (m) =>
        m.role === 'maintainer' ? (
          <StatusBadge tone="success">{PROJECT_MEMBER_ROLE_TEXT.maintainer}</StatusBadge>
        ) : (
          <StatusBadge tone="neutral">{PROJECT_MEMBER_ROLE_TEXT.viewer}</StatusBadge>
        ),
    },
    { key: 'createdAt', header: '加入时间', render: (m) => fmtRel(m.createdAt * 1000) },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (m) => {
        if (!canManage || m.userId === project.data?.createdBy) return <span className="text-label-sm text-outline">—</span>
        const next: ProjectMemberRole = m.role === 'maintainer' ? 'viewer' : 'maintainer'
        return (
          <div className="flex items-center justify-end gap-space-sm">
            <Button
              variant="secondary"
              size="sm"
              disabled={busy !== ''}
              onClick={() => void run(`role-${m.userId}`, '变更角色', () => projectsAPI.updateMember(id, m.userId, next))}
            >
              {busy === `role-${m.userId}` ? '处理中...' : `设为${PROJECT_MEMBER_ROLE_TEXT[next]}`}
            </Button>
            <Button
              variant="danger"
              size="sm"
              disabled={busy !== ''}
              onClick={() => void run(`remove-${m.userId}`, '移除成员', () => projectsAPI.removeMember(id, m.userId))}
            >
              {busy === `remove-${m.userId}` ? '处理中...' : '移除'}
            </Button>
          </div>
        )
      },
    },
  ]

  if (project.error) {
    const status = project.error instanceof ApiError ? project.error.status : 0
    return (
      <>
        <PageHeader title="项目成员" />
        <p role="alert" className="text-body-md text-error py-space-lg">
          {status === 404 ? '未找到该项目：它可能是私有项目，而你不是它的成员。' : memberError(project.error, '读取项目')}
        </p>
        <Link to="/" className="text-body-md text-primary hover:underline">
          返回项目列表
        </Link>
      </>
    )
  }

  return (
    <>
      <PageHeader
        title={project.data ? `${project.data.name} · 成员` : '项目成员'}
        subtitle="维护者可以修改项目与触发构建；查看者只读。全局角色是上限，管理员始终可管理。"
        actions={
          <Link to="/" className="text-body-md text-primary hover:underline">
            返回项目列表
          </Link>
        }
      />
      {project.data && (
        <p className="mb-space-md text-body-sm text-outline">
          可见性：
          {project.data.visibility === 'private' ? (
            <StatusBadge tone="warning">{PROJECT_VISIBILITY_TEXT.private}</StatusBadge>
          ) : (
            <StatusBadge tone="neutral">{PROJECT_VISIBILITY_TEXT.public}</StatusBadge>
          )}
          <span className="ml-space-sm">私有项目只有管理员、创建者与列出的成员能看到。</span>
        </p>
      )}
      {actionError && (
        <p role="alert" className="mb-space-md text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
          {actionError}
        </p>
      )}
      {members.isLoading ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : members.error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {memberError(members.error, '读取成员')}
        </p>
      ) : (
        <DataTable columns={columns} rows={rows} empty="还没有额外成员；创建者已自动成为维护者" />
      )}

      {canManage && (
        <form onSubmit={add} className="mt-space-xl max-w-[640px] space-y-space-md" aria-busy={busy === 'add'}>
          <h2 className="text-title-md font-medium">添加成员</h2>
          {isAdmin ? (
            <Field label="用户" hint="来自用户管理页的现有账号" htmlFor="member-user">
              <Select id="member-user" value={addUser} onChange={(e) => setAddUser(e.target.value)} disabled={busy !== ''}>
                <option value="">请选择用户</option>
                {candidates.map((u) => (
                  <option key={u.id} value={u.id}>
                    {u.username}
                    {u.disabled ? '（已禁用）' : ''}
                  </option>
                ))}
              </Select>
            </Field>
          ) : (
            <Field label="用户 ID" hint="非管理员看不到全站用户列表；向被邀请者索取其用户 ID" htmlFor="member-user">
              <Input id="member-user" mono value={addUser} onChange={(e) => setAddUser(e.target.value)} disabled={busy !== ''} placeholder="用户 ID" />
            </Field>
          )}
          <Field label="角色" hint="维护者可写项目与构建；查看者只读" htmlFor="member-role">
            <Select id="member-role" value={addRole} onChange={(e) => setAddRole(e.target.value as ProjectMemberRole)} disabled={busy !== ''}>
              <option value="viewer">查看者</option>
              <option value="maintainer">维护者</option>
            </Select>
          </Field>
          <Button type="submit" disabled={busy !== '' || addUser.trim() === ''}>
            {busy === 'add' ? '添加中...' : '添加成员'}
          </Button>
        </form>
      )}
    </>
  )
}
