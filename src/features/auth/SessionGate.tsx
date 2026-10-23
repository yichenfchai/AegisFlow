import { useEffect, useState, type ReactNode } from 'react'
import { Navigate, Outlet, useLocation } from 'react-router'
import { useSession } from '@/stores/session'
import { Button } from '@/components/ui/DataTable'
import { safeReturnTo } from './returnTo'

export function SessionGate({ children, publicOnly = false, adminOnly = false }: {
  children?: ReactNode
  publicOnly?: boolean
  adminOnly?: boolean
}) {
  const { status, user, error, restore } = useSession()
  const location = useLocation()
  const [restoreError, setRestoreError] = useState<string | null>(null)
  const [retrying, setRetrying] = useState(false)

  useEffect(() => {
    // The store owns initial restoration and StrictMode request deduplication.
    void restore().catch((cause: unknown) => {
      setRestoreError(cause instanceof Error ? cause.message : '无法恢复会话，请重试')
    })
  }, [restore])

  const retry = async () => {
    setRestoreError(null)
    setRetrying(true)
    try {
      await restore()
    } catch (cause) {
      setRestoreError(cause instanceof Error ? cause.message : '无法恢复会话，请重试')
    } finally {
      setRetrying(false)
    }
  }

  if (status === 'loading' || retrying) {
    return <div className="min-h-screen bg-bg text-on-surface flex items-center justify-center" role="status" aria-live="polite">正在恢复会话...</div>
  }
  if (status === 'error' || restoreError) {
    return (
      <div className="min-h-screen bg-bg text-on-surface flex flex-col items-center justify-center gap-space-lg">
        <p role="alert">{error || restoreError || '无法恢复会话，请重试'}</p>
        <Button onClick={() => void retry()}>重试恢复会话</Button>
      </div>
    )
  }
  if (status === 'authenticated' && user) {
    if (publicOnly) {
      return <Navigate to={safeReturnTo(new URLSearchParams(location.search).get('returnTo'))} replace />
    }
    if (adminOnly && user.role !== 'admin') {
      return (
        <section className="p-space-xl" aria-labelledby="access-denied-title">
          <h1 id="access-denied-title" className="text-headline-md">无权访问</h1>
          <p className="mt-space-md text-on-surface-variant">此页面仅管理员可访问。请联系管理员申请权限。</p>
        </section>
      )
    }
    return children ?? <Outlet />
  }
  if (publicOnly && status === 'anonymous') return children ?? <Outlet />
  const returnTo = safeReturnTo(`${location.pathname}${location.search}${location.hash}`)
  return <Navigate to={`/login?${new URLSearchParams({ returnTo })}`} replace />
}
