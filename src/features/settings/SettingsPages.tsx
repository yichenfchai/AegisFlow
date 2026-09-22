import { useState } from 'react'
import { PageHeader, Button, Field, Input, Select } from '@/components/ui/DataTable'
import { Segmented, Toggle } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'

type Tab = 'general' | 'ai' | 'daemon' | 'members' | 'about'

/** 设置页（_14/_15 稿）：通用 / AI Provider / 成员管理 / 关于 */
export function SettingsPage() {
  const [tab, setTab] = useState<Tab>('general')
  return (
    <>
      <PageHeader
        title="设置"
        subtitle="平台全局配置"
        actions={
          <span className="flex items-center gap-space-sm text-label-md text-secondary">
            <span className="w-1.5 h-1.5 rounded-full bg-secondary gp-breathe" />
            引擎在线
          </span>
        }
      />
      <Segmented
        className="mb-space-xl"
        value={tab}
        onChange={setTab}
        options={[
          { value: 'general', label: '通用' },
          { value: 'ai', label: 'AI Provider' },
          { value: 'members', label: '成员管理' },
          { value: 'about', label: '关于' },
        ]}
      />
      {tab === 'general' && <GeneralTab />}
      {tab === 'ai' && <AiProviderTab />}
      {tab === 'members' && <MembersTab />}
      {tab === 'about' && <AboutTab />}
    </>
  )
}

function GeneralTab() {
  const [ldap, setLdap] = useState(false)
  const [lockout, setLockout] = useState(true)
  return (
    <div className="max-w-[880px] space-y-gutter">
      <Card title="平台标识" tag="NODE_METADATA_V1">
        <div className="grid grid-cols-2 gap-space-xl">
          <Field label="实例名称" hint="用于守护服务内部寻址与遥测上报">
            <Input defaultValue="solo-server" />
          </Field>
          <Field label="实例 ID" hint="系统初始化时由节点机密哈希派生">
            <div className="relative">
              <Input mono defaultValue="gp-7f3a-2c91" readOnly />
              <Icon
                name="content_copy"
                size={16}
                className="absolute right-3 top-1/2 -translate-y-1/2 text-outline cursor-pointer hover:text-on-surface"
              />
            </div>
          </Field>
        </div>
      </Card>
      <Card title="访问与会话" tag="AUTH_SECURITY_SPEC">
        <SwitchRow
          title="允许 LDAP 登录"
          desc="预留接口，当前未配置 LDAP 服务器"
          checked={ldap}
          onChange={setLdap}
          disabled
          badge="EX"
        />
        <SwitchRow
          title="会话记住时长"
          desc="颁发的客户端 Token 最大有效生命周期"
          checked
          select={<Select defaultValue="30"><option>7 天</option><option>14 天</option><option>30 天</option><option>90 天</option></Select>}
        />
        <SwitchRow title="登录失败锁定" desc="5 次失败后锁定 15 分钟" checked={lockout} onChange={setLockout} badge="ACTIVE" />
      </Card>
      <div className="rounded-lg border border-error/40 p-space-xl flex items-center justify-between">
        <div>
          <h3 className="text-headline-sm font-semibold text-error flex items-center gap-space-sm">
            <Icon name="warning" size={16} />
            重置平台配置
          </h3>
          <p className="text-body-sm text-on-surface-variant mt-space-xs">
            将恢复所有策略默认值，审计日志保留。此操作不可逆。
          </p>
        </div>
        <Button variant="danger">重置…</Button>
      </div>
    </div>
  )
}

