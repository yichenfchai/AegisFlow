/** 底部状态条：陈述真实服务范围；不展示模拟遥测数据。 */
export function StatusBar() {
  return (
    <footer className="fixed bottom-0 left-0 right-0 h-7 z-50 bg-surface border-t border-surface-container-highest flex items-center px-space-lg text-label-sm text-outline">
      <span>项目 · 构建 · 部署 · 审批 · 账号使用真实服务</span>
      <span className="flex-1 text-center font-mono">AegisFlow</span>
      <span className="font-mono">其余模块未接入</span>
    </footer>
  )
}
