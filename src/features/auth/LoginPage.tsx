import { useEffect, useState } from 'react'
import { useLocation, useNavigate } from 'react-router'
import { Icon } from '@/components/ui/Icon'
import { Button } from '@/components/ui/DataTable'
import type { LDAPConfig, OIDCConfig } from '@/api/http'
import { errorMessage, useSession } from '@/stores/session'
import { SessionGate } from './SessionGate'
import { TENANT_REQUIRED_TEXT, runLoginAttempt, tenantSlugFromSearch } from './loginAttempt'
import { safeReturnTo } from './returnTo'
import { loadOIDCConfig, ssoButtonLabel, ssoErrorMessage, ssoStartURL } from './sso'
import {
  ldapButtonLabel,
  ldapEnabled,
  ldapLoginErrorMessage,
  loadLDAPConfig,
  runLDAPLoginAttempt,
} from './ldap'

export function LoginPage() {
  return <SessionGate publicOnly><LoginForm /></SessionGate>
}

function LoginForm() {
  const nav = useNavigate()
  const location = useLocation()
  const login = useSession((s) => s.login)
  const loginLDAP = useSession((s) => s.loginLDAP)
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [remember, setRemember] = useState(true)
  const [showPwd, setShowPwd] = useState(false)
  // 工作空间标识（阶段 F3）：默认收起，单工作空间登录保持原样；?tenant=<slug>
  // 可预填并展开，tenant_required 时也会自动展开并保留已填邮箱/密码。
  const [tenantSlug, setTenantSlug] = useState(() => tenantSlugFromSearch(location.search))
  const [tenantOpen, setTenantOpen] = useState(() => tenantSlugFromSearch(location.search) !== '')
  const [tenantSlugRequired, setTenantSlugRequired] = useState(false)
  // SSO callback failures arrive as ?sso_error=<固定错误码>；初始 error 直接取映射，
  // 随后把该参数从地址栏清掉，避免刷新重复提示或把错误码留在历史记录里。
  const [error, setError] = useState(() => ssoErrorMessage(new URLSearchParams(location.search).get('sso_error')) ?? '')
  const [loading, setLoading] = useState(false)
  const [oidc, setOIDC] = useState<OIDCConfig | null>(null)
  const ssoLabel = ssoButtonLabel(oidc)
  // LDAP 目录认证（阶段 G）是可选能力：未配置时页面与纯密码登录完全一致；
  // 启用时在密码表单上方显示「{label}」入口，点击展开目录表单（用户名、密码、
  // 可选工作空间标识）。目录表单没有「保持登录」，会话为非持久 Cookie。
  const [ldap, setLDAP] = useState<LDAPConfig | null>(null)
  const [ldapOpen, setLDAPOpen] = useState(false)
  const [ldapUsername, setLDAPUsername] = useState('')
  const [ldapPassword, setLDAPPassword] = useState('')
  const [ldapTenantSlug, setLDAPTenantSlug] = useState('')
  const [ldapTenantOpen, setLDAPTenantOpen] = useState(false)
  const [ldapTenantRequired, setLDAPTenantRequired] = useState(false)
  const ldapLabel = ldapButtonLabel(ldap)
  const ldapIsEnabled = ldapEnabled(ldap)

  // OIDC 是可选能力：探针失败在 loadOIDCConfig 内归一为未启用，页面保持纯密码登录。
  useEffect(() => {
    let active = true
    void loadOIDCConfig().then((config) => {
      if (active) setOIDC(config)
    })
    return () => {
      active = false
    }
  }, [])

  // LDAP 探针同理：未配置或探针失败都归一为未启用，不显示目录入口、不报错。
  useEffect(() => {
    let active = true
    void loadLDAPConfig().then((config) => {
      if (active) setLDAP(config)
    })
    return () => {
      active = false
    }
  }, [])

  useEffect(() => {
    const params = new URLSearchParams(location.search)
    if (!params.has('sso_error')) return
    params.delete('sso_error')
    const search = params.toString()
    nav({ pathname: location.pathname, search: search ? `?${search}` : '' }, { replace: true })
  }, [location.pathname, location.search, nav])

  const startSSO = () => {
    window.location.assign(ssoStartURL(new URLSearchParams(location.search).get('returnTo')))
  }

  const openLDAP = () => {
    setLDAPOpen(true)
    setError('')
  }

  const closeLDAP = () => {
    setLDAPOpen(false)
    setError('')
  }

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (loading) return
    setError('')
    setLoading(true)
    try {
      const outcome = await runLoginAttempt(
        { username, password, remember, tenantSlug, tenantSlugRequired },
        login,
      )
      if (outcome.kind === 'invalid') {
        setError(outcome.message)
        return
      }
      if (outcome.kind === 'tenant_required') {
        // 歧义登录：保留邮箱/密码，展开工作空间标识并提示，用户补填后重试。
        setTenantSlugRequired(true)
        setTenantOpen(true)
        setError(errorMessage(outcome.error))
        return
      }
      if (outcome.kind === 'failed') {
        setError(errorMessage(outcome.error))
        return
      }
      setError('')
      nav(safeReturnTo(new URLSearchParams(location.search).get('returnTo')), { replace: true })
    } finally {
      setLoading(false)
    }
  }

  // 目录登录：成功后与密码登录一致进入 returnTo；401 统一「用户名或密码错误」，
  // 503 提示目录服务暂不可用，tenant_required 展开工作空间标识并保留已填输入。
  const submitLDAPLogin = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (loading) return
    setError('')
    setLoading(true)
    try {
      const outcome = await runLDAPLoginAttempt(
        {
          username: ldapUsername,
          password: ldapPassword,
          tenantSlug: ldapTenantSlug,
          tenantSlugRequired: ldapTenantRequired,
        },
        loginLDAP,
      )
      if (outcome.kind === 'invalid') {
        setError(outcome.message)
        return
      }
      if (outcome.kind === 'tenant_required') {
        setLDAPTenantRequired(true)
        setLDAPTenantOpen(true)
        setError(TENANT_REQUIRED_TEXT)
        return
      }
      if (outcome.kind === 'failed') {
        setError(ldapLoginErrorMessage(outcome.error))
        return
      }
      setError('')
      nav(safeReturnTo(new URLSearchParams(location.search).get('returnTo')), { replace: true })
    } finally {
      setLoading(false)
    }
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
            <span className="text-[24px] font-semibold text-on-surface tracking-tight">AegisFlow</span>
          </div>
          <p className="text-body-md text-outline">轻量交付治理平台</p>
        </div>

        {/* 登录卡片：OIDC 已配置时在密码表单上方显示「{label}」入口，未配置时完全不渲染；
            LDAP 已配置时同样在密码表单上方显示目录入口，未配置时保持原有占位与行为。 */}
        <form onSubmit={ldapOpen ? submitLDAPLogin : submit} aria-busy={loading} className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg">
          {ssoLabel && (
            <>
              <Button
                type="button"
                variant="secondary"
                className="w-full justify-center"
                disabled={loading}
                onClick={startSSO}
              >
                <Icon name="domain" size={16} />
                {ssoLabel}
              </Button>
              <div className="flex items-center gap-space-lg">
                <span className="flex-1 h-px bg-surface-container-highest" />
                <span className="text-label-sm text-outline">或使用密码登录</span>
                <span className="flex-1 h-px bg-surface-container-highest" />
              </div>
            </>
          )}
          {ldapLabel && !ldapOpen && (
            <Button
              type="button"
              variant="secondary"
              className="w-full justify-center"
              disabled={loading}
              onClick={openLDAP}
            >
              <Icon name="domain" size={16} />
              {ldapLabel}
            </Button>
          )}
          {ldapLabel && ldapOpen && (
            <button
              type="button"
              onClick={closeLDAP}
              disabled={loading}
              className="self-start text-label-sm text-primary hover:underline disabled:opacity-50"
            >
              使用密码登录
            </button>
          )}
          {!ldapOpen && (
            <>
              <div>
                <label htmlFor="login-username" className="block text-label-md text-on-surface-variant mb-space-sm">用户名</label>
                <div className="relative">
                  <Icon name="person" size={16} className="absolute left-3 top-1/2 -translate-y-1/2 text-outline" />
                  <input
                    id="login-username"
                    name="username"
                    autoComplete="username"
                    disabled={loading}
                    autoFocus
                    value={username}
                    onChange={(e) => setUsername(e.target.value)}
                    placeholder="用户名"
                    className="w-full bg-surface-container-lowest border border-surface-container-highest rounded pl-9 pr-3 py-space-md text-body-md text-on-surface placeholder:text-text-3 focus:outline-none focus:border-primary/60 transition-colors"
                  />
                </div>
              </div>

              <div>
                <label htmlFor="login-password" className="block text-label-md text-on-surface-variant mb-space-sm">密码</label>
                <div className="relative">
                  <Icon name="lock" size={16} className="absolute left-3 top-1/2 -translate-y-1/2 text-outline" />
                  <input
                    id="login-password"
                    name="password"
                    autoComplete="current-password"
                    disabled={loading}
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
                    aria-label={showPwd ? '隐藏密码' : '显示密码'}
                    disabled={loading}
                  >
                    <Icon name={showPwd ? 'visibility_off' : 'visibility'} size={16} />
                  </button>
                </div>
              </div>

              {/* 工作空间标识：默认收起，单工作空间登录不受影响；tenant_required 或
                  ?tenant=<slug> 时展开。格式校验与提交由 loginAttempt.ts 负责。 */}
              {!tenantOpen && (
                <button
                  type="button"
                  onClick={() => setTenantOpen(true)}
                  disabled={loading}
                  className="self-start text-label-sm text-primary hover:underline disabled:opacity-50"
                >
                  填写工作空间标识
                </button>
              )}
              {tenantOpen && (
                <div>
                  <div className="flex items-center justify-between mb-space-sm">
                    <label htmlFor="login-tenant" className="block text-label-md text-on-surface-variant">工作空间标识</label>
                    {!tenantSlugRequired && (
                      <button
                        type="button"
                        onClick={() => setTenantOpen(false)}
                        disabled={loading}
                        className="text-label-sm text-outline hover:text-on-surface disabled:opacity-50"
                      >
                        收起
                      </button>
                    )}
                  </div>
                  <div className="relative">
                    <Icon name="domain" size={16} className="absolute left-3 top-1/2 -translate-y-1/2 text-outline" />
                    <input
                      id="login-tenant"
                      name="tenantSlug"
                      autoComplete="organization"
                      disabled={loading}
                      autoFocus
                      value={tenantSlug}
                      onChange={(e) => setTenantSlug(e.target.value)}
                      placeholder="例如 acme"
                      spellCheck={false}
                      className="w-full bg-surface-container-lowest border border-surface-container-highest rounded pl-9 pr-3 py-space-md text-body-md text-on-surface placeholder:text-text-3 focus:outline-none focus:border-primary/60 transition-colors font-mono"
                    />
                  </div>
                  <p className="mt-space-sm text-label-sm text-outline">
                    {tenantSlugRequired
                      ? '该邮箱存在于多个工作空间，请输入工作空间标识'
                      : '工作空间标识由管理员提供，只能包含小写字母、数字和短横线；只有一个工作空间时无需填写。'}
                  </p>
                </div>
              )}
            </>
          )}

          {ldapOpen && (
            <>
              <div>
                <label htmlFor="login-ldap-username" className="block text-label-md text-on-surface-variant mb-space-sm">目录用户名</label>
                <div className="relative">
                  <Icon name="person" size={16} className="absolute left-3 top-1/2 -translate-y-1/2 text-outline" />
                  <input
                    id="login-ldap-username"
                    name="username"
                    autoComplete="username"
                    disabled={loading}
                    autoFocus
                    value={ldapUsername}
                    onChange={(e) => setLDAPUsername(e.target.value)}
                    placeholder="目录用户名"
                    className="w-full bg-surface-container-lowest border border-surface-container-highest rounded pl-9 pr-3 py-space-md text-body-md text-on-surface placeholder:text-text-3 focus:outline-none focus:border-primary/60 transition-colors"
                  />
                </div>
              </div>

              <div>
                <label htmlFor="login-ldap-password" className="block text-label-md text-on-surface-variant mb-space-sm">密码</label>
                <div className="relative">
                  <Icon name="lock" size={16} className="absolute left-3 top-1/2 -translate-y-1/2 text-outline" />
                  <input
                    id="login-ldap-password"
                    name="password"
                    autoComplete="current-password"
                    disabled={loading}
                    type={showPwd ? 'text' : 'password'}
                    value={ldapPassword}
                    onChange={(e) => setLDAPPassword(e.target.value)}
                    placeholder="••••••••"
                    className="w-full bg-surface-container-lowest border border-surface-container-highest rounded pl-9 pr-9 py-space-md text-body-md text-on-surface placeholder:text-text-3 focus:outline-none focus:border-primary/60 transition-colors"
                  />
                  <button
                    type="button"
                    onClick={() => setShowPwd((v) => !v)}
                    className="absolute right-3 top-1/2 -translate-y-1/2 text-outline hover:text-on-surface"
                    aria-label={showPwd ? '隐藏密码' : '显示密码'}
                    disabled={loading}
                  >
                    <Icon name={showPwd ? 'visibility_off' : 'visibility'} size={16} />
                  </button>
                </div>
              </div>

              {!ldapTenantOpen && (
                <button
                  type="button"
                  onClick={() => setLDAPTenantOpen(true)}
                  disabled={loading}
                  className="self-start text-label-sm text-primary hover:underline disabled:opacity-50"
                >
                  填写工作空间标识
                </button>
              )}
              {ldapTenantOpen && (
                <div>
                  <div className="flex items-center justify-between mb-space-sm">
                    <label htmlFor="login-ldap-tenant" className="block text-label-md text-on-surface-variant">工作空间标识</label>
                    {!ldapTenantRequired && (
                      <button
                        type="button"
                        onClick={() => setLDAPTenantOpen(false)}
                        disabled={loading}
                        className="text-label-sm text-outline hover:text-on-surface disabled:opacity-50"
                      >
                        收起
                      </button>
                    )}
                  </div>
                  <div className="relative">
                    <Icon name="domain" size={16} className="absolute left-3 top-1/2 -translate-y-1/2 text-outline" />
                    <input
                      id="login-ldap-tenant"
                      name="tenantSlug"
                      autoComplete="organization"
                      disabled={loading}
                      autoFocus
                      value={ldapTenantSlug}
                      onChange={(e) => setLDAPTenantSlug(e.target.value)}
                      placeholder="例如 acme"
                      spellCheck={false}
                      className="w-full bg-surface-container-lowest border border-surface-container-highest rounded pl-9 pr-3 py-space-md text-body-md text-on-surface placeholder:text-text-3 focus:outline-none focus:border-primary/60 transition-colors font-mono"
                    />
                  </div>
                  <p className="mt-space-sm text-label-sm text-outline">
                    {ldapTenantRequired
                      ? TENANT_REQUIRED_TEXT
                      : '工作空间标识由管理员提供，只能包含小写字母、数字和短横线；目录账号只在指定的工作空间内查找或开通。'}
                  </p>
                </div>
              )}
              <p className="text-label-sm text-outline">目录口令只用于本次认证，不会写入本地密码；目录账号与本地账号由管理员关联，未开通时请联系管理员。</p>
            </>
          )}

          {error && (
            <div role="alert" className="flex items-center gap-space-sm text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
              <Icon name="error" size={14} />
              {error}
            </div>
          )}

          {!ldapOpen && (
            <div className="space-y-space-sm text-body-sm">
              <label className="flex items-center gap-space-sm text-on-surface-variant cursor-pointer select-none">
                <input
                  type="checkbox"
                  name="remember"
                  disabled={loading}
                  checked={remember}
                  onChange={(e) => setRemember(e.target.checked)}
                  className="accent-primary"
                />
                保持登录（最长30天，受服务端配置限制）
              </label>
              <p className="text-label-sm text-outline">不勾选时使用会话 Cookie；浏览器恢复会话时可能保留 Cookie，关闭浏览器不保证注销，请主动退出登录。</p>
              <p className="text-label-sm text-outline">忘记密码请联系管理员。</p>
            </div>
          )}

          <Button type="submit" className="w-full justify-center" disabled={loading}>
            {loading ? '验证中...' : '登录'}
          </Button>

          {!ldapIsEnabled && (
            <>
              <div className="flex items-center gap-space-lg">
                <span className="flex-1 h-px bg-surface-container-highest" />
                <span className="text-label-sm text-outline">或</span>
                <span className="flex-1 h-px bg-surface-container-highest" />
              </div>

              <Button
                type="button"
                variant="secondary"
                className="w-full justify-center"
                disabled
              >
                <Icon name="domain" size={16} />
                LDAP 暂未配置
              </Button>
            </>
          )}
        </form>

        <p className="text-center text-label-sm text-outline mt-space-xl font-mono">
          AegisFlow · © 2026
        </p>
      </div>
    </div>
  )
}
