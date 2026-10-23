# AegisFlow Go 后端

Go 1.23+，Gin 1.11 + slog，PostgreSQL（pgx）+ Redis 共享限流，多应用实例部署，bcrypt，HS256 JWT。浏览器鉴权采用短期 Bearer access token + HttpOnly refresh Cookie；推荐前后端通过**同源代理**部署。构建、发布等业务接口及 LDAP 尚未实现。完整接口契约见 [openapi.yaml](openapi.yaml)。

## 本地启动（PowerShell）

在 backend 目录执行：

```powershell
go mod download
# 先创建独立数据库并启动 Redis；生产 URL 启用 TLS，凭据按 URL 编码。
$env:AEGIS_DATABASE_URL = 'postgres://aegis:CHANGE_ME@127.0.0.1:5432/aegisflow?sslmode=disable'
$env:AEGIS_REDIS_URL = 'redis://127.0.0.1:6379/0'
$env:AEGIS_REDIS_NAMESPACE = 'aegisflow'
$keyBytes = New-Object byte[] 48
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($keyBytes)
$rng.Dispose()
$env:AEGIS_JWT_SECRET = [Convert]::ToBase64String($keyBytes)
$env:AEGIS_BOOTSTRAP_USERNAME = 'admin'
# 密码为 12–72 UTF-8 字节，避免写入脚本或提交 Git。
$credential = Get-Credential -UserName 'admin' -Message '设置首次管理员密码'
$env:AEGIS_BOOTSTRAP_PASSWORD = $credential.GetNetworkCredential().Password
$env:AEGIS_ALLOWED_ORIGINS = 'http://localhost:5173'
$env:AEGIS_COOKIE_SECURE = 'false' # 本地 HTTP；生产 HTTPS 必须 true
# 后端仅读取环境变量，不自动加载 .env 文件。
go run ./cmd/server
```

默认监听 http://127.0.0.1:8080。PostgreSQL 数据库须预先创建，服务自动执行版本化表迁移；advisory 事务锁保证多实例并发启动安全。首次空库原子创建一位管理员；已有用户时不覆盖。初始化后移除 bootstrap 环境变量。JWT 密钥至少 32 字节，所有实例必须使用同一密钥、issuer/audience、Cookie 与来源配置；通过部署密钥管理保存并保持一致，重启无需粘性会话。

环境配置见 [.env.example](.env.example)。AEGIS_DATABASE_URL 和 AEGIS_REDIS_URL 均必填，无 SQLite/进程内限流自动回退；旧 AEGIS_DATABASE_PATH 会使启动失败。每个实例默认最多 20 个数据库连接、空闲 5 个，可通过 AEGIS_DB_MAX_OPEN/IDLE 调整，连接总数需按副本数控制。数据库和 Redis 应使用隔离网络、访问控制、TLS 与备份保护。

## 推荐部署：同源代理

浏览器只访问前端源，例如本地 `http://localhost:5173` 或生产 `https://aegis.example.com`，所有 `/api` 请求代理到后端。前端始终用相对 `/api/v1/...`，不要在浏览器中混用 localhost 与 127.0.0.1，也不要把生产 API 部署到不同站点再依赖跨站 Cookie。

本地 Vite 配置示例（供前端维护者使用；本次后端实现不会修改前端配置）：

```ts
server: {
  proxy: {
    '/api': { target: 'http://127.0.0.1:8080', changeOrigin: false },
  },
}
```

必须保留浏览器原始 `Origin`，且将**浏览器看到的前端源**加入 `AEGIS_ALLOWED_ORIGINS`。允许列表使用逗号分隔、精确匹配 scheme/host/port、无路径/尾斜杠/通配符。默认只允许 `http://localhost:5173`；使用 `http://127.0.0.1:5173` 需显式增加。同源请求仍必须带 Origin，代理不得删除或伪造它。

