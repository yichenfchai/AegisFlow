import { Icon } from '@/components/ui/Icon'
import { ConfirmDialog } from '@/components/ui/DataTable'
import { errorMessage, useSession } from '@/stores/session'
import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate } from 'react-router'
import { useQueryClient } from '@tanstack/react-query'

const ROLE_TEXT: Record<string, string> = { admin: '管理员', member: '成员', viewer: '只读用户' }

export function TopBar({ onOpenPalette }: { onOpenPalette?: () => void }) {
  const { user, logout } = useSession()
  const nav = useNavigate()
  const queryClient = useQueryClient()
  const [loggingOut, setLoggingOut] = useState(false)
  const [logoutError, setLogoutError] = useState<string | null>(null)
  const [menuOpen, setMenuOpen] = useState(false)
  const [confirmingLogout, setConfirmingLogout] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!menuOpen) return
    const onPointerDown = (event: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(event.target as Node)) setMenuOpen(false)
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setMenuOpen(false)
    }
    document.addEventListener('mousedown', onPointerDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onPointerDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [menuOpen])

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

  const requestLogout = () => {
    setMenuOpen(false)
    setLogoutError(null)
    setConfirmingLogout(true)
  }
  const confirmLogout = () => {
    setConfirmingLogout(false)
    void handleLogout()
  }

  return (
    <>
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
      <div className="relative flex items-center gap-space-md" ref={menuRef}>
        <div className="text-right leading-tight">
          <div className="text-body-sm text-on-surface font-medium">{user?.displayName ?? user?.username}</div>
          <div className="text-label-sm text-outline">{user?.role ? ROLE_TEXT[user.role] ?? user.role : ''}</div>
        </div>
        <button
          type="button"
          onClick={() => setMenuOpen((open) => !open)}
          disabled={loggingOut}
          aria-label="打开用户菜单"
          aria-haspopup="menu"
          aria-expanded={menuOpen}
          title="用户菜单"
          className="w-7 h-7 rounded-full bg-primary-container flex items-center justify-center hover:brightness-110 disabled:opacity-50"
        >
          <Icon name="person" size={16} className="text-on-primary-container" />
        </button>
        {menuOpen && (
          <div
            role="menu"
            aria-label="用户菜单"
            className="absolute right-0 top-full mt-space-sm w-[200px] rounded-lg border border-surface-container-highest bg-surface-container p-space-xs shadow-lg"
          >
            <div className="px-space-md py-space-sm border-b border-surface-container-highest mb-space-xs">
              <div className="text-body-sm text-on-surface font-medium truncate">{user?.displayName ?? user?.username}</div>
              <div className="text-label-sm text-outline">{user?.username}</div>
            </div>
            <Link
              role="menuitem"
              to="/account"
              onClick={() => setMenuOpen(false)}
              className="flex items-center gap-space-sm px-space-md py-space-sm rounded text-body-sm text-on-surface-variant hover:bg-surface-container-high hover:text-on-surface"
            >
              <Icon name="shield_lock" size={14} />
              账号安全
            </Link>
            <button
              type="button"
              role="menuitem"
              onClick={requestLogout}
              className="w-full flex items-center gap-space-sm px-space-md py-space-sm rounded text-body-sm text-error hover:bg-error/10"
            >
              <Icon name="logout" size={14} />
              退出登录
            </button>
          </div>
        )}
      </div>
      </header>
      <ConfirmDialog
        open={confirmingLogout}
        title="退出登录"
        danger
        confirmText="确认退出"
        onConfirm={confirmLogout}
        onCancel={() => setConfirmingLogout(false)}
      >
        退出后当前浏览器的会话将被撤销，需要重新登录。
      </ConfirmDialog>
    </>
  )
}
