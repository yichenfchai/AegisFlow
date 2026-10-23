import type { ReactNode } from 'react'
import { businessMocksEnabled } from '@/api'

/** Do not render static demo controls as working business features in real mode. */
export function BusinessGate({ children }: { children: ReactNode }) {
  if (businessMocksEnabled) return children
  return (
    <section className="rounded border border-surface-container-highest p-space-xl">
      <h1 className="text-headline-md">账号鉴权已接入</h1>
      <p className="mt-space-md text-on-surface-variant">当前业务模块尚未开放真实接口，不展示演示数据或可产生误解的操作按钮。</p>
      <p className="mt-space-md text-body-sm text-outline">登录、会话恢复、自动刷新及右上角退出均使用真实后端。业务开发进度见项目 TODO。</p>
    </section>
  )
}
