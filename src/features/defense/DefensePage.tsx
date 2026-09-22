import { useQuery } from '@tanstack/react-query'
import { api } from '@/api'
import { PageHeader, Button } from '@/components/ui/DataTable'
import { StatCard, LoaBadge } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'
import { fmtRel } from '@/api/format'

/** 攻击防御（iptables 稿）：实时拦截流水 + 黑名单 + 经验卡 */
export function DefensePage() {
  // refetchInterval 模拟 1s 自动刷新（真实实现走 SSE）
  const { data } = useQuery({
    queryKey: ['defense'],
    queryFn: api.defenseEvents,
    refetchInterval: 1000,
  })
  const items = data?.items ?? []
  const sum = data?.summary

  return (
    <>
      <PageHeader
        title="攻击防御"
        subtitle="单机边缘防护与自愈拦截 · LOA 7 全自动"
        actions={
          <>
            <span className="flex items-center gap-space-sm text-label-md text-outline font-mono">
              <span className="w-1.5 h-1.5 rounded-full bg-secondary gp-breathe" />
              自动刷新: 1s
            </span>
            <Button variant="secondary" icon="gpp_bad" onClick={() => alert('查看全部流水 1,428 条（演示）')}>
              全部流水 (1,428)
            </Button>
          </>
        }
      />

      <div className="grid grid-cols-4 gap-gutter mb-space-xl">
        <StatCard label="今日拦截总计" value={sum?.todayTotal ?? '—'} accent="error" sub="相比昨日下降 18.5 小时/周" />
        <StatCard label="带宽抑制" value={sum?.bandwidthSaved ?? '—'} accent="success" sub="99.4% 带宽节省" />
        <StatCard label="活跃黑名单" value={sum?.activeBans ?? '—'} sub="IP 动态冷却 60m" />
        <StatCard label="经验卡沉淀" value={sum?.kbCards ?? '—'} accent="ai" sub="自动沉淀中 · 已验证 8 次" />
      </div>

      <div className="grid grid-cols-[1fr_300px] gap-gutter">
        {/* 实时拦截流水 */}
        <div className="bg-surface-container rounded-lg border border-surface-container-highest overflow-hidden">
          <div className="flex items-center justify-between px-space-lg py-space-md bg-surface-container-low border-b border-surface-container-highest">
            <span className="text-headline-sm font-semibold flex items-center gap-space-sm">
              <Icon name="radar" size={16} className="text-error" />
              实时拦截流水
            </span>
            <span className="font-mono text-label-sm text-outline">吞吐: 12.4 req/s · 受限受控</span>
          </div>
          <table className="w-full text-left">
            <thead>
              <tr className="text-label-md text-outline border-b border-surface-container-highest">
                <th className="px-space-lg py-space-md font-normal">时间戳</th>
                <th className="px-space-lg py-space-md font-normal">源 IP / AS</th>
                <th className="px-space-lg py-space-md font-normal">命中规则</th>
                <th className="px-space-lg py-space-md font-normal">频率窗口</th>
                <th className="px-space-lg py-space-md font-normal">处置动作</th>
                <th className="px-space-lg py-space-md font-normal">状态</th>
              </tr>
            </thead>
            <tbody>
              {items.map((e) => (
                <tr key={e.id} className="border-b border-surface-container-highest/50 last:border-0 hover:bg-surface-container-low">
                  <td className="px-space-lg py-space-md font-mono text-label-md text-text-3">{fmtRel(e.ts)}</td>
                  <td className="px-space-lg py-space-md font-mono text-body-sm">
                    {e.sourceIp}
                    <span className="text-outline ml-1">{e.asn}</span>
                  </td>
                  <td className="px-space-lg py-space-md text-body-sm">{e.ruleId}</td>
                  <td className="px-space-lg py-space-md font-mono text-label-md text-warning">{e.rateWindow}</td>
                  <td className="px-space-lg py-space-md">
                    <span
                      className={`text-body-sm ${e.action.includes('封禁') ? 'text-error' : 'text-warning'}`}
                    >
                      {e.action}
                    </span>
                  </td>
                  <td className="px-space-lg py-space-md">
                    <span
                      className={`text-label-md ${
                        e.status === 'active' ? 'text-secondary' : 'text-outline'
                      }`}
                    >
                      {e.status === 'active' ? '生效中' : '已过期'}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        {/* 右列：策略 + 经验卡 */}
        <div className="space-y-gutter">
          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
            <h3 className="text-headline-sm font-semibold mb-space-md">活跃防御策略</h3>
            <div className="space-y-space-md text-body-sm">
              <div className="flex justify-between">
                <span className="text-on-surface-variant">活跃策略</span>
                <span className="font-mono">3 条</span>
              </div>
              <div className="flex justify-between">
                <span className="text-on-surface-variant">规则</span>
                <span className="font-mono">14 条 rules</span>
              </div>
              <div className="flex justify-between">
                <span className="text-on-surface-variant">扫描器高频探测</span>
                <span className="font-mono text-warning">5次/10s</span>
              </div>
              <div className="flex justify-between">
                <span className="text-on-surface-variant">爆破特征</span>
                <span className="font-mono text-warning">15次/1min</span>
              </div>
              <div className="flex items-center justify-between pt-space-md border-t border-surface-container-highest">
                <span className="text-on-surface-variant">自动封禁档位</span>
                <LoaBadge loa={7} label="全自动" />
              </div>
              <div className="flex justify-between">
                <span className="text-on-surface-variant">封禁动作</span>
                <span className="text-label-md text-outline">静默重置 TCP RST · 限流/伪装警告</span>
              </div>
            </div>
          </div>

          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
            <h3 className="text-headline-sm font-semibold mb-space-md">灰度观察名单</h3>
            <div className="space-y-space-md text-body-sm">
              {['103.94.11.8 · 临时抑制 1h', '91.240.118.6 · 冷却 22:00'].map((ip) => (
                <div key={ip} className="flex items-center justify-between bg-surface-container-low rounded px-space-lg py-space-md">
                  <span className="font-mono text-label-md">{ip.split(' ')[0]}</span>
                  <span className="text-label-sm text-warning">{ip.split(' ')[1]}</span>
                </div>
              ))}
              <button className="w-full text-label-md text-error border border-error/40 rounded py-space-md hover:bg-error/10 mt-space-sm">
                立即生效封禁 · LOA 4 须双人审批解封
              </button>
            </div>
          </div>

          <div className="bg-surface-container rounded-lg border border-brand/30 p-space-xl">
            <h3 className="text-headline-sm font-semibold mb-space-md flex items-center gap-space-sm">
              <Icon name="school" size={16} className="text-brand" />
              防御经验卡
            </h3>
            <p className="text-body-sm text-on-surface-variant leading-relaxed">
              GraphQL 批量查询深度拦截模版已沉淀为知识条目，回滚准备好 · 0 次回滚 · 命中规则自动升级。
            </p>
          </div>
        </div>
      </div>
    </>
  )
}
