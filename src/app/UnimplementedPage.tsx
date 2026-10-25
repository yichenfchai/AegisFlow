import { Icon } from '@/components/ui/Icon'

/**
 * 明确的未实现声明页：P2 之前不再为非主线业务模块投入 mock 演示。
 * 项目/构建主线页面保留在 BusinessGate 后面；本页对任何访问都恒定显示。
 */
export function UnimplementedPage({ title }: { title: string }) {
  return (
    <section className="rounded-lg border border-surface-container-highest bg-surface-container p-space-xl" aria-labelledby="unimplemented-title">
      <div className="flex items-start gap-space-lg">
        <div className="w-11 h-11 rounded-lg bg-surface-container-high flex items-center justify-center shrink-0">
          <Icon name="construction" size={24} className="text-outline" />
        </div>
        <div>
          <h1 id="unimplemented-title" className="text-headline-md font-semibold">
            {title}
          </h1>
          <p className="mt-space-md text-body-md text-on-surface-variant">
            此模块的真实业务接口尚未实现，当前不提供演示数据或操作按钮。
          </p>
          <p className="mt-space-md text-body-sm text-outline">
            当前进度：真实账号鉴权与管理已完成（P1），项目与构建链路见开发 TODO 的 P2/P3 里程碑。
          </p>
        </div>
      </div>
    </section>
  )
}