生产在 TLS 反向代理后运行，后端绑定回环或受控内部网络；**显式设 `AEGIS_COOKIE_SECURE=true`**。本地默认 false，服务不会根据 X-Forwarded-Proto、Host 等可伪造头推断 Secure，也没有隐含的“生产环境检测”。代理需要透传 Cookie/Set-Cookie/Origin，不能缓存 auth 响应。请勿用明文 HTTP 传输账号和令牌。

`SameSite=Strict` 拒绝跨站发送 Cookie；即使配置允许某源，`Sec-Fetch-Site: cross-site` 仍拒绝。CORS 仅兼容受控同站/调试源，不是跨站部署方案。Gin 默认不信任转发地址；AEGIS_TRUSTED_PROXIES 可配置实际入口代理的精确 CIDR，只在来自可信代理时解析 X-Forwarded-For，忽略 X-Real-IP。代理必须覆盖客户端传入的转发头，不可开放所有地址。登录使用 Redis Lua 原子共享限流，每 IP 固定一分钟最多 10 次，所有副本共享同一 AEGIS_REDIS_NAMESPACE；Redis 故障登录返回 503，不回退本地计数。

## HTTP 契约

成功：`{"data": ..., "requestId":"..."}`；失败：`{"error":{"code":"unauthorized","message":"..."},"requestId":"..."}`。所有响应有 `X-Request-ID`、`Cache-Control: no-store`。写请求需要 `Content-Type: application/json`，单个 JSON 对象，拒绝未知字段、null 字段、额外 JSON、数组、非对象和超过 16 KiB 的正文。

| 方法 | 路径 | 请求与权限 | 成功 data |
| --- | --- | --- | --- |
| GET | /healthz | 无鉴权 | `{status:"ok"}` |
| GET | /readyz | 无鉴权，PostgreSQL + Redis 检查 | `{status:"ready"}` |
| POST | /api/v1/auth/login | `{username,password,remember?:boolean}`；Origin 必需 | `{user,accessToken,expiresIn}` |
| POST | /api/v1/auth/refresh | **`{}`** + Cookie；Origin 必需 | `{user,accessToken,expiresIn}` |
| POST | /api/v1/auth/logout | **`{}`** + Cookie（可缺省）；Origin 必需 | `{loggedOut:true}` |
| GET | /api/v1/auth/me | `Authorization: Bearer <accessToken>` | User |
| GET | /api/v1/users | Bearer + admin | User[] |
| POST | /api/v1/users | Bearer + admin；`{username,password,displayName?,role}` | User，201 |

**所有 auth POST（包括 login）强制恰好一个 Origin，且精确匹配允许列表**；缺失、不匹配、`null` 或跨站请求返回 `403 origin_denied`，在登录、轮换和退出之前拦截。非浏览器 curl/脚本也必须带该 Origin。其他请求如果携带 Origin 也须符合允许列表。全局拒绝 `Sec-Fetch-Site: cross-site`。允许来源响应包含 `Access-Control-Allow-Origin: <原始允许源>`、`Access-Control-Allow-Credentials: true`，`Vary: Origin, Sec-Fetch-Site`；OPTIONS 额外 Vary 请求方法与请求头，允许 GET/POST/OPTIONS 和 Authorization/Content-Type。

### Cookie 与前端对齐

