import { useState } from 'react'
import { PageHeader, Button, Field, Stepper, ConfirmDialog } from '@/components/ui/DataTable'
import { LoaBadge } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'

/** 自动化策略页（_10 稿） */
export function PoliciesPage() {
  const [loa, setLoa] = useState(3)
  const [minVerify, setMinVerify] = useState(3)
  const [confidence, setConfidence] = useState(75)
  const [budget, setBudget] = useState(200)
  const [confirmReset, setConfirmReset] = useState(false)

  const LOA_OPTIONS = [
    { value: 1, label: 'L1 仅建议', desc: 'AI 只输出建议，不执行任何动作' },
    { value: 2, label: 'L2 需单次批准', desc: '动作进入审批中心，人工裁决后执行' },
    { value: 3, label: 'L3 阈值信任', desc: '满足验证次数与可信度阈值时自动执行' },
    { value: 4, label: 'L4 全自动', desc: '预算内免审执行，全量证据链审计' },
  ]

  return (
    <>
      <PageHeader
        title="自动化策略"
        subtitle="LOA 自治档位与安全阈值 · 策略: team-preset"
        actions={
          <>
            <Button variant="secondary">查看策略变更历史</Button>
            <Button icon="check" onClick={() => alert('策略已保存（演示）')}>
              保存策略
            </Button>
          </>
        }
      />

      <div className="max-w-[880px] space-y-gutter">
        {/* LOA 档位选择器 */}
        <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
          <h2 className="text-headline-md font-semibold mb-space-lg">LOA 自治档位选择器</h2>
          <div className="grid grid-cols-4 gap-gutter">
            {LOA_OPTIONS.map((opt) => (
              <button
                key={opt.value}
                onClick={() => setLoa(opt.value)}
                className={`text-left rounded-lg border p-space-lg transition-all ${
                  loa === opt.value
                    ? 'border-primary bg-primary-container/10'
                    : 'border-surface-container-highest hover:border-outline-variant'
                }`}
              >
                <div className="flex items-center justify-between mb-space-sm">
                  <LoaBadge loa={opt.value} />
                  {loa === opt.value && <Icon name="check_circle" size={16} className="text-primary" fill />}
                </div>
                <div className="text-body-md font-medium text-on-surface">{opt.label.split(' ').slice(1).join(' ')}</div>
                <p className="text-label-sm text-outline mt-space-xs leading-relaxed">{opt.desc}</p>
              </button>
            ))}
          </div>
        </div>

        {/* 参数与安全阈值 */}
        <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
          <h2 className="text-headline-md font-semibold mb-space-lg">参数与安全阈值</h2>
          <div className="grid grid-cols-3 gap-space-xl">
            <Field label="最小验证次数" hint="知识条目升级所需验证下限">
              <div className="pt-1">
                <Stepper value={minVerify} onChange={setMinVerify} min={1} max={10} />
              </div>
            </Field>
            <Field label={`自愈可信度阈值 · ${confidence}%`} hint="低于阈值的自动修复转人工">
              <input
                type="range"
                min={50}
                max={99}
                value={confidence}
                onChange={(e) => setConfidence(+e.target.value)}
                className="w-full accent-primary mt-3"
              />
            </Field>
            <Field label="每日自动化预算" hint="超限动作自动转 L2 审批">
              <div className="pt-1">
                <Stepper value={budget} onChange={setBudget} min={10} max={1000} />
              </div>
            </Field>
          </div>
        </div>

        {/* 项目偏好 */}
        <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
          <h2 className="text-headline-md font-semibold mb-space-lg">动作档位明细</h2>
          <table className="w-full text-body-md">
            <thead>
              <tr className="text-label-md text-outline border-b border-surface-container-highest">
                <th className="text-left py-space-md font-normal">动作类型</th>
                <th className="text-left py-space-md font-normal">档位</th>
                <th className="text-left py-space-md font-normal">说明</th>
              </tr>
            </thead>
            <tbody>
              {[
                ['部署下发', 6, '构建时预授权，健康检查通过后自动执行'],
                ['部署回滚', 2, '需人工确认，二次弹窗'],
                ['AI 修复重跑', loa, '按当前档位执行'],
                ['依赖自动升级', 3, 'pnpm 修复类动作 · 命中已验证规则免调模型'],
                ['自动封禁', 7, '攻击防御全自动，双人审批解封'],
              ].map(([name, l, desc]) => (
                <tr key={name as string} className="border-b border-surface-container-highest/50 last:border-0">
                  <td className="py-space-md">{name}</td>
                  <td className="py-space-md">
                    <LoaBadge loa={l as number} />
                  </td>
                  <td className="py-space-md text-on-surface-variant text-body-sm">{desc}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        {/* 危险区 */}
        <div className="rounded-lg border border-error/40 p-space-xl flex items-center justify-between">
          <div>
            <h3 className="text-headline-sm font-semibold text-error flex items-center gap-space-sm">
              <Icon name="warning" size={16} />
              危险区
            </h3>
            <p className="text-body-sm text-on-surface-variant mt-space-xs">
              重置所有策略为默认值（L2），审计日志保留
            </p>
          </div>
          <Button variant="danger" onClick={() => setConfirmReset(true)}>
            重置…
          </Button>
        </div>
      </div>

      <ConfirmDialog
        open={confirmReset}
        danger
        title="重置平台策略"
        confirmText="确认重置"
        onConfirm={() => setConfirmReset(false)}
        onCancel={() => setConfirmReset(false)}
      >
        将恢复所有 LOA 档位与阈值为默认值。此操作不可逆，且本身将记入审计日志。
      </ConfirmDialog>
    </>
  )
}
