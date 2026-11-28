import { execFile } from 'node:child_process'
import path from 'node:path'
import { promisify } from 'node:util'

/**
 * 双租户 E2E（阶段 F3）的租户运维 CLI 驱动器。
 *
 * 租户开通与管理员创建没有 HTTP API（只走 `cmd/tenant`，这是设计边界），因此
 * 浏览器测试必须调用真实 CLI。本模块用 Node `child_process.execFile`（绝不拼
 * shell 字符串，Windows 与 Linux 行为一致）在 `backend/` 下执行
 * `go run ./cmd/tenant ...`：
 *
 * - 数据库复用 E2E 的一次性库：把 `AEGIS_E2E_DATABASE_URL` 映射为 CLI 需要的
 *   `AEGIS_DATABASE_URL`；
 * - GOMODCACHE/GOCACHE/GOTMPDIR/GOFLAGS/GOPROXY/CGO_ENABLED/CC 等从
 *   `process.env` 原样继承（本地离线缓存与 CI 的 go 环境都靠它）；
 * - CLI 未给 `--password` 时会生成一次性口令并**只写 stdout**，这里捕获并返回，
 *   不写日志；失败时只返回 stderr，避免把口令带进错误信息。
 *
 * 测试进程的 cwd 是仓库根目录（Playwright 配置所在目录），可用
 * `AEGIS_E2E_BACKEND_DIR` 覆盖。
 */

const execFileAsync = promisify(execFile)

/** CLI 一次调用的超时：首次 `go run` 需要编译，之后走缓存。 */
const CLI_TIMEOUT_MS = 180_000

export interface TenantRow {
  id: string
  slug: string
  name: string
  status: string
  users: number
  createdAt: string
}

export interface CreatedAdmin {
  tenant: string
  username: string
  email: string
  /** 仅在未显式传 --password 时存在；CLI 只在 stdout 打印一次。 */
  password: string
}

export interface CreatedTenant {
  id: string
  slug: string
  name: string
  status: string
}

function backendDir(): string {
  return process.env.AEGIS_E2E_BACKEND_DIR ?? path.join(process.cwd(), 'backend')
}

/**
 * 子进程环境：CLI 只需要 AEGIS_DATABASE_URL；同时补齐既有的 JWT/密钥测试
 * env（与 E2E 后端一致的一次性测试占位值），并显式移除会让 CLI 拒绝启动的
 * AEGIS_DATABASE_PATH。其余变量（Go 缓存、代理等）原样继承。
 */
function cliEnv(): NodeJS.ProcessEnv {
  const databaseURL = process.env.AEGIS_E2E_DATABASE_URL
  if (!databaseURL) {
    throw new Error('AEGIS_E2E_DATABASE_URL is required to drive the tenant CLI in E2E')
  }
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    AEGIS_DATABASE_URL: databaseURL,
    // The CLI itself only reads AEGIS_DATABASE_URL; these mirror the existing
    // disposable E2E backend configuration and are never production values.
    AEGIS_JWT_SECRET: process.env.AEGIS_E2E_JWT_SECRET ?? 'e2e-tenant-cli-jwt-secret-at-least-32-bytes',
    AEGIS_SECRET_KEY: process.env.AEGIS_E2E_SECRET_KEY ?? 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=',
  }
  delete env.AEGIS_DATABASE_PATH
  return env
}

/** Runs one real CLI command and returns its stdout. Never echoes stdout on failure. */
async function runTenantCLI(args: string[]): Promise<string> {
  try {
    const { stdout } = await execFileAsync('go', ['run', './cmd/tenant', ...args], {
      cwd: backendDir(),
      env: cliEnv(),
      timeout: CLI_TIMEOUT_MS,
      windowsHide: true,
      maxBuffer: 4 * 1024 * 1024,
    })
    return stdout
  } catch (error) {
    const failure = error as { stderr?: string; code?: number | string; signal?: string; message?: string }
    const detail = (failure.stderr ?? '').trim() || failure.message || 'unknown failure'
    throw new Error(`tenant CLI ${args.join(' ')} failed (code=${failure.code ?? 'unknown'}): ${detail}`)
  }
}

/** 校验 CLI 输出中的 slug/标识符，避免把意外文本带进断言或日志。 */
const SAFE_VALUE = /^[A-Za-z0-9@._:%-]{1,320}$/

function matchValue(output: string, pattern: RegExp, label: string): string {
  const match = output.match(pattern)
  if (!match) {
    throw new Error(`tenant CLI output is missing ${label}`)
  }
  return match[1]
}