- 名称 `aegis_refresh`，host-only（无 Domain），`Path=/api/v1/auth`，`HttpOnly`，`SameSite=Strict`；Secure 完全由 `AEGIS_COOKIE_SECURE` 显式布尔 `true/false` 控制，其他值启动失败。
- `remember` 可省略，默认 false；false 签发 session cookie，不含 Max-Age/Expires。true 签发持久 Cookie，Max-Age/Expires 为服务端绝对会话截止时间的剩余寿命（默认 30 天）。
- refresh 每次替换 Cookie，但**保留原 remember 与绝对截止时间**，不重置成新的 30 天。服务端记住期限与模式；客户端不能通过 refresh 请求修改。
- login/refresh 的 JSON **恰好只有 `user/accessToken/expiresIn`**，绝不返回 refreshToken，也不存在 JSON refreshToken 请求降级；发送该字段返回 400。expiresIn 为 access token 剩余秒数，默认 15 分钟，受绝对会话期限限制。
- refresh 缺 Cookie、值非法、过期、已撤销或重放：401 并删除 Cookie。logout 对不存在、未知或已撤销 Cookie 幂等返回 200，同时清 Cookie；已知 Cookie（包括已轮换的历史值）撤销整个会话，使其 access token 立即失效。Cookie 删除保持原 Path/HttpOnly/SameSite/Secure，Max-Age=0。数据库撤销失败返回 500 时不删除 Cookie，保留重试撤销凭据。
- `/auth/me` 和 users **仅接受 Bearer**，Cookie 不授予这些接口权限。刷新与退出不需要 Bearer。退出时若 Cookie 缺失就无法标识并撤销其他会话，但仍清 Cookie 并成功。
- access token 建议只保存在内存；Cookie 由浏览器自动处理，不能读取、不能写 localStorage/sessionStorage。浏览器请求可显式 `credentials: 'include'`，即使同源默认也会携带 Cookie。刷新必须串行化（多标签页也协调）；使用旧 Cookie 重放会撤销整个会话。
- **HttpOnly session cookie 不保证浏览器关闭即注销**：浏览器会话恢复可能恢复 session cookie。remember=false 不应宣传为可靠“关闭浏览器退出”；要立即注销必须调用 logout，服务端绝对期限始终有效。

浏览器调用示例（使用同源代理，无需也不要从 JavaScript 手写 Origin）：

```js
const response = await fetch('/api/v1/auth/login', {
  method: 'POST', credentials: 'include',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ username, password, remember: false }),
});
const { data } = await response.json(); // user, accessToken, expiresIn
await fetch('/api/v1/auth/me', {
  headers: { Authorization: `Bearer ${data.accessToken}` },
});
// 刷新/退出都是 JSON 空对象；浏览器自动携带 Cookie。
await fetch('/api/v1/auth/refresh', {
  method: 'POST', credentials: 'include',
  headers: { 'Content-Type': 'application/json' }, body: '{}',
});
```

PowerShell Cookie 容器调用示例：

```powershell
$origin = @{ Origin = 'http://localhost:5173' }
$body = @{ username = 'admin'; password = $credential.GetNetworkCredential().Password; remember = $false } | ConvertTo-Json
$login = Invoke-RestMethod 'http://127.0.0.1:8080/api/v1/auth/login' -Method Post -Headers $origin -ContentType 'application/json' -Body $body -SessionVariable browser
Invoke-RestMethod 'http://127.0.0.1:8080/api/v1/auth/me' -Headers @{ Authorization = "Bearer $($login.data.accessToken)" }
Invoke-RestMethod 'http://127.0.0.1:8080/api/v1/auth/refresh' -Method Post -Headers $origin -ContentType 'application/json' -Body '{}' -WebSession $browser
Invoke-RestMethod 'http://127.0.0.1:8080/api/v1/auth/logout' -Method Post -Headers $origin -ContentType 'application/json' -Body '{}' -WebSession $browser
```

User 字段为 `id/username/displayName/role`，无密码或哈希。用户名为 3–64 个 ASCII 字母、数字、下划线、点、短横线，首字符须为字母或数字；匹配不区分大小写。密码为 12–72 UTF-8 字节；displayName 最多 256 字节，省略/空串回退 username。角色为 admin/member/viewer；只有 admin 能创建/列出用户，其他角色可读自己的资料。当前没有公共注册接口。

常见错误：400 `invalid_request`/`validation_failed`；401 `unauthorized`；403 `origin_denied`/`forbidden`；409 `conflict`；415 `unsupported_media_type`；429 `rate_limited`（Retry-After 为窗口剩余秒数，向上取整）；503 `not_ready`/`dependencies_unavailable`；500 `internal_error`。未知路径、错误方法和额外尾斜杠均为统一 404 `not_found`，不自动重定向。Origin/CSRF 拦截优先于正文与身份验证。

