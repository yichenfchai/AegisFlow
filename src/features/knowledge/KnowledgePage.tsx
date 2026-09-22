import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/api'
import { PageHeader, Button } from '@/components/ui/DataTable'
import { StatCard, Segmented } from '@/components/ui/Badge'
import { StatusBadge } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'
import type { KnowledgeEntry } from '@/api/types'

const DOMAIN_TEXT: Record<KnowledgeEntry['domain'], string> = {
  build: '构建',
  review: '审查',
  deploy: '部署',
  defense: '防御',
  config: '配置',
}

const STATUS_TONE: Record<KnowledgeEntry['status'], 'success' | 'running' | 'pending' | 'neutral'> = {
  promoted: 'success',
  verified: 'success',
  pending_verify: 'pending',
  candidate: 'running',
  stale: 'neutral',
  false_positive: 'neutral',
}
const STATUS_TEXT: Record<KnowledgeEntry['status'], string> = {
  promoted: '规则',
  verified: '已验证',
  pending_verify: '待验证',
  candidate: '候选',
  stale: '已失效',
  false_positive: '误报',
}

type Sort = 'hit' | 'recent'

export function KnowledgePage() {
  const { data: entries = [] } = useQuery({ queryKey: ['knowledge'], queryFn: api.knowledge })
  const [domain, setDomain] = useState<'all' | KnowledgeEntry['domain']>('all')
  const [sort, setSort] = useState<Sort>('hit')
  const [drawerOpen, setDrawerOpen] = useState(false)

  const filtered = entries
    .filter((e) => domain === 'all' || e.domain === domain)
    .sort((a, b) => (sort === 'hit' ? b.hitCount - a.hitCount : b.updatedAt - a.updatedAt))

  const totalHits = entries.reduce((a, e) => a + e.hitCount, 0)
  const staleCount = entries.filter((e) => e.status === 'stale' || e.status === 'false_positive').length
  const savedTotal = entries.reduce((a, e) => a + e.modelSaved, 0)
  const domains = new Set(entries.map((e) => e.domain)).size

  return (
    <>
      <PageHeader
        title="知识库"
        subtitle="人训 AI 修复规则与自愈条目 · 覆盖 4 个域"
        actions={
          <>
            <Button variant="secondary" size="md" icon="download">
              导出规则
            </Button>
            <Button icon="add" onClick={() => setDrawerOpen(true)}>
              新建知识条目
            </Button>
          </>
        }
      />

      <div className="grid grid-cols-4 gap-gutter mb-space-xl">
        <StatCard label="本周命中" value={totalHits + ' 次'} trend="33%" trendDir="up" sub="全部条目" />
        <StatCard label="免调模型节省" value={savedTotal + ' 次'} accent="ai" sub="AI 信号 · ~28.9% 算力节约" />
        <StatCard label="废弃 / 失效" value={staleCount + ' 条'} sub="按命中率排序已剔除" />
        <StatCard label="覆盖域" value={domains + ' 个'} sub="build · review · deploy · defense" />
      </div>

      <div className="flex items-center justify-between mb-space-lg">
        <Segmented
          value={domain}
          onChange={setDomain}
          options={[
            { value: 'all' as const, label: '全部' },
            { value: 'build' as const, label: '构建' },
            { value: 'review' as const, label: '审查' },
            { value: 'deploy' as const, label: '部署' },
            { value: 'defense' as const, label: '防御' },
          ]}
        />
        <Segmented
          value={sort}
          onChange={setSort}
          options={[
            { value: 'hit', label: '按命中率' },
            { value: 'recent', label: '最近更新' },
          ]}
        />
      </div>

      <div className="bg-surface-container rounded-lg border border-surface-container-highest overflow-hidden">
        <table className="w-full text-left">
          <thead>
            <tr className="text-label-md text-outline border-b border-surface-container-highest">
              <th className="px-space-xl py-space-md font-normal">条目 ID</th>
              <th className="px-space-xl py-space-md font-normal">所属领域</th>
              <th className="px-space-xl py-space-md font-normal">诊断与错误特征</th>
              <th className="px-space-xl py-space-md font-normal">验证状态</th>
              <th className="px-space-xl py-space-md font-normal">命中 / 节省</th>
              <th className="px-space-xl py-space-md font-normal">最后命中</th>
              <th className="px-space-xl py-space-md font-normal text-right">操作</th>
            </tr>
          </thead>
          <tbody>
            {filtered.map((e) => (
              <tr key={e.id} className="border-b border-surface-container-highest/50 last:border-0 hover:bg-surface-container-low transition-colors">
                <td className="px-space-xl py-space-md">
                  <span className="flex items-center gap-space-sm">
                    {e.status === 'promoted' && (
                      <span className="text-label-sm bg-brand/12 text-brand rounded px-1">AI 智囊</span>
                    )}
                    <span className="font-mono text-primary">{e.id}</span>
                  </span>
                </td>
                <td className="px-space-xl py-space-md text-on-surface-variant">
                  {DOMAIN_TEXT[e.domain]} / {domainTag(e.domain)}
                </td>
                <td className="px-space-xl py-space-md max-w-[320px]">
                  <div className="text-body-md text-on-surface truncate">{e.title}</div>
                  <div className="text-label-sm text-outline font-mono truncate">{e.errorPattern}</div>
                </td>
                <td className="px-space-xl py-space-md">
                  <StatusBadge tone={STATUS_TONE[e.status]}>
                    {STATUS_TEXT[e.status]} ({e.verifyCount}/{e.minVerifications})
                  </StatusBadge>
                </td>
                <td className="px-space-xl py-space-md font-mono text-body-sm">
                  {e.hitCount} / <span className="text-brand">省 {e.modelSaved}</span>
                </td>
                <td className="px-space-xl py-space-md text-on-surface-variant">{fmtDay(e.updatedAt)}</td>
                <td className="px-space-xl py-space-md">
                  <div className="flex justify-end gap-space-md">
                    <button className="text-body-sm text-on-surface-variant hover:text-primary">编辑规则</button>
                    <button className="text-body-sm text-on-surface-variant hover:text-error">标记失效</button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {drawerOpen && <KbEditorDrawer onClose={() => setDrawerOpen(false)} />}
    </>
  )
}

function domainTag(d: KnowledgeEntry['domain']) {
  return { build: 'TypeScript', review: '安全', deploy: 'K8s', defense: 'WAF', config: '通用' }[d]
}

function fmtDay(ts: number) {
  const diff = Date.now() - ts
  if (diff < 86400000) return '今天'
  return `${Math.floor(diff / 86400000)} 天前`
}

/** 新建知识条目抽屉（_17_kb_editor 稿） */
function KbEditorDrawer({ onClose }: { onClose: () => void }) {
  const [domain, setDomain] = useState<KnowledgeEntry['domain']>('build')
  const [verify, setVerify] = useState(3)
  return (
    <div className="fixed inset-0 z-overlay bg-black/60" onClick={onClose}>
      <div
        className="absolute right-0 top-0 bottom-0 w-[480px] bg-surface-container border-l border-surface-container-highest flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="p-space-xl border-b border-surface-container-highest">
          <div className="flex items-center justify-between">
            <h2 className="text-headline-md font-semibold">新建知识条目</h2>
            <button onClick={onClose} className="text-outline hover:text-on-surface">
              <Icon name="close" size={20} />
            </button>
          </div>
          <p className="text-label-sm text-outline mt-space-sm">
            来源：手动 · 创建后将进入候选状态，验证 ≥3 次后自动升级
          </p>
        </div>

        <div className="flex-1 overflow-y-auto p-space-xl space-y-space-lg">
          <div>
            <label className="block text-label-md text-on-surface-variant mb-space-sm">所属域</label>
            <div className="flex gap-1 bg-surface-container-lowest rounded p-0.5 border border-surface-container-highest">
              {(['build', 'review', 'deploy', 'defense', 'config'] as const).map((d) => (
                <button
                  key={d}
                  onClick={() => setDomain(d)}
                  className={`flex-1 py-1 rounded text-body-sm ${
                    domain === d ? 'bg-primary text-on-primary font-medium' : 'text-on-surface-variant'
                  }`}
                >
                  {DOMAIN_TEXT[d]}
                </button>
              ))}
            </div>
          </div>

          <div>
            <label className="block text-label-md text-on-surface-variant mb-space-sm">标题</label>
            <input
              defaultValue="TokenParser 索引越界修复"
              className="w-full bg-surface-container-lowest border border-surface-container-highest rounded px-space-lg py-space-md text-body-md focus:outline-none focus:border-primary/60"
            />
          </div>

          <div>
            <label className="block text-label-md text-on-surface-variant mb-space-sm">
              错误指纹 <span className="float-right font-mono text-label-sm text-outline">SIG_NORMALIZE_V2</span>
            </label>
            <textarea
              rows={2}
              defaultValue="TypeError: Cannot read properties of undefined (reading 'claims') @ parser/token-parser.ts"
              className="w-full bg-surface-container-lowest border border-surface-container-highest rounded px-space-lg py-space-md font-mono text-[12px] leading-relaxed focus:outline-none focus:border-primary/60 resize-none"
            />
            <p className="text-label-sm text-outline mt-space-xs">归一化匹配：忽略数字、路径与变量名</p>
          </div>

          <div>
            <label className="block text-label-md text-on-surface-variant mb-space-sm">根因诊断</label>
            <textarea
              rows={2}
              defaultValue="JWT claims 切片在缺失 kid 头时索引越界，未做空值防护"
              className="w-full bg-surface-container-lowest border border-surface-container-highest rounded px-space-lg py-space-md text-body-md focus:outline-none focus:border-primary/60 resize-none"
            />
          </div>

          <div>
            <label className="block text-label-md text-on-surface-variant mb-space-sm">
              修复动作 <span className="float-right font-mono text-label-sm text-outline">TS</span>
            </label>
            <div className="bg-surface-container-lowest border border-surface-container-highest rounded overflow-hidden">
              <pre className="p-space-lg font-mono text-[12px] leading-relaxed">
                <span className="text-outline select-none mr-3">1</span>
                <span className="text-primary">if</span> (!payload?.claims){'\n'}
                <span className="text-outline select-none mr-3">2</span>
                {'  '}<span className="text-error">throw new</span>{' '}
                <span className="text-brand">ClaimsMissingError</span>(
                <span className="text-secondary">'JWT payload lacks kid claim'</span>);{'\n'}
                <span className="text-outline select-none mr-3">3</span>
                {'}'}
              </pre>
            </div>
          </div>

          <div className="flex items-center justify-between">
            <div>
              <label className="block text-label-md text-on-surface-variant">最小验证次数</label>
              <p className="text-label-sm text-outline mt-0.5">达到阈值后规则转入自动补丁策略池</p>
            </div>
            <div className="inline-flex items-center border border-surface-container-highest rounded overflow-hidden">
              <button onClick={() => setVerify(Math.max(1, verify - 1))} className="px-space-lg py-space-sm text-on-surface-variant hover:bg-surface-container">
                <Icon name="remove" size={14} />
              </button>
              <span className="font-mono text-body-md px-space-lg min-w-10 text-center">{verify}</span>
              <button onClick={() => setVerify(verify + 1)} className="px-space-lg py-space-sm text-on-surface-variant hover:bg-surface-container">
                <Icon name="add" size={14} />
              </button>
            </div>
          </div>

          <div className="bg-surface-container-low rounded p-space-lg flex items-center gap-space-md">
            <Icon name="link" size={16} className="text-outline" />
            <span className="text-label-md text-on-surface-variant">关联来源：</span>
            <button className="text-body-sm text-primary hover:underline">构建 #1091</button>
            <span className="text-outline">·</span>
            <button className="text-body-sm text-primary hover:underline">查看日志锚点 L241</button>
            <span className="ml-auto font-mono text-label-sm text-outline border border-surface-container-highest rounded px-1">
              READONLY
            </span>
          </div>
        </div>

        <div className="p-space-xl border-t border-surface-container-highest flex justify-end gap-space-md">
          <Button variant="secondary" onClick={onClose}>
            取消
          </Button>
          <Button onClick={() => alert('已创建（演示）')}>创建条目</Button>
        </div>
      </div>
    </div>
  )
}
