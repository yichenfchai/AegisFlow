import { Icon } from '@/components/ui/Icon'

/**
 * 明确的未实现声明页：非主线业务模块不投入 mock 演示。
 * 项目、构建、部署、审批与账号主线为真实接口；本页对任何访问都恒定显示。
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
            当前已上线：真实账号鉴权与管理、项目注册表、构建闭环，以及部署目标、部署任务、部署审批与审查中心；本模块属于后续阶段。
          </p>
        </div>
      </div>
    </section>
  )
}
