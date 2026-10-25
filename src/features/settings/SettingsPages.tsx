import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Button,
  ConfirmDialog,
  DataTable,
  Field,
  Input,
  PageHeader,
  Pagination,
  Select,
  type Column,
} from '@/components/ui/DataTable'
import { Segmented, StatusBadge } from '@/components/ui/Badge'
import { adminAPI, AUDIT_ACTION_TEXT, type AccountUser, type AuditEntry } from '@/api/account'
import { ApiError } from '@/api/http'
import { useSession } from '@/stores/session'
import { fmtRel } from '@/api/format'

const PAGE_SIZE = 10

/** 管理员页：真实账号管理（角色/禁用/重置密码）与鉴权审计查询 */
export function SettingsPage() {
  const [tab, setTab] = useState<'users' | 'audit' | 'about'>('users')
  return (
    <>
      <PageHeader title="用户与审计" subtitle="账号管理、角色与安全审计，均为真实接口" />
      <Segmented
        className="mb-space-xl"
        value={tab}
        onChange={setTab}
        options={[
          { value: 'users', label: '用户管理' },
          { value: 'audit', label: '审计' },
          { value: 'about', label: '关于' },
        ]}
      />
      {tab === 'users' && <UsersTab />}
      {tab === 'audit' && <AuditTab />}
      {tab === 'about' && <AboutTab />}
    </>
  )
}

function opError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 0) return '无法连接服务，请检查网络后重试'
    if (cause.status === 400) return '操作被拒绝：参数不合法，或触发最后管理员保护'
    if (cause.status === 404) return '目标用户不存在，请刷新列表'
  }
  return cause instanceof Error ? cause.message : '操作失败，请稍后重试'
}

function UsersTab() {
  const queryClient = useQueryClient()
  const { user: me } = useSession()
  const [page, setPage] = useState(1)
  const offset = (page - 1) * PAGE_SIZE
  const { data, isLoading, error: loadError } = useQuery({
    queryKey: ['admin-users', page],
    queryFn: () => adminAPI.users(PAGE_SIZE, offset),
  })
  const users = data?.items ?? []
  const total = data?.total ?? 0

  const [creating, setCreating] = useState(false)
  const [resetTarget, setResetTarget] = useState<AccountUser | null>(null)
  const [resetValue, setResetValue] = useState('')
  const [toggleTarget, setToggleTarget] = useState<AccountUser | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const refresh = () => queryClient.invalidateQueries({ queryKey: ['admin-users'] })
  const run = async (action: () => Promise<unknown>) => {
    setError('')
    setBusy(true)
    try {
      await action()
      await refresh()
      return true
    } catch (cause) {
      setError(opError(cause))
      await refresh()
      return false
    } finally {
      setBusy(false)
    }
  }

  const columns: Column<AccountUser>[] = [
    {
      key: 'username',
      header: '用户',
      render: (u) => (
        <div>
          <div className="font-medium">
            {u.username}
            {me?.id === u.id && <span className="ml-space-sm text-label-sm text-outline">（当前登录）</span>}
          </div>
          <div className="text-label-sm text-outline">{u.displayName}</div>
        </div>
      ),
    },
    {
      key: 'role',
      header: '角色',
      render: (u) => (
        <Select value={u.role} disabled={busy} onChange={(e) => void run(() => adminAPI.updateUser(u.id, { role: e.target.value }))} className="!py-1">
          <option value="admin">管理员</option>
          <option value="member">成员</option>
          <option value="viewer">只读用户</option>
        </Select>
      ),
    },
    {
      key: 'disabled',
      header: '状态',
      render: (u) =>
        u.disabled ? <StatusBadge tone="error">已禁用</StatusBadge> : <StatusBadge tone="success">正常</StatusBadge>,
    },
    { key: 'createdAt', header: '创建时间', render: (u) => (u.createdAt ? fmtRel(u.createdAt * 1000) : '未知') },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (u) => (
        <div className="flex justify-end gap-space-md">
          <Button variant="secondary" size="sm" disabled={busy} onClick={() => { setResetTarget(u); setResetValue('') }}>
            重置密码
          </Button>
          <Button variant={u.disabled ? 'secondary' : 'danger'} size="sm" disabled={busy} onClick={() => setToggleTarget(u)}>
            {u.disabled ? '启用' : '禁用'}
          </Button>
        </div>
      ),
    },
  ]

  return (
    <div className="max-w-[1080px]">
      {error && (
        <p role="alert" className="mb-space-md text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
          {error}
        </p>
      )}
      <div className="flex justify-end mb-space-lg">
        <Button icon="person_add" disabled={busy} onClick={() => setCreating((v) => !v)}>
          {creating ? '收起表单' : '创建用户'}
        </Button>
      </div>
      {creating && <CreateUserCard onDone={() => { setCreating(false); void refresh() }} onError={setError} />}
      {isLoading ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : loadError ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {loadError instanceof Error ? loadError.message : '加载用户失败'}
        </p>
      ) : (
        <>
          <DataTable columns={columns} rows={users} empty="暂无用户" />
          <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPage={setPage} />
        </>
      )}
      <ConfirmDialog
        open={resetTarget !== null}
        title={`重置 ${resetTarget?.username ?? ''} 的密码`}
        confirmText="确认重置"
        onConfirm={() => {
          if (byteLength(resetValue) < 12 || byteLength(resetValue) > 72) {
            setError('新密码长度需为 12-72 字节')
            return
          }
          const target = resetTarget
          if (!target) return
          void run(async () => {
            await adminAPI.resetPassword(target.id, resetValue)
            setResetTarget(null)
          })
        }}
        onCancel={() => setResetTarget(null)}
      >
        <p className="mb-space-md">该用户的所有会话将被撤销，需使用新密码重新登录。</p>
        <Field label="新密码" hint="12-72 字节" htmlFor="reset-password">
          <Input id="reset-password" type="password" value={resetValue} onChange={(e) => setResetValue(e.target.value)} />
        </Field>
      </ConfirmDialog>
      <ConfirmDialog
        open={toggleTarget !== null}
        title={toggleTarget?.disabled ? `启用 ${toggleTarget?.username ?? ''}` : `禁用 ${toggleTarget?.username ?? ''}`}
        danger={!toggleTarget?.disabled}
        confirmText={toggleTarget?.disabled ? '确认启用' : '确认禁用'}
        onConfirm={() => {
          const target = toggleTarget
          if (!target) return
          void run(async () => {
            await adminAPI.updateUser(target.id, { disabled: !target.disabled })
            setToggleTarget(null)
          })
        }}
        onCancel={() => setToggleTarget(null)}
      >
        {toggleTarget?.disabled
          ? '启用后该用户可重新登录。'
          : '禁用后该用户的所有会话立即失效，且无法登录，直到重新启用。'}
      </ConfirmDialog>
    </div>
  )
}

function byteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

function CreateUserCard({ onDone, onError }: { onDone: () => void; onError: (message: string) => void }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [role, setRole] = useState('viewer')
  const [busy, setBusy] = useState(false)

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (busy) return
    onError('')
    if (byteLength(password) < 12 || byteLength(password) > 72) {
      onError('密码长度需为 12-72 字节')
      return
    }
    setBusy(true)
    try {
      await adminAPI.createUser({ username, password, displayName: displayName || undefined, role })
      onDone()
    } catch (cause) {
      onError(opError(cause))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl mb-space-lg">
      <h2 className="text-headline-md font-semibold mb-space-lg">创建用户</h2>
      <div className="grid grid-cols-2 gap-space-xl">
        <Field label="用户名" hint="3-64 位字母、数字、点、下划线或连字符" htmlFor="create-username">
          <Input id="create-username" value={username} onChange={(e) => setUsername(e.target.value)} disabled={busy} />
        </Field>
        <Field label="显示名" hint="留空则与用户名相同" htmlFor="create-display-name">
          <Input id="create-display-name" value={displayName} onChange={(e) => setDisplayName(e.target.value)} disabled={busy} />
        </Field>
        <Field label="初始密码" hint="12-72 字节" htmlFor="create-password">
          <Input id="create-password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} disabled={busy} />
        </Field>
        <Field label="角色" htmlFor="create-role">
          <Select id="create-role" value={role} onChange={(e) => setRole(e.target.value)} disabled={busy}>
            <option value="viewer">只读用户</option>
            <option value="member">成员</option>
            <option value="admin">管理员</option>
          </Select>
        </Field>
      </div>
      <div className="mt-space-lg">
        <Button type="submit" disabled={busy || !username || !password}>
          {busy ? '创建中...' : '创建'}
        </Button>
      </div>
    </form>
  )
}

const AUDIT_COLUMNS: Column<AuditEntry>[] = [
  { key: 'createdAt', header: '时间', render: (e) => new Date(e.createdAt * 1000).toLocaleString() },
  { key: 'actor', header: '操作人', render: (e) => e.actorUsername || '系统' },
  { key: 'action', header: '动作', render: (e) => AUDIT_ACTION_TEXT[e.action] ?? e.action },
  {
    key: 'result',
    header: '结果',
    render: (e) => (e.result === 'success' ? <StatusBadge tone="success">成功</StatusBadge> : <StatusBadge tone="error">失败</StatusBadge>),
  },
  {
    key: 'detail',
    header: '对象 / 详情',
    render: (e) => (
      <div>
        <div className="font-mono text-label-sm text-outline" title={e.requestId}>
          {e.objectType}:{e.objectId.slice(0, 10)}
        </div>
        {e.detail && <div className="text-label-sm text-outline">{e.detail}</div>}
      </div>
    ),
  },
]

function AuditTab() {
  const [page, setPage] = useState(1)
  const offset = (page - 1) * PAGE_SIZE
  const { data, isLoading, error } = useQuery({
    queryKey: ['admin-audit', page],
    queryFn: () => adminAPI.audit(PAGE_SIZE, offset),
  })
  return (
    <div className="max-w-[1080px]">
      {error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {error instanceof Error ? error.message : '加载审计失败'}
        </p>
      ) : isLoading ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : (
        <>
          <DataTable columns={AUDIT_COLUMNS} rows={data?.items ?? []} empty="暂无审计记录" />
          <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
        </>
      )}
    </div>
  )
}

function AboutTab() {
  return (
    <div className="max-w-[880px]">
      <section className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
        <h2 className="text-headline-md font-semibold mb-space-lg">关于 AegisFlow</h2>
        <div className="space-y-space-md text-body-md text-on-surface-variant">
          <p>轻量交付治理平台 · React + Go/Gin 模块化单体，PostgreSQL 共享存储、Redis 共享限流。</p>
          <div className="grid grid-cols-2 gap-space-lg text-body-sm">
            <div>鉴权：JWT + 轮换刷新 Cookie</div>
            <div>角色：admin / member / viewer</div>
            <div>© 2026 AegisFlow</div>
          </div>
          <p className="text-body-sm text-outline">
            业务模块（项目、构建、部署、审批等）按 TODO 里程碑推进；当前版本提供真实账号管理与审计。
          </p>
        </div>
      </section>
    </div>
  )
}
