import { useEffect, useId, useMemo, useState } from 'react'
import { useNavigate } from 'react-router'
import { create } from 'zustand'
import { Icon } from '@/components/ui/Icon'
import { useDialogFocus } from '@/components/ui/useDialogFocus'
import { useSession } from '@/stores/session'

interface PaletteState {
  open: boolean
  setOpen: (v: boolean) => void
}

export const usePalette = create<PaletteState>((set) => ({
  open: false,
  setOpen: (v) => set({ open: v }),
}))

interface PaletteResult {
  group: string
  label: string
  to: string
  /** 与 Sidebar 一致：管理员入口只对 admin 展示；路由守卫与 API 仍会再校验。 */
  adminOnly?: boolean
}

/** ⌘K 全局命令面板：跨实体搜索跳转 */
export function CommandPalette() {
  const { open, setOpen } = usePalette()
  const { user } = useSession()
  const [query, setQuery] = useState('')
  const [idx, setIdx] = useState(0)
  const nav = useNavigate()
  const titleId = useId()
  const close = () => setOpen(false)
  const dialogRef = useDialogFocus(open, close)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setOpen(!open)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, setOpen])

  useEffect(() => {
    if (open) {
      setQuery('')
      setIdx(0)
    }
  }, [open])

  const results = useMemo<PaletteResult[]>(
    () => [
      { group: '页面', label: '项目列表', to: '/' },
      { group: '页面', label: '构建流水线', to: '/builds' },
      { group: '页面', label: '账号安全 · 修改密码与管理会话', to: '/account' },
      { group: '页面', label: '用户与审计 · 管理员', to: '/settings', adminOnly: true },
      { group: '页面', label: '部署记录', to: '/deploys' },
      { group: '页面', label: '审批中心 · 管理员', to: '/approvals', adminOnly: true },
      { group: '页面', label: '部署目标 · 管理员', to: '/deploy-targets', adminOnly: true },
      { group: '页面', label: '审查中心', to: '/reviews' },
      { group: '页面', label: '安全扫描', to: '/scans' },
      { group: '页面', label: '知识库', to: '/knowledge' },
      { group: '页面', label: '密钥管理 · 管理员', to: '/keys', adminOnly: true },
    ],
    [],
  )

  const filtered = results.filter(
    (r) => (!r.adminOnly || user?.role === 'admin') && r.label.toLowerCase().includes(query.toLowerCase()),
  )

  if (!open) return null

  const go = (to: string) => {
    close()
    nav(to)
  }

  return (
    <div className="fixed inset-0 z-palette bg-black/60 flex items-start justify-center pt-[15vh]" onClick={close}>
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className="w-[560px] bg-surface-container rounded-lg border border-surface-container-highest overflow-hidden focus:outline-none"
        onClick={(e) => e.stopPropagation()}
      >
        <h2 id={titleId} className="sr-only">
          全局命令面板
        </h2>
        <div className="flex items-center gap-space-lg px-space-xl py-space-lg border-b border-surface-container-highest">
          <Icon name="search" size={18} className="text-outline" />
          <input
            value={query}
            onChange={(e) => {
              setQuery(e.target.value)
              setIdx(0)
            }}
            onKeyDown={(e) => {
              if (e.key === 'ArrowDown') setIdx((i) => Math.min(filtered.length - 1, i + 1))
              if (e.key === 'ArrowUp') setIdx((i) => Math.max(0, i - 1))
              if (e.key === 'Enter' && filtered[idx]) go(filtered[idx].to)
            }}
            aria-label="搜索页面"
            placeholder="搜索页面与构建..."
            className="flex-1 bg-transparent text-body-lg text-on-surface placeholder:text-text-3 focus:outline-none"
          />
          <span className="font-mono text-label-sm text-outline border border-outline-variant rounded px-1">ESC</span>
        </div>
        <div className="max-h-[380px] overflow-y-auto py-space-sm">
          {filtered.length === 0 && (
            <div className="px-space-xl py-8 text-center text-body-md text-outline">无匹配结果</div>
          )}
          {filtered.map((r, i) => (
            <button
              key={r.to + r.label}
              onClick={() => go(r.to)}
              onMouseEnter={() => setIdx(i)}
              className={`w-full flex items-center gap-space-lg px-space-xl py-space-md text-left transition-colors ${
                i === idx ? 'bg-surface-container-high' : ''
              }`}
            >
              <span className="text-label-sm bg-surface-container-highest text-outline rounded px-1.5 py-0.5 w-14 text-center shrink-0">
                {r.group}
              </span>
              <span className="text-body-md text-on-surface flex-1 truncate">{r.label}</span>
              {i === idx && <Icon name="keyboard_return" size={14} className="text-outline" />}
            </button>
          ))}
        </div>
        <div className="px-space-xl py-space-md border-t border-surface-container-highest text-label-sm text-outline flex gap-space-xl">
          <span>↑↓ 导航</span>
          <span>↳ 跳转</span>
          <span className="ml-auto font-mono">AegisFlow ⌘K</span>
        </div>
      </div>
    </div>
  )
}