## 会话、迁移与运行边界

- JWT 验证 HS256、issuer、audience、到期时间、PostgreSQL 会话状态和当前数据库用户角色；auth 核心继续使用标准 context.Context，可直接用于 Go 测试/非 HTTP 调用。`Login` 默认非持久会话，`LoginWithRemember` 显式控制 remember，`Tokens.RefreshToken/Remember/SessionExpiresAt` 均为 JSON 隐藏字段。
- Refresh 为密码学随机值，只存 SHA-256 哈希，事务性单次消费和轮换；历史令牌用于重放检测、撤销。refreshTTL 控制**绝对会话期限**，不是滑动窗口。
- PostgreSQL 版本化 schema_migrations 在 advisory 事务锁下执行，未知更高版本或缺号会拒绝启动。首管理员初始化独立加锁，多个实例同时启动也只创建一位管理员。
- Refresh/Logout 统一锁定 session 行，再读取 token 状态；同一会话跨多个实例串行化刷新与撤销，重放撤销会提交到共享数据库。数据库事务使用 READ COMMITTED。
- SQLite 开发库不会自动导入 PostgreSQL。现阶段开发数据可重新初始化；如有需要保留的用户，必须先备份并单独编写审核过的离线迁移，保留 bcrypt 哈希、校验用户名大小写唯一性并撤销旧会话，不能直接复制文件或将 SQLite 挂共享盘。
- /readyz 同时检查 PostgreSQL 和 Redis；Redis 故障使就绪返回 503，登录也 503。已存在 Bearer 会话在 PostgreSQL 正常时仍可鉴权；负载均衡是否摘除整个实例取决于运维策略。
- 请求 ID、slog JSON 访问日志、鉴权操作日志、panic 恢复、安全响应头、HTTP 超时、信号触发优雅退出；日志不记录密码、Cookie、令牌、正文。审计输出为日志，不是持久化审计查询服务。

## 验证

```powershell
# 测试库必须可创建/删除隔离 schema，不要指向生产库。
$env:AEGIS_TEST_DATABASE_URL = 'postgres://test:password@localhost:5432/aegis_test?sslmode=disable'
$env:AEGIS_TEST_REDIS_URL = 'redis://localhost:6379/1'
go test ./...
go vet ./...
go build ./cmd/server
# 安装 C 编译器并支持 race detector 时可额外：
go test -race ./...
```

分层测试覆盖 auth 核心（持久化、并发轮换/重放撤销、remember、绝对期限、版本迁移和多连接池会话互通）、HTTP（Cookie flags、Secure 显式配置、默认/session/持久模式、轮换、幂等退出、缺 Cookie、JSON 降级拒绝、CSRF、credentialed CORS、Bearer/RBAC、严格 JSON、限流/错误封装）以及真实 httptest.Server + CookieJar 登录→刷新→退出链路。

## 架构

cmd/server 负责配置、PostgreSQL 连接池、Redis、迁移与标准库 http.Server 生命周期；internal/config 校验连接 URL、连接数与可信代理；internal/auth 负责用户/密码/会话/JWT；internal/ratelimit 负责 Redis 共享限流；internal/httpapi 负责 Gin 路由与 Cookie、Origin、CORS、Bearer/RBAC。JSON 解码按请求校验，不更改 Gin 全局 binding。Gin 1.11 与 Go 1.23 兼容。

本地双实例样例见 [compose.yaml](../compose.yaml) 与 [部署说明](../deploy/README.md)。应用副本可横向扩展，但数据库、Redis、入口网关的高可用需另行部署；样例单节点依赖不等于整套服务高可用。数据库集成测试未配置 URL 会明确 Skip，不能将跳过当作生产验证通过。
