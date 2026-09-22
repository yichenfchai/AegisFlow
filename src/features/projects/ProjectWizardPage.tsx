import { useState } from 'react'
import { useNavigate } from 'react-router'
import { PageHeader, Button, Field, Input, Select } from '@/components/ui/DataTable'
import { Icon } from '@/components/ui/Icon'
import { Stepper } from '@/components/ui/DataTable'

/** 项目接入向导（_12 稿）：仓库来源与触发 → 构建配置 → 部署目标 */
export function ProjectWizardPage() {
  const nav = useNavigate()
  const [step, setStep] = useState(1)
  const [name, setName] = useState('')
  const [repo, setRepo] = useState('~/workspace/web-api')
  const [branch, setBranch] = useState('main')
  const [trigger, setTrigger] = useState<'webhook' | 'cron' | 'manual'>('webhook')
  const [targets, setTargets] = useState<string[]>(['dt-1'])
  const [secretsLinked, setSecretsLinked] = useState<string[]>(['sk-1', 'sk-2'])
  const [buildCmd, setBuildCmd] = useState('pnpm build')
  const [keepTags, setKeepTags] = useState(5)

  const webhookVerified = name.length > 0 && repo.length > 0

  const canNext = step === 1 ? name.trim().length > 0 && repo.trim().length > 0 : step === 2 ? buildCmd.trim().length > 0 : targets.length > 0

  return (
    <>
      <PageHeader title="项目接入向导" subtitle="三步完成项目接入 · 完成后立即可触发构建" />

      {/* 步骤条 */}
      <div className="flex items-center gap-space-lg mb-space-xl max-w-[880px]">
        {['仓库来源', '构建配置', '部署目标'].map((label, i) => {
          const n = i + 1
          return (
            <div key={label} className="flex items-center gap-space-lg flex-1 last:flex-none">
              <div className="flex items-center gap-space-md">
                <span
                  className={`w-6 h-6 rounded-full flex items-center justify-center font-mono text-label-md ${
                    step > n
                      ? 'bg-secondary text-on-secondary'
                      : step === n
                        ? 'bg-primary text-on-primary font-semibold'
                        : 'bg-surface-container-high text-outline'
                  }`}
                >
                  {step > n ? <Icon name="check" size={14} /> : n}
                </span>
                <span className={`text-body-md ${step === n ? 'text-on-surface font-medium' : 'text-outline'}`}>{label}</span>
              </div>
              {i < 2 && <span className="flex-1 h-px bg-surface-container-highest" />}
            </div>
          )
        })}
      </div>

      <div className="max-w-[880px] bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg">
        {step === 1 && (
          <>
            <h2 className="text-headline-md font-semibold">仓库来源</h2>
            <div className="grid grid-cols-2 gap-gutter">
              <Field label="项目名称" hint="全局唯一，用于构建号与审计">
                <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="web-api" />
              </Field>
              <Field label="仓库来源" hint="支持本地路径或 Git URL">
                <Input mono value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="~/workspace/web-api" />
              </Field>
            </div>
            <div className="grid grid-cols-2 gap-gutter">
              <Field label="默认分支">
                <Select value={branch} onChange={(e) => setBranch(e.target.value)}>
                  <option>main</option>
                  <option>master</option>
                  <option>develop</option>
                </Select>
              </Field>
              <Field label="分支与触发 · 触发方式">
                <Select value={trigger} onChange={(e) => setTrigger(e.target.value as typeof trigger)}>
                  <option value="webhook">Webhook 推送</option>
                  <option value="cron">定时构建 (Cron)</option>
                  <option value="manual">手动分发</option>
                </Select>
              </Field>
            </div>
            {trigger === 'webhook' && (
              <div
                className={`flex items-center gap-space-md rounded p-space-lg text-body-sm ${
                  webhookVerified ? 'bg-secondary/10 text-secondary' : 'bg-surface-container-low text-outline'
                }`}
              >
                <Icon name={webhookVerified ? 'check_circle' : 'radio_button_unchecked'} size={16} fill={webhookVerified} />
                {webhookVerified ? (
                  <>
                    已验证握手 ✓ · Payload 回调终结点已就绪
                    <span className="ml-auto font-mono text-label-sm text-outline">POST /api/v1/hooks/github/…</span>
                  </>
                ) : (
                  '填写项目名称与仓库来源后自动生成 Webhook 端点'
                )}
              </div>
            )}
            {trigger === 'cron' && (
              <Field label="Cron 表达式">
                <Input mono defaultValue="0 2 * * *" />
              </Field>
            )}
          </>
        )}

        {step === 2 && (
          <>
            <h2 className="text-headline-md font-semibold">构建配置</h2>
            <div className="grid grid-cols-2 gap-gutter">
              <Field label="构建命令" hint="在容器内执行的工作目录命令">
                <Input mono value={buildCmd} onChange={(e) => setBuildCmd(e.target.value)} />
              </Field>
              <Field label="容器镜像" hint="构建执行环境">
                <Select>
                  <option>node:22-bookworm (pnpm 预装)</option>
                  <option>golang:1.22</option>
                  <option>自定义镜像</option>
                </Select>
              </Field>
            </div>
            <Field label="产物类型">
              <Select>
                <option>Docker 镜像 (Image Artifact)</option>
                <option>静态文件</option>
              </Select>
            </Field>
            <div className="bg-surface-container-low rounded p-space-lg font-mono text-[12px] text-outline leading-relaxed">
              <div className="text-primary mb-1">流水线预览（自动编排）</div>
              1. Checkout — 检出代码 ({branch}){'\n'}
              2. Install — 安装依赖{'\n'}
              3. Build — {buildCmd || '…'}{'\n'}
              4. Deploy — 自动化部署 (LOA 6 预授权)
            </div>
          </>
        )}

        {step === 3 && (
          <>
            <h2 className="text-headline-md font-semibold">部署目标</h2>
            <div className="space-y-space-md">
              {[
                { id: 'dt-1', name: 'prod-vps-01', addr: '10.0.0.8:22', health: '5/5 全部通过' },
                { id: 'dt-2', name: 'staging-vps', addr: '10.0.0.12:22', health: '5/5 全部通过' },
                { id: 'dt-3', name: 'backup-nas', addr: '192.168.1.50:22', health: '0/5 超时未响应' },
              ].map((t) => (
                <label
                  key={t.id}
                  className={`flex items-center gap-space-lg rounded border p-space-lg cursor-pointer transition-colors ${
                    targets.includes(t.id) ? 'border-primary bg-primary/5' : 'border-surface-container-highest hover:border-outline-variant'
                  }`}
                >
                  <input
                    type="checkbox"
                    checked={targets.includes(t.id)}
                    onChange={(e) => setTargets((prev) => (e.target.checked ? [...prev, t.id] : prev.filter((x) => x !== t.id)))}
                    className="accent-primary"
                  />
                  <div className="flex-1">
                    <div className="text-body-md font-medium">{t.name}</div>
                    <div className="text-label-sm text-outline font-mono">{t.addr}</div>
                  </div>
                  <span className={`text-label-sm ${t.health.includes('超时') ? 'text-error' : 'text-secondary'}`}>{t.health}</span>
                </label>
              ))}
            </div>

            <Field label="凭据关联" hint="从密钥库选择（Registry 凭据 · SSH 私钥）">
              <div className="flex flex-wrap gap-space-md">
                {[
                  { id: 'sk-1', name: 'SSH 私钥 (1)', fp: 'SHA256:9f3a…c1de' },
                  { id: 'sk-2', name: 'Registry 凭据', fp: 'SHA256:b7e2…44aa' },
                ].map((s) => (
                  <button
                    key={s.id}
                    type="button"
                    onClick={() => setSecretsLinked((p) => (p.includes(s.id) ? p.filter((x) => x !== s.id) : [...p, s.id]))}
                    className={`flex items-center gap-space-md rounded border px-space-lg py-space-md text-body-sm transition-colors ${
                      secretsLinked.includes(s.id) ? 'border-primary text-on-surface bg-primary/5' : 'border-surface-container-highest text-on-surface-variant'
                    }`}
                  >
                    <Icon name={secretsLinked.includes(s.id) ? 'check_box' : 'check_box_outline_blank'} size={16} />
                    {s.name}
                    <span className="font-mono text-label-sm text-outline">{s.fp}</span>
                  </button>
                ))}
                <button
                  type="button"
                  className="rounded border border-dashed border-outline-variant px-space-lg py-space-md text-body-sm text-outline hover:text-on-surface-variant"
                  onClick={() => alert('跳转密钥管理（演示）')}
                >
                  管理凭据库 ↗
                </button>
              </div>
            </Field>

            <div className="grid grid-cols-2 gap-gutter">
              <Field label="镜像 Tag 保留数">
                <div className="pt-1">
                  <Stepper value={keepTags} onChange={setKeepTags} min={1} max={20} />
                </div>
              </Field>
              <Field label="网络模式">
                <Select defaultValue="bridge">
                  <option value="bridge">bridge</option>
                  <option value="host">host</option>
                </Select>
              </Field>
            </div>
          </>
        )}

        {/* 底部操作 */}
        <div className="flex items-center justify-between pt-space-lg border-t border-surface-container-highest">
          <Button variant="ghost" onClick={() => (step > 1 ? setStep(step - 1) : nav('/'))}>
            {step > 1 ? '上一步' : '取消'}
          </Button>
          <div className="flex items-center gap-space-md">
            {step < 3 ? (
              <Button disabled={!canNext} onClick={() => setStep(step + 1)}>
                下一步：{['构建配置', '部署目标'][step - 1]}
              </Button>
            ) : (
              <Button
                icon="check"
                disabled={!canNext}
                onClick={() => {
                  alert(`项目「${name}」已创建（演示）\n已选 ${targets.length} 个部署目标 · ${secretsLinked.length} 个凭据`)
                  nav('/')
                }}
              >
                确认创建
              </Button>
            )}
          </div>
        </div>
      </div>
    </>
  )
}