/**
 * `tenant create --slug <slug> --name <name> --admin-email <email>`：创建租户并
 * 同时创建首位管理员。返回租户信息、管理员用户名与一次性初始口令（仅来自
 * stdout）。密码只存在于本次 E2E 运行内存中。
 */
export async function createTenantWithAdmin(input: {
  slug: string
  name: string
  adminEmail: string
}): Promise<{ tenant: CreatedTenant; admin: CreatedAdmin }> {
  const output = await runTenantCLI([
    'create',
    '--slug',
    input.slug,
    '--name',
    input.name,
    '--admin-email',
    input.adminEmail,
  ])
  const tenant: CreatedTenant = {
    id: matchValue(output, /^created tenant: id=(\S+) slug=\S+ name="[^"]*" status=\S+$/m, 'tenant id'),
    slug: matchValue(output, /^created tenant: id=\S+ slug=(\S+) name="[^"]*" status=\S+$/m, 'tenant slug'),
    name: matchValue(output, /^created tenant: id=\S+ slug=\S+ name="([^"]*)" status=\S+$/m, 'tenant name'),
    status: matchValue(output, /^created tenant: id=\S+ slug=\S+ name="[^"]*" status=(\S+)$/m, 'tenant status'),
  }
  if (tenant.slug !== input.slug) {
    throw new Error(`tenant CLI created slug ${tenant.slug}, expected ${input.slug}`)
  }
  const admin = parseCreatedAdmin(output)
  if (admin.email !== input.adminEmail) {
    throw new Error('tenant CLI created an administrator with an unexpected email')
  }
  return { tenant, admin }
}

/**
 * `tenant create-admin --tenant <slug> --email <email>`：在既有租户创建管理员。
 * 用户名由 email local part 派生（冲突自动加 -2/-3 后缀），口令同样从 stdout
 * 捕获。用于「B 租户里存在与 A 管理员相同登录标识」的歧义场景。
 */
export async function createAdminInTenant(tenantSlug: string, email: string): Promise<CreatedAdmin> {
  const output = await runTenantCLI(['create-admin', '--tenant', tenantSlug, '--email', email])
  const admin = parseCreatedAdmin(output)
  if (admin.tenant !== tenantSlug || admin.email !== email) {
    throw new Error('tenant CLI created an administrator for an unexpected tenant/email')
  }
  return admin
}

/** Parses the shared "created administrator" + one-time password lines. */
function parseCreatedAdmin(output: string): CreatedAdmin {
  const tenant = matchValue(output, /^created administrator: tenant=(\S+) username=\S+ email=\S+$/m, 'administrator tenant')
  const username = matchValue(output, /^created administrator: tenant=\S+ username=(\S+) email=\S+$/m, 'administrator username')
  const email = matchValue(output, /^created administrator: tenant=\S+ username=\S+ email=(\S+)$/m, 'administrator email')
  const password = matchValue(
    output,
    /^initial password \(shown only once; store it securely\): (\S+)$/m,
    'one-time initial password',
  )
  if (!SAFE_VALUE.test(username) || !SAFE_VALUE.test(email) || !/^[A-Za-z0-9_-]{12,128}$/.test(password)) {
    throw new Error('tenant CLI output carried an unexpected administrator identity')
  }
  return { tenant, username, email, password }
}

/**
 * `tenant list`：解析 tabwriter 表格（列为 ID/SLUG/NAME/STATUS/USERS/
 * CREATED_AT，用 2+ 空格分隔）。用于断言 CLI 与部署视角一致。
 */
export async function listTenants(): Promise<TenantRow[]> {
  const output = await runTenantCLI(['list'])
  const lines = output
    .split(/\r?\n/)
    .map((line) => line.trimEnd())
    .filter((line) => line.trim() !== '')
  if (!lines[0]?.startsWith('ID')) {
    throw new Error('tenant CLI list output is missing the header row')
  }
  return lines.slice(1).map((line) => {
    const columns = line.trim().split(/\s{2,}/)
    if (columns.length < 6) {
      throw new Error('tenant CLI list row is malformed')
    }
    const users = Number.parseInt(columns[4], 10)
    if (!Number.isFinite(users)) {
      throw new Error('tenant CLI list carried an invalid user count')
    }
    return { id: columns[0], slug: columns[1], name: columns[2], status: columns[3], users, createdAt: columns[5] }
  })
}
