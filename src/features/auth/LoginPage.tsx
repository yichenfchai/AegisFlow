import { useState } from 'react'
import { useNavigate } from 'react-router'
import { Icon } from '@/components/ui/Icon'
import { Button } from '@/components/ui/DataTable'
import { useSession, mockJwtFor } from '@/stores/session'

/** 登录页（gopulse 稿）：本地账号 + LDAP 入口 · JWT 会话 */
export function LoginPage() {
  const nav = useNavigate()
  const login = useSession((s) => s.login)
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [remember, setRemember] = useState(true)
  const [showPwd, setShowPwd] = useState(false)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    setError('')
    if (!username.trim()) {
      setError('用户名不能为空')
      return
    }
    if (!password) {
      setError('密码不能为空')
      return
    }
    setLoading(true)
    // mock 鉴权：任何非空账号通过；真实后端就绪后替换为 POST /api/v1/auth/login
    await new Promise((r) => setTimeout(r, 500))
    if (password.toLowerCase() === 'ldap') {
      setError('LDAP 登录当前未配置，请使用本地账号')
      setLoading(false)
      return
    }
    login(
      {
        id: 'u-alex',
        username,
        displayName: username === 'alex' ? 'Alex Mercer' : username,
        role: 'admin',
      },
      mockJwtFor(username),
    )
    setLoading(false)
    nav('/', { replace: true })
  }

  return (
    <div className="min-h-screen bg-bg flex items-center justify-center relative overflow-hidden">
      {/* 背景脉冲装饰 */}
      <div className="absolute inset-0 pointer-events-none opacity-40">
        <svg className="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2" width="900" height="600" viewBox="0 0 900 600" fill="none">
          <path d="M0 300 H340 L380 260 L420 340 L450 210 L480 380 L510 290 H900" stroke="var(--brand)" strokeOpacity="0.25" strokeWidth="1.5" />
          <path d="M0 330 H320 L360 290 L400 370 L430 240 L460 410 L490 320 H900" stroke="var(--success)" strokeOpacity="0.12" strokeWidth="1" />
          <circle cx="450" cy="300" r="140" stroke="var(--brand)" strokeOpacity="0.08" />
          <circle cx="450" cy="300" r="220" stroke="var(--brand)" strokeOpacity="0.05" />
        </svg>
      </div>

      <div className="w-[400px] relative z-10">
        {/* 品牌 */}
        <div className="flex flex-col items-center mb-space-xl">
          <div className="flex items-center gap-space-md mb-space-md">
            <svg width="36" height="36" viewBox="0 0 36 36" fill="none">
              <path d="M2 18 H12 L15 12 L19 24 L22 8 L25 22 L28 18 H34" stroke="var(--brand)" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M28 14 L34 18 L28 22" stroke="var(--success)" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" fill="none" />
            </svg>
            <span className="text-[24px] font-semibold text-on-surface tracking-tight">GoPulse</span>
          </div>
          <p className="text-body-md text-outline">轻量交付治理平台 · v2.4</p>
        </div>

        {/* 登录卡片 */}
        <form onSubmit={submit} className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg">
          <div>
            <label className="block text-label-md text-on-surface-variant mb-space-sm">用户名</label>
            <div className="relative">
              <Icon name="person" size={16} className="absolute left-3 top-1/2 -translate-y-1/2 text-outline" />
              <input
                autoFocus
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder="用户名或 LDAP 账号"
                className="w-full bg-surface-container-lowest border border-surface-container-highest rounded pl-9 pr-3 py-space-md text-body-md text-on-surface placeholder:text-text-3 focus:outline-none focus:border-primary/60 transition-colors"
              />
            </div>
          </div>

          <div>
            <label className="block text-label-md text-on-surface-variant mb-space-sm">密码</label>
            <div className="relative">
              <Icon name="lock" size={16} className="absolute left-3 top-1/2 -translate-y-1/2 text-outline" />
              <input
                type={showPwd ? 'text' : 'password'}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="••••••••"
                className="w-full bg-surface-container-lowest border border-surface-container-highest rounded pl-9 pr-9 py-space-md text-body-md text-on-surface placeholder:text-text-3 focus:outline-none focus:border-primary/60 transition-colors"
              />
              <button
                type="button"
                onClick={() => setShowPwd((v) => !v)}
                className="absolute right-3 top-1/2 -translate-y-1/2 text-outline hover:text-on-surface"
                aria-label="Toggle password visibility"
              >
                <Icon name={showPwd ? 'visibility_off' : 'visibility'} size={16} />
              </button>
            </div>
          </div>

          {error && (
            <div className="flex items-center gap-space-sm text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
              <Icon name="error" size={14} />
              {error}
            </div>
          )}

          <div className="flex items-center justify-between text-body-sm">
            <label className="flex items-center gap-space-sm text-on-surface-variant cursor-pointer select-none">
              <input
                type="checkbox"
                checked={remember}
                onChange={(e) => setRemember(e.target.checked)}
                className="accent-primary"
              />
              记住此会话 (30 天)
            </label>
            <button type="button" className="text-primary hover:underline" onClick={() => alert('请联系管理员重置（演示）')}>
              忘记密码?
            </button>
          </div>

          <Button type="submit" className="w-full justify-center" disabled={loading}>
            {loading ? '验证中...' : '登录'}
          </Button>

          <div className="flex items-center gap-space-lg">
            <span className="flex-1 h-px bg-surface-container-highest" />
            <span className="text-label-sm text-outline">或</span>
            <span className="flex-1 h-px bg-surface-container-highest" />
          </div>

          <Button
            type="button"
            variant="secondary"
            className="w-full justify-center"
            onClick={() => {
              setPassword('ldap')
              setError('提示：输入密码为 "ldap" 可体验 LDAP 分支的失败提示（当前未配置）')
            }}
          >
            <Icon name="domain" size={16} />
            使用 LDAP 登录
          </Button>
        </form>

        <p className="text-center text-label-sm text-outline mt-space-xl font-mono">
          GoPulse v2.4 · © 2026 · solo-server
        </p>
      </div>
    </div>
  )
}
