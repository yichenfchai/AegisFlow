import { NavLink } from 'react-router'
import { NAV_GROUPS } from './nav'
import { Icon } from '@/components/ui/Icon'
import { useSession } from '@/stores/session'

export function Sidebar() {
  const { user } = useSession()
  return (
    <aside className="fixed left-0 top-12 bottom-7 w-[200px] z-40 bg-surface-container-low border-r border-surface-container-highest flex flex-col justify-between overflow-y-auto">
      <div className="py-space-md px-space-xs space-y-space-md">
        {NAV_GROUPS.map((group) => (
          <section key={group.title}>
            <div className="px-space-sm pb-space-xs text-label-sm text-text-2 uppercase tracking-wider">
              {group.title}
            </div>
            <nav className="space-y-0.5">
              {group.items
                .filter((item) => !item.adminOnly || user?.role === 'admin')
                .map((item) => (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    end={item.to === '/'}
                    className={({ isActive }) =>
                      `flex items-center gap-space-md px-space-sm py-[5px] mx-space-xs rounded text-body-md transition-colors ${
                        isActive
                          ? 'bg-surface-container-high text-on-surface font-medium border-l-2 border-primary'
                          : 'text-on-surface-variant hover:bg-surface-container hover:text-on-surface border-l-2 border-transparent'
                      }`
                    }
                  >
                    <Icon name={item.icon} size={16} />
                    <span className="flex-1">{item.label}</span>
                    {item.badge ? (
                      <span className="text-label-sm bg-error-container text-on-error-container rounded px-1">
                        {item.badge}
                      </span>
                    ) : null}
                  </NavLink>
                ))}
            </nav>
          </section>
        ))}
      </div>
      <div className="py-space-md px-space-xs border-t border-surface-container-highest">
        <NavLink
          to="/account"
          className={({ isActive }) =>
            `flex items-center gap-space-md px-space-sm py-[5px] mx-space-xs rounded text-body-md transition-colors ${
              isActive
                ? 'bg-surface-container-high text-on-surface font-medium'
                : 'text-on-surface-variant hover:bg-surface-container hover:text-on-surface'
            }`
          }
        >
          <Icon name="shield_lock" size={16} />
          账号安全
        </NavLink>
        {user?.role === 'admin' && (
          <NavLink
            to="/settings"
            className={({ isActive }) =>
              `flex items-center gap-space-md px-space-sm py-[5px] mx-space-xs rounded text-body-md transition-colors ${
                isActive
                  ? 'bg-surface-container-high text-on-surface font-medium'
                  : 'text-on-surface-variant hover:bg-surface-container hover:text-on-surface'
              }`
            }
          >
            <Icon name="settings" size={16} />
            用户与审计
          </NavLink>
        )}
      </div>
    </aside>
  )
}
