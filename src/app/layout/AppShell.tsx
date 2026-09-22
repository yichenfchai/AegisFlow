import { Outlet, useNavigate } from 'react-router'
import { Sidebar } from './Sidebar'
import { TopBar } from './TopBar'
import { StatusBar } from './StatusBar'
import { useMockTelemetry } from '@/stores/telemetry'
import { CommandPalette, usePalette } from '@/app/CommandPalette'
import { useSession } from '@/stores/session'
import { useEffect } from 'react'

export function AppShell() {
  useMockTelemetry()
  const setOpen = usePalette((s) => s.setOpen)
  const user = useSession((s) => s.user)
  const nav = useNavigate()

  useEffect(() => {
    if (!user) nav('/login', { replace: true })
  }, [user, nav])

  if (!user) return null

  return (
    <div className="min-h-screen bg-bg text-on-surface">
      <TopBar onOpenPalette={() => setOpen(true)} />
      <CommandPalette />
      <Sidebar />
      <main className="pt-12 pb-7 pl-[200px] min-h-screen">
        <div className="p-6 max-w-[1440px]">
          <Outlet />
        </div>
      </main>
      <StatusBar />
    </div>
  )
}