function AiProviderTab() {
  const [mode, setMode] = useState<'openai' | 'ollama' | 'rule'>('openai')
  return (
    <div className="max-w-[880px] space-y-gutter">
      <Card title="模型接入" tag="ENDPOINT_CONFIG">
        <Segmented
          value={mode}
          onChange={setMode}
          options={[
            { value: 'openai', label: 'OpenAI 兼容 API' },
            { value: 'ollama', label: 'Ollama 本地' },
            { value: 'rule', label: '仅规则直答' },
          ]}
        />
        {mode !== 'rule' && (
          <div className="grid grid-cols-2 gap-space-xl mt-space-lg">
            <Field label="Base URL">
              <Input mono defaultValue="https://api.deepseek.com/v1" />
            </Field>
            <Field label="API Key">
              <div className="relative">
                <Input mono type="password" defaultValue="sk-dummy-key-3f7a" />
                <span className="absolute right-3 top-1/2 -translate-y-1/2 text-label-sm text-secondary">已加密存储</span>
              </div>
            </Field>
            <Field label="默认模型">
              <Select defaultValue="deepseek-chat">
                <option>deepseek-chat</option>
                <option>deepseek-reasoner</option>
                <option>qwen2.5-coder:32b (Ollama)</option>
              </Select>
            </Field>
          </div>
        )}
        <div className="flex items-center justify-between mt-space-lg">
          <span className="flex items-center gap-space-sm text-label-md text-outline">
            <span className="w-1.5 h-1.5 rounded-full bg-outline" />
            待验证连接
          </span>
          <div className="flex gap-space-md">
            <Button variant="secondary" size="sm">
              查看可用模型
            </Button>
            <Button size="sm" icon="play_arrow">
              测试连接
            </Button>
          </div>
        </div>
      </Card>

      <Card title="路由策略" tag="CASCADE_PIPELINE">
        <div className="space-y-space-md">
          <RouteRow
            green
            title="命中已验证知识条目"
            desc="高置信度静态规范条目直接返回，跳过 LLM 运算"
            action="→ 规则直答"
            actionTone="text-secondary"
          />
          <RouteRow
            title="诊断 / 审查请求"
            desc="复杂 AST 静态漏洞分析、多分支流水线异常推演"
            action="→ 默认模型"
            actionTone="text-primary"
          />
          <RouteRow
            warn
            title="模型失败或超时"
            desc="上游 API 5xx、DNS 异常或 8000ms 响应逾期"
            action="→ 自动降级 Ollama"
            actionTone="text-warning"
            warnText="当前未检测到本地 Ollama"
          />
        </div>
      </Card>

      <Card title="用量统计（本周）" tag="W42 (10/14 - 10/20)">
        <div className="grid grid-cols-3 gap-gutter">
          <MiniStat label="调用次数" value="148" unit="reqs" />
          <MiniStat label="消耗 Token" value="96.2K" unit="tokens" />
          <MiniStat label="规则直答节省" value="43 次" unit="~28.9% 算力节约" accent="ai" badge="AI 信号" />
        </div>
      </Card>
    </div>
  )
}

