import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router'
import { create } from 'zustand'
import { Icon } from '@/components/ui/Icon'

interface PaletteState {
  open: boolean
  setOpen: (v: boolean) => void
}

export const usePalette = create<PaletteState>((set) => ({
  open: false,
  setOpen: (v) => set({ open: v }),
}))

/** ⌘K 全局命令面板：跨实体搜索跳转 */
export function CommandPalette() {
  const { open, setOpen } = usePalette()
  const [query, setQuery] = useState('')
  const [idx, setIdx] = useState(0)
  const nav = useNavigate()
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setOpen(!open)
      }
      if (e.key === 'Escape') setOpen(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, setOpen])

  useEffect(() => {
    if (open) {
      setQuery('')
      setIdx(0)
      setTimeout(() => inputRef.current?.focus(), 30)
    }
  }, [open])

  const RESULTS = useMemo(
    () => [
      { group: '构建', label: '#1092 · 运行中 · web-api', to: '/builds/1092' },
      { group: '构建', label: '#1091 · 失败 · AI 诊断', to: '/builds/1091/diagnosis' },
      { group: '审查', label: 'RR-118 · 待批准 · 代码一致性优化', to: '/reviews/rr-118' },
      { group: '部署', label: 'DP-312 · 进行中 · prod-vps-01', to: '/deploys' },
      { group: '知识库', label: 'kb-082 · TokenParser 索引越界修复', to: '/knowledge' },
      { group: '防御', label: '45.132.28.117 · AS49505 · 封禁 24h', to: '/defense' },
      { group: '审计', label: '自动封禁 · LOA 7 · 近 24h', to: '/audit' },
      { group: '页面', label: '自动化策略 · LOA 档位选择器', to: '/policies' },
      { group: '页面', label: '密钥管理', to: '/secrets' },
      { group: '页面', label: '设置 · AI Provider', to: '/settings' },
    ],
    [],
  )

  const filtered = RESULTS.filter((r) => r.label.toLowerCase().includes(query.toLowerCase()))

  if (!open) return null

  const go = (to: string) => {
    setOpen(false)
    nav(to)
  }

  return (
    <div className="fixed inset-0 z-palette bg-black/60 flex items-start justify-center pt-[15vh]" onClick={() => setOpen(false)}>
      <div
        className="w-[560px] bg-surface-container rounded-lg border border-surface-container-highest overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-space-lg px-space-xl py-space-lg border-b border-surface-container-highest">
          <Icon name="search" size={18} className="text-outline" />
          <input
            ref={inputRef}
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
            placeholder="搜索构建号、RR、IP、知识条目..."
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
          <span className="ml-auto font-mono">GoPulse ⌘K</span>
        </div>
      </div>
    </div>
  )
}
