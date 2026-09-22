import { useState } from 'react'
import { useNavigate, useParams } from 'react-router'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/api'
import { PageHeader, Button, ConfirmDialog } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'
import { fmtDur, fmtRel } from '@/api/format'

/** 部署单详情（_8 稿）：镜像/编排/健康 + 单机守护参数 + 一键回滚 */
export function DeployDetailPage() {
  const { id } = useParams()
  const nav = useNavigate()
  const { data: deploys = [] } = useQuery({ queryKey: ['deploys'], queryFn: api.deploys })
  const deploy = deploys.find((d) => d.id === id || d.number === id) ?? deploys[0]
  const [confirmRollback, setConfirmRollback] = useState(false)
  const [rolling, setRolling] = useState(false)

  if (!deploy) return null
  const canRollback = deploy.status === 'success' || deploy.status === 'rolled_back'

  return (
    <>
      <PageHeader
        title={`部署单 ${deploy.number}`}
        subtitle={`${deploy.projectName} · 目标主机 ${deploy.targetName} · ${fmtRel(deploy.createdAt)}`}
        actions={
          <>
            <Button variant="secondary" icon="terminal" onClick={() => alert('在终端查看（演示）')}>
              查看生产日志
            </Button>
            {canRollback && (
              <Button variant="danger" icon="history" onClick={() => setConfirmRollback(true)} disabled={rolling}>
                {rolling ? '回滚下发中...' : '回滚到此版本'}
              </Button>
            )}
          </>
        }
      />

      <div className="grid grid-cols-[1fr_340px] gap-gutter">
        {/* 左列：部署信息 */}
        <div className="space-y-gutter">
          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg">
            <div className="flex items-center justify-between">
              <h3 className="text-headline-md font-semibold">部署配置信息</h3>
              <StatusBadge
                tone={
                  deploy.status === 'success'
                    ? 'success'
                    : deploy.status === 'failed'
                      ? 'error'
                      : deploy.status === 'rolled_back'
                        ? 'pending'
                        : 'running'
                }
              >
                {deploy.status === 'success'
                  ? '部署成功'
                  : deploy.status === 'failed'
                    ? '部署失败'
                    : deploy.status === 'rolled_back'
                      ? '已回滚'
                      : deploy.status === 'running'
                        ? '部署进行中'
                        : '待执行'}
              </StatusBadge>
            </div>
            <div className="grid grid-cols-2 gap-space-xl">
              <Row label="镜像标签" value={<span className="font-mono">{deploy.imageTag}</span>} />
              <Row
                label="关联构建"
                value={
                  <button className="font-mono text-primary hover:underline" onClick={() => nav(`/builds/${deploy.buildNumber}`)}>
                    构建 #{deploy.buildNumber}
                  </button>
                }
              />
              <Row label="编排文件" value={<span className="font-mono">docker-compose.prod.yml</span>} />
              <Row label="网络模式" value="bridge" mono />
              <Row label="工作目录" value={<span className="font-mono">/srv/web-api</span>} />
              <Row label="部署耗时" value={deploy.durationMs ? fmtDur(deploy.durationMs) : '—'} mono />
            </div>
          </div>

          {/* 健康检查 */}
          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
            <h3 className="text-headline-md font-semibold mb-space-lg">健康检查</h3>
            <div className="flex items-center gap-space-xl">
              {[1, 2, 3, 4, 5].map((n) => {
                const pass =
                  deploy.status === 'success' ||
                  (deploy.status === 'running' && n <= 3) ||
                  (deploy.status === 'rolled_back' && false)
                const pending = deploy.status === 'running' && n > 3
                return (
                  <div key={n} className="flex flex-col items-center gap-space-sm">
                    <span
                      className={`w-8 h-8 rounded-full flex items-center justify-center border ${
                        pass
                          ? 'border-secondary text-secondary'
                          : pending
                            ? 'border-primary text-primary gp-breathe'
                            : 'border-error text-error'
                      }`}
                    >
                      <Icon name={pass ? 'check' : pending ? 'more_horiz' : 'close'} size={16} />
                    </span>
                    <span className="text-label-sm text-outline">HC-{n}</span>
                  </div>
                )
              })}
              <div className="ml-space-xl">
                <div className={`text-body-md ${deploy.status === 'rolled_back' ? 'text-error' : deploy.status === 'running' ? 'text-primary' : 'text-secondary'}`}>
                  {deploy.status === 'success'
                    ? '5/5 全部通过 · 容器启动就绪'
                    : deploy.status === 'running'
                      ? deploy.healthNote ?? '健康探测中...'
                      : deploy.status === 'rolled_back'
                        ? '5/5 超时 · 已触发回滚基线'
                        : deploy.healthNote ?? '健康检查未通过'}
                </div>
                <div className="text-label-sm text-outline mt-0.5">健康检查地址: http://10.0.0.8:8080/healthz</div>
              </div>
            </div>
          </div>

          {/* 单机部署流记录 */}
          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
            <h3 className="text-headline-md font-semibold mb-space-lg">单机部署流记录</h3>
            <div className="space-y-0">
              {[
                ['构建产物拉取', `镜像 ${deploy.imageTag} 推送至目标主机 · 节点镜像缓存命中`],
                ['编排下发', 'docker-compose.prod.yml · 网络模式 bridge'],
                ['容器启动', '容器启动就绪 · 退出码 0'],
                ['健康探测', deploy.status === 'success' ? '5/5 全部通过 · 平均 412ms' : deploy.healthNote ?? '—'],
                ['流量切换', deploy.status === 'success' ? '旧容器已下线 · 保留版本上限内' : deploy.status === 'rolled_back' ? '已回切至历史 tag' : '待完成'],
              ].map(([t, d], i, arr) => (
                <div key={t} className="flex gap-space-lg">
                  <div className="flex flex-col items-center">
                    <span
                      className={`w-2 h-2 rounded-full mt-1.5 ${
                        deploy.status === 'failed' && i >= 3 ? 'bg-error' : deploy.status === 'rolled_back' && i === 4 ? 'bg-warning' : 'bg-secondary'
                      }`}
                    />
                    {i < arr.length - 1 && <span className="w-px flex-1 bg-surface-container-highest my-1" />}
                  </div>
                  <div className="pb-space-lg">
                    <div className="text-body-md">{t}</div>
                    <div className="text-label-sm text-outline mt-0.5 font-mono">{d}</div>
                  </div>
                </div>
              ))}
            </div>
          </div>
        </div>

        {/* 右列：回滚与守护参数 */}
        <div className="space-y-gutter">
          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
            <h3 className="text-headline-md font-semibold mb-space-md">本地版本历史</h3>
            <p className="text-label-sm text-outline mb-space-md">5 个本地历史 tag · 保留版本上限 5</p>
            <div className="space-y-space-md">
              {[
                ['web-api:1092', '进行中', 'running'],
                ['web-api:1091', '当前运行', 'current'],
                ['web-api:1090', '可回滚', 'ok'],
                ['web-api:1089', '回滚基线', 'ok'],
                ['web-api:1088', '可回滚', 'ok'],
              ].map(([tag, label, kind]) => (
                <div key={tag} className="flex items-center justify-between bg-surface-container-low rounded px-space-lg py-space-md">
                  <span className="font-mono text-body-sm">{tag}</span>
                  {kind === 'running' ? (
                    <span className="text-label-sm text-primary gp-breathe">{label}</span>
                  ) : kind === 'current' ? (
                    <span className="text-label-sm text-secondary">{label}</span>
                  ) : (
                    <button
                      className="text-label-sm text-primary hover:underline"
                      onClick={() => alert(`下发回滚任务至 ${tag}（演示）`)}
                    >
                      回滚到此版本
                    </button>
                  )}
                </div>
              ))}
            </div>
          </div>

          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg">
            <h3 className="text-headline-md font-semibold">单机守护参数</h3>
            <Row label="连接目标主机" value={<span className="font-mono">{deploy.targetName} (10.0.0.8)</span>} />
            <Row label="主机直连" value={<span className="text-secondary flex items-center gap-space-sm"><span className="w-1.5 h-1.5 rounded-full bg-secondary" />SSH Agent 就绪</span>} />
            <Row label="保留版本上限" value="5" mono />
            <Row label="健康失败策略" value="健康失败即刻回退" />
            <Row label="重试预算" value="-1 (不限)" mono />
          </div>

          {deploy.status === 'rolled_back' && (
            <div className="rounded-lg border border-warning/40 p-space-lg text-body-sm text-warning flex items-start gap-space-md">
              <Icon name="warning" size={16} className="shrink-0 mt-0.5" />
              本部署因健康检查超时已回滚至 web-api:1088 · 回滚动作已记入审计日志与证据链
            </div>
          )}
        </div>
      </div>

      <ConfirmDialog
        open={confirmRollback}
        danger
        title="确认立即回滚"
        confirmText="确认回滚"
        onConfirm={() => {
          setRolling(true)
          setConfirmRollback(false)
          setTimeout(() => {
            setRolling(false)
            alert(`已成功向目标主机 ${deploy.targetName} (10.0.0.8) 下发回滚指令 · 重新拉起历史镜像（演示）`)
          }, 800)
        }}
        onCancel={() => setConfirmRollback(false)}
      >
        您将直接在目标主机 <span className="font-mono">{deploy.targetName}</span> 上回滚{' '}
        <span className="font-mono">{deploy.imageTag}</span> →{' '}
        <span className="font-mono">web-api:1089</span>。回滚动作走 LOA 4 审批关口，将记入审计日志。
      </ConfirmDialog>
    </>
  )
}

function Row({ label, value, mono }: { label: string; value: React.ReactNode; mono?: boolean }) {
  return (
    <div className="flex flex-col gap-0.5">
      <span className="text-label-sm text-outline">{label}</span>
      <span className={`text-body-md text-on-surface ${mono ? 'font-mono' : ''}`}>{value}</span>
    </div>
  )
}
