import { useMemo, useState } from 'react'
import { PageHeader, Button } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'

type LineType = 'add' | 'del' | 'ctx' | 'hunk'
interface DiffLine {
  type: LineType
  oldNo?: number
  newNo?: number
  content: string
  /** AI 锚点行 */
  anchor?: string
}
interface DiffFile {
  path: string
  lang: string
  lines: DiffLine[]
}

/** 模拟 diff 数据（RR-118 · fix-tls → main） */
const FILES: DiffFile[] = [
  {
    path: 'src/auth/client.ts',
    lang: 'TypeScript',
    lines: [
      { type: 'hunk', content: '@@ -8,11 +8,14 @@ export class AuthClient {' },
      { type: 'ctx', oldNo: 8, newNo: 8, content: 'export class AuthClient {' },
      { type: 'ctx', oldNo: 9, newNo: 9, content: '  private refreshUrl: string' },
      { type: 'del', oldNo: 10, content: '  refresh(payload: RefreshPayload) {' },
      { type: 'del', oldNo: 11, content: '    const kid = payload.claims[kid]' },
      { type: 'del', oldNo: 12, content: '    return this.post(refreshUrl, { kid })', anchor: 'client.ts:12' },
      { type: 'add', newNo: 10, content: '  refresh(payload: RefreshPayload) {' },
      { type: 'add', newNo: 11, content: '    // 空值防护：缺失 kid 头时显式失败' },
      { type: 'add', newNo: 12, content: '    if (!payload?.claims) throw new ClaimsMissingError(payload)' },
      { type: 'add', newNo: 13, content: '    const kid = payload.claims[kid]' },
      { type: 'add', newNo: 14, content: '    return this.post(refreshUrl, { kid })' },
      { type: 'ctx', oldNo: 13, newNo: 15, content: '  }' },
    ],
  },
  {
    path: 'src/auth/errors.ts',
    lang: 'TypeScript',
    lines: [
      { type: 'hunk', content: '@@ -1,4 +1,8 @@' },
      { type: 'ctx', oldNo: 1, newNo: 1, content: "export class AuthError extends Error {}" },
      { type: 'add', newNo: 2, content: 'export class ClaimsMissingError extends AuthError {' },
      { type: 'add', newNo: 3, content: '  constructor(payload: unknown) {' },
      { type: 'add', newNo: 4, content: "    super('JWT payload lacks kid claim')" },
      { type: 'add', newNo: 5, content: '  }' },
      { type: 'add', newNo: 6, content: '}' },
    ],
  },
  {
    path: '.env.example',
    lang: 'Dotenv',
    lines: [
      { type: 'hunk', content: '@@ -3,2 +3,2 @@' },
      { type: 'del', oldNo: 3, content: 'REFRESH_SECRET=dev-secret-123' },
      { type: 'add', newNo: 3, content: 'REFRESH_SECRET=' },
      { type: 'add', newNo: 4, content: '# 豁免说明：示例文件，已脱敏' },
    ],
  },
]

