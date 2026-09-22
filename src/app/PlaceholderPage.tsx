import { Icon } from '@/components/ui/Icon'

export function PlaceholderPage({ title, batch }: { title: string; batch: string }) {
  return (
    <div className="flex flex-col items-center justify-center py-32 gap-space-lg">
      <div className="w-12 h-12 rounded-lg bg-surface-container flex items-center justify-center">
        <Icon name="construction" size={28} className="text-outline" />
      </div>
      <div className="text-center">
        <h1 className="text-headline-lg font-semibold text-on-surface">{title}</h1>
        <p className="text-body-md text-outline mt-space-sm">
          此页面将在 {batch} 实现 · 路由已就绪
        </p>
      </div>
    </div>
  )
}
