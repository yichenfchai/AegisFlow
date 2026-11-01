import { Outlet } from 'react-router'
import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Sidebar } from './Sidebar'
import { TopBar } from './TopBar'
import { StatusBar } from './StatusBar'
import { CommandPalette, usePalette } from '@/app/CommandPalette'
import { useSession } from '@/stores/session'

export function AppShell() {
  const setOpen = usePalette((s) => s.setOpen)
  const queryClient = useQueryClient()

  useEffect(() => useSession.subscribe((state, previous) => {
    if (state.user?.id !== previous.user?.id) {
      // Synchronous subscription clears the previous account's data before its next render.
      void queryClient.cancelQueries()
      queryClient.clear()
    }
  }), [queryClient])

  return (
    <div className="min-h-screen bg-bg text-on-surface">
      <TopBar onOpenPalette={() => setOpen(true)} />
      <CommandPalette />
      <Sidebar />
      <main className="pt-12 pb-7 pl-[200px] min-h-screen">
        <div className="p-6 max-w-[1440px]">
          <div role="status" className="mb-space-lg rounded border border-surface-container-highest bg-surface-container px-space-lg py-space-md text-body-sm text-on-surface-variant">
            项目、构建、部署、审批、账号、审计与部署目标使用真实服务；其余业务模块尚未接入，页面显示未实现声明。
            {' '}构建与项目写操作受角色限制（admin/member 可写，viewer 只读）；部署、审批与部署目标仅管理员可用。
          </div>
          <Outlet />
        </div>
      </main>
      <StatusBar />
    </div>
  )
}