function MembersTab() {
  const members = [
    { name: 'Alex Mercer', email: 'alex@team.dev', role: '管理员', domains: '全部域', active: '10 分钟前', self: true },
    { name: 'Taylor Lin', email: 'taylor@team.dev', role: '成员', domains: '交付 + 质量与安全', active: '1 小时前' },
    { name: 'Elena Rostova', email: 'elena@team.dev', role: '成员', domains: '质量与安全', active: '3 天前' },
    { name: 'David Kim', email: 'david@team.dev', role: '观察者', domains: '只读全部', active: '2 周前' },
  ]
  return (
    <div className="max-w-[880px]">
      <div className="flex justify-end mb-space-lg">
        <Button icon="person_add">邀请成员</Button>
      </div>
      <table className="w-full bg-surface-container rounded-lg border border-surface-container-highest text-body-md">
        <thead>
          <tr className="text-label-md text-outline border-b border-surface-container-highest">
            <th className="text-left px-space-xl py-space-md font-normal">成员</th>
            <th className="text-left px-space-xl py-space-md font-normal">角色</th>
            <th className="text-left px-space-xl py-space-md font-normal">可访问域</th>
            <th className="text-left px-space-xl py-space-md font-normal">最近活跃</th>
            <th className="text-right px-space-xl py-space-md font-normal">操作</th>
          </tr>
        </thead>
        <tbody>
          {members.map((m) => (
            <tr key={m.email} className="border-b border-surface-container-highest/50 last:border-0">
              <td className="px-space-xl py-space-md">
                <div className="font-medium">{m.name}</div>
                <div className="text-label-sm text-outline font-mono">{m.email}</div>
              </td>
              <td className="px-space-xl py-space-md">
                <Select defaultValue={m.role} className={m.role === '管理员' ? 'text-primary' : ''}>
                  <option>管理员</option>
                  <option>成员</option>
                  <option>观察者</option>
                </Select>
              </td>
              <td className="px-space-xl py-space-md text-on-surface-variant">{m.domains}</td>
              <td className="px-space-xl py-space-md text-on-surface-variant">{m.active}</td>
              <td className="px-space-xl py-space-md text-right">
                <button
                  disabled={m.self}
                  className="text-body-sm text-on-surface-variant hover:text-error disabled:opacity-30"
                >
                  移除
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="text-label-sm text-outline mt-space-md">共 {members.length} 名成员 · 角色变更将记入审计日志</p>
    </div>
  )
}

function AboutTab() {
  return (
    <div className="max-w-[880px]">
      <Card title="关于 GoPulse" tag="v2.4">
        <div className="space-y-space-md text-body-md text-on-surface-variant">
          <p>轻量交付治理平台 · 单二进制自托管 DevOps 控制台与自治治理引擎</p>
          <div className="grid grid-cols-2 gap-space-lg text-body-sm">
            <div>实例 ID: <span className="font-mono text-on-surface">gp-7f3a-2c91</span></div>
            <div>运行形态: <span className="font-mono text-on-surface">solo-server</span></div>
            <div>License: MIT</div>
            <div>© 2026 GoPulse</div>
          </div>
        </div>
      </Card>
    </div>
  )
}

/* ---- 局部小组件 ---- */
function Card({ title, tag, children }: { title: string; tag?: string; children: React.ReactNode }) {
  return (
    <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
      <div className="flex items-center justify-between mb-space-lg">
        <h2 className="text-headline-md font-semibold">{title}</h2>
        {tag && <span className="font-mono text-label-sm text-outline">{tag}</span>}
      </div>
      {children}
    </div>
  )
}

function SwitchRow({
  title,
  desc,
  checked,
  onChange,
  disabled,
  badge,
  select,
}: {
  title: string
  desc: string
  checked: boolean
  onChange?: (v: boolean) => void
  disabled?: boolean
  badge?: string
  select?: React.ReactNode
}) {
  return (
    <div className="flex items-center justify-between py-space-lg border-b border-surface-container-highest/50 last:border-0">
      <div>
        <div className="flex items-center gap-space-sm">
          <span className="text-body-md font-medium">{title}</span>
          {badge && (
            <span className={`text-label-sm rounded px-1 ${badge === 'ACTIVE' ? 'bg-secondary-container/50 text-on-secondary-container' : 'bg-surface-container-high text-outline'}`}>
              {badge}
            </span>
          )}
        </div>
        <p className="text-label-sm text-outline mt-0.5">{desc}</p>
      </div>
      {select || <Toggle checked={checked} onChange={onChange} disabled={disabled} />}
    </div>
  )
}

function RouteRow({
  title,
  desc,
  action,
  actionTone,
  green,
  warn,
  warnText,
}: {
  title: string
  desc: string
  action: string
  actionTone: string
  green?: boolean
  warn?: boolean
  warnText?: string
}) {
  return (
    <div className="flex items-center justify-between bg-surface-container-low rounded p-space-lg">
      <div className="flex items-center gap-space-md">
        {green && <Icon name="check_circle" size={18} className="text-secondary" fill />}
        {warn && <Icon name="error" size={18} className="text-error" fill />}
        <div>
          <div className="text-body-md font-medium">{title}</div>
          <div className="text-label-sm text-outline">{desc}</div>
        </div>
      </div>
      <div className="text-right">
        <div className={`text-body-sm font-medium ${actionTone}`}>{action}</div>
        {warnText && <div className="text-label-sm text-warning flex items-center gap-1 justify-end"><Icon name="warning" size={12} />{warnText}</div>}
      </div>
    </div>
  )
}

function MiniStat({
  label,
  value,
  unit,
  accent,
  badge,
}: {
  label: string
  value: string
  unit?: string
  accent?: 'ai'
  badge?: string
}) {
  return (
    <div className="bg-surface-container-low rounded p-space-lg">
      <div className="flex items-center justify-between">
        <span className="text-body-sm text-on-surface-variant">{label}</span>
        {badge && (
          <span className="text-label-sm bg-brand/12 text-brand rounded px-1">{badge}</span>
        )}
      </div>
      <div className={`font-mono text-[22px] font-semibold mt-space-sm ${accent === 'ai' ? 'text-brand' : ''}`}>
        {value}
      </div>
      {unit && <div className="text-label-sm text-outline mt-0.5">{unit}</div>}
    </div>
  )
}
