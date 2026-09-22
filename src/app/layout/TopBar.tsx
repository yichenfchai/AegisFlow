import { Icon } from '@/components/ui/Icon'
import { useSession } from '@/stores/session'
import { useNavigate } from 'react-router'

export function TopBar({ onOpenPalette }: { onOpenPalette?: () => void }) {
  const { user, logout } = useSession()
  const nav = useNavigate()
  const handleLogout = () => {
    logout()
    nav('/login', { replace: true })
  }
  return (
    <header className="fixed top-0 left-0 right-0 h-12 z-50 bg-surface border-b border-surface-container-highest flex items-center px-space-lg gap-space-lg">
      <div className="flex items-baseline gap-space-sm">
        <span className="text-headline-md font-semibold text-on-surface tracking-tight">
          GoPulse
        </span>
        <span className="font-mono text-label-sm text-outline bg-surface-container rounded px-1">
          v2.4
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

      <div className="flex items-center gap-space-md">
        <div className="text-right leading-tight">
          <div className="text-body-sm text-on-surface font-medium">{user?.displayName ?? 'Alex Mercer'}</div>
          <div className="text-label-sm text-outline">{user?.role === 'admin' ? 'Platform SRE' : user?.role}</div>
        </div>
        <button
          onClick={handleLogout}
          title="退出登录"
          className="w-7 h-7 rounded-full bg-primary-container flex items-center justify-center hover:brightness-110"
        >
          <Icon name="person" size={16} className="text-on-primary-container" />
        </button>
      </div>
    </header>
  )
}
