import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, DataTable, Field, Input, PageHeader, type Column } from '@/components/ui/DataTable'
import { ConfirmDialog } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import { accountAPI, type SessionInfo } from '@/api/account'
import { ApiError } from '@/api/http'
import { fmtRel } from '@/api/format'

/** 账号安全：自助修改密码 + 活跃会话管理（真实接口，无 mock） */
export function AccountSecurityPage() {
  return (
    <>
      <PageHeader title="账号安全" subtitle="修改密码并管理当前账号的活跃会话" />
      <div className="max-w-[880px] space-y-gutter">
        <ChangePasswordCard />
        <SessionsCard />
      </div>
    </>
  )
}

function passwordError(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 401) return '当前密码不正确'
    if (error.status === 400) return '新密码长度需为 12-72 字节'
    if (error.status === 0) return '无法连接服务，请检查网络后重试'
  }
  return error instanceof Error ? error.message : '请求失败，请稍后重试'
}

function byteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

function ChangePasswordCard() {
  const queryClient = useQueryClient()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (busy) return
    setError('')
    setNotice('')
    if (byteLength(next) < 12 || byteLength(next) > 72) {
      setError('新密码长度需为 12-72 字节')
      return
    }
    if (next !== confirm) {
      setError('两次输入的新密码不一致')
      return
    }
    setBusy(true)
    try {
      await accountAPI.changePassword(current, next)
      // Other sessions were revoked server-side; refresh the list to match.
      await queryClient.invalidateQueries({ queryKey: ['account-sessions'] })
      setNotice('密码已更新，除当前会话外的其他会话已全部撤销。')
      setCurrent('')
      setNext('')
      setConfirm('')
    } catch (cause) {
      setError(passwordError(cause))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
      <h2 className="text-headline-md font-semibold mb-space-lg">修改密码</h2>
      <form onSubmit={submit} className="max-w-[420px] space-y-space-lg" aria-busy={busy}>
        <Field label="当前密码" hint="验证通过后才会更新密码；其他会话将被撤销" htmlFor="account-current">
          <Input id="account-current" type="password" autoComplete="current-password" value={current} onChange={(e) => setCurrent(e.target.value)} disabled={busy} />
        </Field>
        <Field label="新密码" hint="12-72 字节；建议混合字母、数字与符号" htmlFor="account-new">
          <Input id="account-new" type="password" autoComplete="new-password" value={next} onChange={(e) => setNext(e.target.value)} disabled={busy} />
        </Field>
        <Field label="确认新密码" htmlFor="account-confirm">
          <Input id="account-confirm" type="password" autoComplete="new-password" value={confirm} onChange={(e) => setConfirm(e.target.value)} disabled={busy} />
        </Field>
        {error && (
          <p role="alert" className="text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
            {error}
          </p>
        )}
        {notice && (
          <p role="status" className="text-body-sm text-success bg-success/10 rounded px-space-lg py-space-md">
            {notice}
          </p>
        )}
        <Button type="submit" disabled={busy || !current || !next || !confirm}>
          {busy ? '提交中...' : '更新密码'}
        </Button>
      </form>
    </section>
  )
}

function SessionsCard() {
  const queryClient = useQueryClient()
  const { data: sessions = [], isLoading, error } = useQuery({ queryKey: ['account-sessions'], queryFn: accountAPI.sessions })
  const [revokingId, setRevokingId] = useState<string | null>(null)
  const [confirmAll, setConfirmAll] = useState(false)
  const [actionError, setActionError] = useState('')

  const refresh = () => queryClient.invalidateQueries({ queryKey: ['account-sessions'] })
  const revoke = async (id: string) => {
    setRevokingId(id)
    setActionError('')
    try {
      await accountAPI.revokeSession(id)
      await refresh()
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : '撤销失败，请稍后重试')
    } finally {
      setRevokingId(null)
    }
  }
  const revokeOthers = async () => {
    setActionError('')
    try {
      await accountAPI.revokeOtherSessions()
      await refresh()
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : '撤销失败，请稍后重试')
    } finally {
      setConfirmAll(false)
    }
  }

  const columns: Column<SessionInfo>[] = [
    { key: 'createdAt', header: '创建时间', render: (s) => (s.createdAt ? fmtRel(s.createdAt * 1000) : '未知') },
    { key: 'expiresAt', header: '过期时间', render: (s) => new Date(s.expiresAt * 1000).toLocaleString() },
    {
      key: 'remember',
      header: '保持登录',
      render: (s) => (s.remember ? <StatusBadge tone="neutral" dot={false}>是</StatusBadge> : <StatusBadge tone="neutral" dot={false}>会话 Cookie</StatusBadge>),
    },
    {
      key: 'current',
      header: '状态',
      render: (s) => (s.current ? <StatusBadge tone="success">当前会话</StatusBadge> : <span className="text-on-surface-variant">其他设备/标签页</span>),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (s) =>
        s.current ? (
          <span className="text-label-sm text-outline">—</span>
        ) : (
          <Button variant="danger" size="sm" disabled={revokingId === s.id} onClick={() => void revoke(s.id)}>
            {revokingId === s.id ? '撤销中...' : '撤销'}
          </Button>
        ),
    },
  ]

  return (
    <section className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
      <div className="flex items-center justify-between mb-space-lg">
        <div>
          <h2 className="text-headline-md font-semibold">活跃会话</h2>
          <p className="text-label-sm text-outline mt-space-xs">撤销后，对应浏览器下次请求会立即退出登录。</p>
        </div>
        <Button variant="secondary" size="sm" disabled={sessions.filter((s) => !s.current).length === 0} onClick={() => setConfirmAll(true)}>
          撤销其他全部会话
        </Button>
      </div>
      {actionError && (
        <p role="alert" className="mb-space-md text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
          {actionError}
        </p>
      )}
      {isLoading ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {error instanceof Error ? error.message : '加载会话失败'}
        </p>
      ) : (
        <DataTable columns={columns} rows={sessions} empty="没有活跃会话" />
      )}
      <ConfirmDialog
        open={confirmAll}
        title="撤销其他全部会话"
        danger
        confirmText="确认撤销"
        onConfirm={() => void revokeOthers()}
        onCancel={() => setConfirmAll(false)}
      >
        将撤销当前账号除本会话外的所有活跃会话，对应浏览器需要重新登录。
      </ConfirmDialog>
    </section>
  )
}
