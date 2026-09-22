import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate } from 'react-router'
import { api } from '@/api'
import { StatCard, StatusBadge, LoaBadge } from '@/components/ui/Badge'
import { Button, PageHeader } from '@/components/ui/DataTable'
import { Icon } from '@/components/ui/Icon'
import { fmtRel } from '@/api/format'

/** 工作台（_6 稿）：项目卡片 + 待办 + 守护状态 */
export function DashboardPage() {
  const nav = useNavigate()
  const { data: projects = [] } = useQuery({ queryKey: ['projects'], queryFn: api.projects })
  const { data: reviews = [] } = useQuery({ queryKey: ['reviews'], queryFn: api.reviews })
  const { data: approvals = [] } = useQuery({ queryKey: ['approvals'], queryFn: api.approvals })
  const { data: builds = [] } = useQuery({ queryKey: ['builds'], queryFn: api.builds })

  const pendingReviews = reviews.filter((r) => r.status === 'pending' || r.status === 'awaiting_approval').length
  const pendingApprovals = approvals.filter((a) => a.status === 'pending').length
  const alertProjects = projects.filter((p) => p.healthStatus.includes('预警')).length

  return (
    <>
      <PageHeader
        title="工作台"
        subtitle="全部项目健康总览 · 活动分支 main"
        actions={
          <Link to="/projects/new">
            <Button icon="add">关联本地项目</Button>
          </Link>
        }
      />

      <div className="grid grid-cols-4 gap-gutter mb-space-xl">
        <StatCard label="项目" value={projects.length} unit="个" sub="全部 HEAD 对齐" />
        <StatCard label="待审查" value={pendingReviews} accent="warning" sub="审查中心" />
        <StatCard label="待批准" value={pendingApprovals} accent="warning" sub="审批中心 · LOA 关口" />
        <StatCard label="预警" value={alertProjects} accent={alertProjects ? 'error' : 'success'} sub="TLS 证书 / 健康检查" />
      </div>

      {/* 项目卡片 */}
      <div className="grid grid-cols-2 gap-gutter mb-space-xl">
        {projects.map((p) => (
          <div key={p.id} className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
            <div className="flex items-center justify-between mb-space-md">
              <div className="flex items-center gap-space-md">
                <Icon name="deployed_code" size={18} className="text-on-surface-variant" />
                <span className="text-headline-sm font-semibold">{p.name}</span>
                <span className="font-mono text-label-sm text-outline">{p.branch}</span>
              </div>
              <StatusBadge
                tone={p.healthStatus.includes('预警') ? 'pending' : 'success'}
              >
                {p.healthStatus}
              </StatusBadge>
            </div>
            <div className="grid grid-cols-3 gap-space-md text-body-sm text-on-surface-variant mb-space-lg">
              <div>
                <div className="text-label-sm text-outline">CPU / 内存</div>
                <span className="font-mono">{p.cpu} · {p.memory}</span>
              </div>
              <div>
                <div className="text-label-sm text-outline">路径</div>
                <span className="font-mono truncate block">{p.localPath}</span>
              </div>
              <div>
                <div className="text-label-sm text-outline">自治档位</div>
                <LoaBadge loa={p.loaDefault} />
              </div>
            </div>
            <div className="flex items-center gap-space-md pt-space-md border-t border-surface-container-highest">
              <Button size="sm" variant="secondary" icon="play_arrow" onClick={() => nav('/builds')}>
                触发构建
              </Button>
              <Button size="sm" variant="ghost" onClick={() => nav('/reviews/new')}>
                进入审查 ({pendingReviews})
              </Button>
              {alertProjects > 0 && (
                <button className="text-label-md text-warning flex items-center gap-space-sm hover:underline">
                  <Icon name="warning" size={14} />
                  预警 (1) · 解决
                </button>
              )}
              <span className="ml-auto text-label-sm text-outline flex items-center gap-space-sm">
                <span className={`w-1.5 h-1.5 rounded-full ${p.headAligned ? 'bg-secondary' : 'bg-warning'}`} />
                {p.headAligned ? 'HEAD 对齐' : '待拉取同步'}
              </span>
            </div>
          </div>
        ))}
      </div>

      {/* 最近构建 */}
      <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
        <div className="flex items-center justify-between mb-space-lg">
          <h3 className="text-headline-sm font-semibold">最近构建</h3>
          <Link to="/builds" className="text-label-md text-primary hover:underline">
            查看全部 →
          </Link>
        </div>
        <div className="space-y-space-md">
          {builds.slice(0, 4).map((b) => (
            <Link
              key={b.id}
              to={`/builds/${b.number}`}
              className="flex items-center gap-space-lg py-space-md border-b border-surface-container-highest/50 last:border-0 hover:bg-surface-container-low rounded px-space-sm -mx-space-sm transition-colors"
            >
              <span className="font-mono text-primary w-14">#{b.number}</span>
              <span className="text-body-md flex-1">{b.projectName}</span>
              <span className="font-mono text-label-sm text-outline">{b.branch}</span>
              <StatusBadge tone={b.status === 'success' ? 'success' : b.status === 'failed' ? 'error' : 'running'}>
                {b.status === 'success' ? '成功' : b.status === 'failed' ? '失败' : '运行中'}
              </StatusBadge>
              <span className="text-label-sm text-text-3 w-20 text-right">{fmtRel(b.createdAt)}</span>
            </Link>
          ))}
        </div>
      </div>
    </>
  )
}