export function ReviewDetailPage() {
  const [adopted, setAdopted] = useState<Record<string, boolean>>({})

  const totals = useMemo(() => {
    let add = 0
    let del = 0
    for (const f of FILES)
      for (const l of f.lines) {
        if (l.type === 'add') add++
        if (l.type === 'del') del++
      }
    return { add, del }
  }, [])

  return (
    <>
      <PageHeader
        title="RR-118 · 代码一致性优化"
        subtitle="fix-tls → main · Alex Mercer · 12 分钟前 · 4/5 检查通过"
        actions={
          <>
            <Button variant="danger" onClick={() => alert('已驳回（演示）')}>
              驳回
            </Button>
            <Button icon="merge" onClick={() => alert('批准并合并 · 将触发构建（演示）')}>
              批准并合并
            </Button>
          </>
        }
      />

      {/* 元信息条 */}
      <div className="flex flex-wrap items-center gap-space-xl bg-surface-container rounded-lg border border-surface-container-highest p-space-lg mb-gutter text-body-sm">
        <StatusBadge tone="pending">待批准</StatusBadge>
        <span className="font-mono">
          <span className="text-secondary">+{totals.add}</span> <span className="text-error">−{totals.del}</span>
        </span>
        <span className="text-on-surface-variant">3 个文件 · 1 个 blocker 待处理</span>
        <span className="flex items-center gap-space-sm text-secondary">
          <Icon name="check_circle" size={14} />
          基线校验一致 · 基线未前移
        </span>
        <span className="flex items-center gap-space-sm text-warning">
          <Icon name="shield" size={14} />
          敏感数据 3 处（哈希脱敏）
        </span>
        <span className="flex items-center gap-space-sm text-outline ml-auto font-mono text-label-md">
          <Icon name="smart_toy" size={14} className="text-brand" />
          AI 分析耗时 6.8s · 14 次合并 · 0 次回滚
        </span>
      </div>

      {/* 变更意图 */}
      <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-lg mb-gutter">
        <div className="text-label-md text-outline mb-space-sm">变更意图（作者提供）</div>
        <p className="text-body-md text-on-surface-variant">
          refresh token 的 kid 头缺失时未做空值防护，补充 ClaimsMissingError 显式失败路径，与基线分支保持一致。
        </p>
      </div>

      {/* Diff */}
      <div className="space-y-gutter">
        {FILES.map((file) => (
          <div key={file.path} className="bg-surface-container rounded-lg border border-surface-container-highest overflow-hidden">
            <div className="flex items-center justify-between px-space-lg py-space-md bg-surface-container-low border-b border-surface-container-highest">
              <span className="font-mono text-body-sm">{file.path}</span>
              <span className="text-label-sm text-outline">{file.lang} · UTF-8 LF</span>
            </div>
            <div className="font-mono text-[12px] leading-[1.8]">
              {file.lines.map((l, i) => {
                if (l.type === 'hunk')
                  return (
                    <div key={i} className="px-space-lg py-0.5 bg-surface-container-low text-outline border-y border-surface-container-highest/50">
                      {l.content}
                    </div>
                  )
                const bg =
                  l.type === 'add'
                    ? 'bg-secondary/8'
                    : l.type === 'del'
                      ? 'bg-error/8'
                      : ''
                return (
                  <div key={i} className={`flex ${bg} ${l.anchor ? 'border-l-2 border-brand' : ''}`}>
                    <span className="select-none text-outline/50 w-10 text-right pr-2 shrink-0">{l.oldNo ?? ''}</span>
                    <span className="select-none text-outline/50 w-10 text-right pr-2 shrink-0">{l.newNo ?? ''}</span>
                    <span
                      className={`select-none w-4 shrink-0 ${l.type === 'add' ? 'text-secondary' : l.type === 'del' ? 'text-error' : 'text-transparent'}`}
                    >
                      {l.type === 'add' ? '+' : l.type === 'del' ? '−' : ' '}
                    </span>
                    <span className={`pr-space-lg ${l.type === 'del' ? 'text-error/80' : l.type === 'add' ? 'text-secondary/90' : 'text-on-surface-variant'}`}>
                      {l.content}
                      {l.anchor && (
                        <span className="ml-space-lg text-brand text-[11px]">· 锚点: {l.anchor}</span>
                      )}
                      {l.content.includes('dev-secret') && (
                        <span className="ml-space-md text-warning text-[11px]">· 哈希脱敏 3 处</span>
                      )}
                    </span>
                  </div>
                )
              })}
            </div>
          </div>
        ))}
      </div>

      {/* AI 修改建议 */}
      <div className="bg-surface-container rounded-lg border border-brand/30 p-space-xl mt-gutter">
        <div className="flex items-center gap-space-sm mb-space-md">
          <Icon name="smart_toy" size={16} className="text-brand" />
          <span className="text-headline-sm font-semibold">AI Blocker · 修改建议</span>
          <span className="text-label-sm bg-brand/12 text-brand rounded px-1">规则: DRY-104</span>
          <span className="ml-auto text-label-sm text-outline">证据链已固化</span>
        </div>
        <p className="text-body-md text-on-surface-variant mb-space-lg">
          <span className="font-mono text-[12px] text-error">client.ts:12</span> 的空值防护应同时覆盖{' '}
          <span className="font-mono text-[12px]">refresh_token</span> 的解码路径，建议抽出统一的 guard 函数避免三处重复判断（跨域复用）。
        </p>
        <div className="flex items-center gap-space-md">
          <Button size="sm" onClick={() => setAdopted((p) => ({ ...p, dry104: true }))} disabled={adopted.dry104}>
            {adopted.dry104 ? '已采纳' : '采纳建议'}
          </Button>
          <Button size="sm" variant="secondary" onClick={() => alert('已标记误报 · 将回流知识库')}>
            标记误报
          </Button>
          <span className="text-label-sm text-outline">👍 采纳后自动并入审查意见</span>
        </div>
      </div>
    </>
  )
}
