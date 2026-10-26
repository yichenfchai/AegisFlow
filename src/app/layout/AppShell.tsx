import { Outlet } from 'react-router'
import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Sidebar } from './Sidebar'
import { TopBar } from './TopBar'
import { StatusBar } from './StatusBar'
import { useMockTelemetry } from '@/stores/telemetry'
import { CommandPalette, usePalette } from '@/app/CommandPalette'
import { useSession } from '@/stores/session'
import { businessMocksEnabled } from '@/api'

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
      {businessMocksEnabled && <DemoTelemetry />}
      <TopBar onOpenPalette={() => setOpen(true)} />
      <CommandPalette />
      <Sidebar />
      <main className="pt-12 pb-7 pl-[200px] min-h-screen">
        <div className="p-6 max-w-[1440px]">
          <div role="status" className="mb-space-lg rounded border border-surface-container-highest bg-surface-container px-space-lg py-space-md text-body-sm text-on-surface-variant">
            {businessMocksEnabled ? '业务演示仅用于未接入的演示页面；项目、构建与账号始终使用真实服务。' : '项目、构建、账号与审计使用真实服务；其余业务模块尚未接入，页面显示未实现声明。'}
            {' '}构建与项目写操作受角色限制（admin/member 可写，viewer 只读）。
          </div>
          <Outlet />
        </div>
      </main>
      <StatusBar />
    </div>
  )
}

function DemoTelemetry() {
  useMockTelemetry()
  return null
}
