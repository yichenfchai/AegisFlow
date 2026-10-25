import { Icon } from '@/components/ui/Icon'
import { errorMessage, useSession } from '@/stores/session'
import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router'

export function TopBar({ onOpenPalette }: { onOpenPalette?: () => void }) {
  const { user, logout } = useSession()
  const nav = useNavigate()
  const queryClient = useQueryClient()
  const [loggingOut, setLoggingOut] = useState(false)
  const [logoutError, setLogoutError] = useState<string | null>(null)
  const handleLogout = async () => {
    if (loggingOut) return
    setLoggingOut(true)
    setLogoutError(null)
    try {
      await logout()
      void queryClient.cancelQueries()
      queryClient.clear()
      nav('/login', { replace: true })
    } catch (cause) {
      setLogoutError(errorMessage(cause))
    } finally {
      setLoggingOut(false)
    }
  }
  return (
    <header className="fixed top-0 left-0 right-0 h-12 z-50 bg-surface border-b border-surface-container-highest flex items-center px-space-lg gap-space-lg">
      <div className="flex items-baseline gap-space-sm">
        <span className="text-headline-md font-semibold text-on-surface tracking-tight">
          AegisFlow
        </span>
      </div>

      <div className="flex-1" />

      <button
        onClick={onOpenPalette}
        className="flex items-center gap-space-md bg-surface-container rounded px-space-md py-1 text-body-sm text-outline hover:text-on-surface-variant border border-surface-container-highest"
      >
        <Icon name="search" size={14} />
        搜索资源...
        <span className="font-mono text-label-sm border border-outline-variant rounded px-1">
          ⌘K
        </span>
      </button>

      {logoutError && (
        <div role="alert" className="absolute top-14 right-space-lg max-w-lg rounded border border-error/40 bg-surface p-space-lg text-body-sm text-error shadow-lg">
          <p>退出失败：{logoutError}。会话已保留，请重试。</p>
          <button type="button" disabled={loggingOut} onClick={() => void handleLogout()} className="mt-space-sm underline disabled:opacity-50">
            {loggingOut ? '正在退出...' : '重试退出'}
          </button>
        </div>
      )}
      <div className="flex items-center gap-space-md">
        <div className="text-right leading-tight">
          <div className="text-body-sm text-on-surface font-medium">{user?.displayName ?? user?.username}</div>
          <div className="text-label-sm text-outline">{user?.role === 'admin' ? '管理员' : user?.role === 'member' ? '成员' : '只读用户'}</div>
        </div>
        <button
          type="button"
          onClick={() => void handleLogout()}
          disabled={loggingOut}
          aria-label={loggingOut ? '正在退出登录' : '退出登录'}
          title={loggingOut ? '正在退出登录' : '退出登录'}
          className="w-7 h-7 rounded-full bg-primary-container flex items-center justify-center hover:brightness-110"
        >
          <Icon name="person" size={16} className="text-on-primary-container" />
        </button>
      </div>
    </header>
  )
}
