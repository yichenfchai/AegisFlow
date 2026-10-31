# AegisFlow Go 后端

Go 1.23+，Gin 1.11 + slog，PostgreSQL（pgx）+ Redis 共享限流，多应用实例部署，bcrypt，HS256 JWT。浏览器鉴权采用短期 Bearer access token + HttpOnly refresh Cookie；推荐前后端通过**同源代理**部署。项目注册表与最小构建闭环（触发、列表、详情、取消、游标日志）已实现；部署、审批等其余业务接口及 LDAP 尚未实现。完整接口契约见 [openapi.yaml](openapi.yaml)。

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

环境配置见 [.env.example](.env.example)。AEGIS_DATABASE_URL 和 AEGIS_REDIS_URL 均必填，无 SQLite/进程内限流自动回退；旧 AEGIS_DATABASE_PATH 会使启动失败。每个实例默认最多 20 个数据库连接、空闲 5 个，可通过 AEGIS_DB_MAX_OPEN/IDLE 调整，连接总数需按副本数控制。AEGIS_BUILD_TIMEOUT 是单次构建的执行上限（默认 10m，最大 24h）；AEGIS_LOGIN_RATE_LIMIT 是每 IP 每分钟的登录配额（默认 10，取值 1-1000，仅供测试环境在单地址多账号时放宽）。**AEGIS_SECRET_KEY** 是部署目标凭据的 AES-256-GCM 密钥（base64 恰好 32 字节；留空表示禁用凭据存储，无凭据目标仍可用，写入凭据返回 400）；**AEGIS_DEPLOY_CHECK_TIMEOUT** 是单次部署目标连通性探测的上限（默认 5s，最大 1m）。**AEGIS_BUILD_SANDBOX** 选择构建执行方式：`none`（默认，开发用）以服务账号执行并在启动时打印警告；`container` 为每个构建启动一次性容器，配合 `AEGIS_BUILD_IMAGE`（默认 `alpine:3.20`）、`AEGIS_BUILD_CPU`（默认 2）、`AEGIS_BUILD_MEMORY`（默认 2g）、`AEGIS_BUILD_PIDS`（默认 512）使用，启动时探测容器运行时，不可用即拒绝启动；`user` 在 Linux 上通过 `setpriv` 降权到 `AEGIS_BUILD_USER` 指定的专用账号，服务必须以 root 启动，否则启动失败（Windows 不支持该模式）。**AEGIS_BUILD_DISK**（默认 2g，范围 16MiB–1TiB）限制单次构建的工作区大小；**AEGIS_BUILD_CLONE**（默认 false）开启仓库 clone，**AEGIS_BUILD_ALLOW_LOCAL_REPO**（默认 false）额外允许 `file:///` 与绝对本地路径。数据库和 Redis 应使用隔离网络、访问控制、TLS 与备份保护。

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

生产在 TLS 反向代理后运行，后端绑定回环或受控内部网络；**显式设 `AEGIS_COOKIE_SECURE=true`**。本地默认 false，服务不会根据 X-Forwarded-Proto、Host 等可伪造头推断 Secure，也没有隐含的“生产环境检测”。代理需要透传 Cookie/Set-Cookie/Origin，不能缓存 auth 响应。请勿用明文 HTTP 传输账号和令牌。完整的入口代理、证书与 SSE 超时示例见 [deploy/nginx-tls.example.conf](../deploy/nginx-tls.example.conf)，运维步骤（TLS、备份恢复、滚动升级）见 [部署说明](../deploy/README.md)。

`SameSite=Strict` 拒绝跨站发送 Cookie；即使配置允许某源，`Sec-Fetch-Site: cross-site` 仍拒绝。CORS 仅兼容受控同站/调试源，不是跨站部署方案。Gin 默认不信任转发地址；AEGIS_TRUSTED_PROXIES 可配置实际入口代理的精确 CIDR，只在来自可信代理时解析 X-Forwarded-For，忽略 X-Real-IP。代理必须覆盖客户端传入的转发头，不可开放所有地址。登录使用 Redis Lua 原子共享限流，每 IP 固定一分钟最多 10 次（AEGIS_LOGIN_RATE_LIMIT 可调），所有副本共享同一 AEGIS_REDIS_NAMESPACE；Redis 故障登录返回 503，不回退本地计数。

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
| POST | /api/v1/auth/password | Bearer；`{currentPassword,newPassword}`；Origin 必需 | `{changed:true}`，撤销其他会话 |
| GET | /api/v1/auth/sessions | Bearer | SessionInfo[]（标记当前会话，不含令牌） |
| DELETE | /api/v1/auth/sessions | Bearer | `{revoked:N}`（撤销除当前外全部） |
| DELETE | /api/v1/auth/sessions/{id} | Bearer，仅自己的会话；未知 404 | `{revoked:true}` |
| GET | /api/v1/users | Bearer + admin；`?limit=&offset=`（1-100，默认 20） | `{items,total,limit,offset}` |
| POST | /api/v1/users | Bearer + admin；`{username,password,displayName?,role}` | User，201 |
| PATCH | /api/v1/users/{id} | Bearer + admin；`{role?,disabled?}` 至少一项 | `{updated:true}` |
| POST | /api/v1/users/{id}/password-reset | Bearer + admin；`{password}` | `{reset:true}`，撤销目标全部会话 |
| GET | /api/v1/audit | Bearer + admin；`?limit=&offset=` | `{items,total,limit,offset}`，最新在前 |
| GET | /api/v1/projects | Bearer（所有角色）；`?limit=&offset=&status=active\|archived` | `{items,total,limit,offset}`，按名称排序，仅列调用者可见项目 |
| POST | /api/v1/projects | Bearer + admin/member；`{name,description?,repoUrl?,defaultBranch?,buildCommand?,visibility?}` | Project，201；创建者自动成为 maintainer |
| GET | /api/v1/projects/{id} | Bearer（可见者） | Project；私有项目对非成员 404 |
| PATCH | /api/v1/projects/{id} | Bearer + admin/member；`{name?,description?,repoUrl?,defaultBranch?,buildCommand?,visibility?,archived?}` 至少一项 | Project；归档项目只接受恢复；不可见项目 404 |
| POST | /api/v1/projects/{id}/builds | Bearer + admin/member；`{branch?}`；项目须活跃且已配置 buildCommand | Build，201（queued）；不可见项目 404 |
| GET | /api/v1/projects/{id}/members | Bearer（可见者） | ProjectMember[]，maintainer 在前 |
| POST | /api/v1/projects/{id}/members | Bearer + admin/member；`{userId,role}` | ProjectMember，201；重复成员 409，账号不存在 404 |
| PATCH | /api/v1/projects/{id}/members/{userId} | Bearer + admin/member；`{role}` | ProjectMember；无该成员 404 |
| DELETE | /api/v1/projects/{id}/members/{userId} | Bearer + admin/member | `{removed:true}`；创建者不可移除（400） |
| GET | /api/v1/builds | Bearer（所有角色）；`?limit=&offset=&projectId=` | `{items,total,limit,offset}`，最新在前，仅含可见项目的构建 |
| GET | /api/v1/builds/{id} | Bearer（可见者） | Build；不可见项目的构建 404 |
| GET | /api/v1/builds/{id}/logs | Bearer（可见者）；`?after=&limit=`（after 为事件 id 游标，limit 1-1000，默认 200） | `{items}`，按事件 id 升序（SSE 的回退通道）；不可见 404 |
| GET | /api/v1/builds/{id}/events | Bearer（所有角色）；`?after=` 或 `Last-Event-ID` 续传 | `text/event-stream`：`status` / `logs` / `done` 帧 + `: ping` 心跳 |
| POST | /api/v1/builds/{id}/cancel | Bearer + admin/member；**`{}`** | Build；重复取消幂等 200，终态构建 409 |
| POST | /api/v1/builds/{id}/retry | Bearer + admin/member；**`{}`** | 新 Build（201，queued）；沿用原构建的分支、使用项目**当前**命令；排队/运行中的构建 409 |

**所有 auth POST（包括 login）强制恰好一个 Origin，且精确匹配允许列表**；缺失、不匹配、`null` 或跨站请求返回 `403 origin_denied`，在登录、轮换和退出之前拦截。非浏览器 curl/脚本也必须带该 Origin。其他请求如果携带 Origin 也须符合允许列表。全局拒绝 `Sec-Fetch-Site: cross-site`。允许来源响应包含 `Access-Control-Allow-Origin: <原始允许源>`、`Access-Control-Allow-Credentials: true`，`Vary: Origin, Sec-Fetch-Site`；OPTIONS 额外 Vary 请求方法与请求头，允许 GET/POST/OPTIONS 和 Authorization/Content-Type。

### 项目可见性与成员（P1 + P2）

- `projects.visibility` 为 `public`（默认）或 `private`。全局 admin 永远可见；其他角色可见 public 项目，以及自己创建或被加入的项目（`project_members`）。
- 可见性在服务端逐请求解析：项目列表、项目详情、构建列表/详情/日志/SSE、成员列表、触发构建与改项目都带同一条可见性谓词。**不可见一律 404**（而非 403），避免用私有 id 枚举项目。
- **写权限叠加（P2）**：全局角色是上限（viewer 永不可写、admin 永远可写）；private 项目另外要求调用者是该项目 `maintainer`，否则 **403**（可见但无权）。public 项目保持「任意 member 可写」。判定集中在 `projects.AuthorizeWrite`，项目修改、归档、触发/取消/重试构建、成员增删改都复用它；构建侧在自己的事务里传入同一个 `*sql.Tx`。
- 成员变更与状态同事务写审计：`project.member_add` / `project.member_update` / `project.member_remove`（objectType `project`，detail 只写用户名与角色）。
- 创建者在同一事务内写入 maintainer 成员行，语义上是 owner：不能被移除（400）。成员目标是真实账号（不存在 404），重复添加 409。
- 迁移 v5 为存量项目补写创建者的 maintainer 行，并把 `visibility` 默认为 `public`，因此升级后行为不变。
- 已知的决策待确认点：public 项目的成员管理目前允许任意 admin/member（P1 行为），设计稿矩阵写作「member 仅成员列表」；若按后者收紧，只需调整 `AuthorizeWrite` 的一个分支。

### 构建执行（P3 第一批）

- 状态机六态：`queued → running → succeeded/failed/cancelled`，只有持有 worker 租约的实例在启动后把遗留的 `running` 统一转为 `interrupted`。`interrupted` 表示进程被打断而不是业务失败，需要重新触发。租约是 PostgreSQL 会话级 advisory lock（按 database + schema 隔离）：同一部署只有一个副本执行与恢复，其余副本待命并在下一重试周期接管；连接或进程消失时锁自动释放。它保证"同一时刻一个 worker"，不提供多 worker 并行吞吐。
- 触发时固化命令、分支、仓库地址（`repoUrl`）与触发人快照，构建号在每个项目内自增；之后修改项目不影响已触发的构建。命令在系统临时目录执行（Unix `sh -c`、Windows `cmd /c`），结束后删除目录。目录显式以 0700 创建；构建命令只获得显式白名单环境变量（`PATH`、`HOME`、`TMPDIR`/`TMP`/`TEMP`、`LANG` 等，Windows 另加 `SYSTEMROOT` 等系统变量），**不继承** `AEGIS_*` 服务凭据。
- 仓库 clone（`AEGIS_BUILD_CLONE=true` 且构建有 `repoUrl`）：开始时用 `git clone --depth 1 --single-branch --branch <构建分支>` 把仓库拉到 `repo/`，命令在该目录执行；clone 在宿主侧完成（容器模式容器无网络），并沿用同一执行身份（`user` 模式下即为构建账号）。只支持无凭据的 https / `git@` 地址；含内联凭据的 URL 直接失败，私有仓库凭据本批不支持；`file:///` 与绝对路径仅在 `AEGIS_BUILD_ALLOW_LOCAL_REPO=true` 时允许。clone 失败写一条脱敏后的 `git clone failed: ...` system 日志并把构建置 `failed`。未开启 clone 时若项目配了 `repoUrl`，会显式记录"cloning is disabled"并在空工作区执行。
- 每个构建工作区受 `AEGIS_BUILD_DISK` 限制：worker 每 2 秒统计 `repo/` 与生成文件的大小，超限即取消构建（含容器）并写 system 日志置 `failed`。这是尽力而为的看门狗，不是文件系统配额，超限时最多会越过上限一个轮询周期的写入量。
- worker 随服务进程启动、单并发运行；持有租约的副本用 `SELECT ... FOR UPDATE SKIP LOCKED` 领取任务，`AEGIS_BUILD_TIMEOUT` 到期或收到取消请求时终止子进程。取消通过 300ms 状态看门狗传播到正在运行的命令。待命副本不领取、不恢复，只定期重试租约。
- 日志按行写入 `build_log_events`，批量 500ms 落库；单行超过 4096 字节按 rune 边界截断，单构建最多记录 5000 条事件，超限后追加一条 system 截断说明。读取有两条通道：`GET /builds/{id}/events` 的 SSE 推送（`status`/`logs`/`done` 帧，`Last-Event-ID` 或 `after` 续传，15s 心跳，终态后等日志静默 3s 再结束，单连接上限 10 分钟后发 `reconnect`），以及保留的 `after` 游标轮询作为回退。SSE 由前端用 `fetch` 流式读取并携带 Bearer——`EventSource` 无法设置 Authorization 头，token 不放进 URL。
- 执行隔离由 `AEGIS_BUILD_SANDBOX` 决定。`container` 模式下每个构建是一个一次性容器：`--network none`、`--read-only` 根文件系统、`--cap-drop ALL`、`no-new-privileges`，只把构建目录挂到 `/workspace`，CPU/内存/进程数由 `--cpus`/`--memory`/`--pids-limit` 限制；超时、取消或服务退出时显式 `docker kill` 容器，避免留下孤儿进程。`user` 模式在 Linux 上把 clone 与命令都经 `setpriv` 降权到 `AEGIS_BUILD_USER`，工作区 chown 给该账号，启动时校验 root、账号存在与 `setpriv` 可用，否则拒绝启动；Windows 不支持该模式。`none` 模式仍以服务账号执行，只用于开发。所有模式都只注入最小环境变量白名单，不继承 `AEGIS_*` 服务凭据。
- 输出脱敏已实现：stdout/stderr 在落库前按模式替换（`AEGIS_*=...`、`Bearer ...`、带内联凭据的 `postgres://`/`redis://` URL、`ghp_`/`github_pat_`/`glpat-`/`AKIA`/`xox[baprs]-`/`sk-` 前缀、JWT、PEM 私钥头）为 `[redacted]`；构建结束时追加一条 system 统计行（只记数量）。命中但无法确认移除的行整行丢弃并计数（fail closed）。多行私钥正文与自由文本口令不覆盖。
- 本批边界（仍不做）：私有仓库凭据（部署密钥 / token）、构建阶段与产物、构建缓存、多 worker 并行吞吐。`container` 与 `user` 模式已提供隔离和资源上限；`none` 模式仅用于开发。

### 部署目标（P4 第一批）

- `deployment_targets` 是全局管理对象，`/api/v1/deployment-targets` 全部路由仅 admin 可用；member/viewer 返回 403。支持 HTTP(S) 目标的新增、查询、分页、修改、删除与连通性探测，`kind` 目前只有 `http`。
- 目标字段：`name`（3–64 字节、单行、大小写不敏感唯一）、`environment`（`development`/`staging`/`production`，默认 development）、`endpoint`（http/https，禁止 userinfo、query、fragment）、`healthPath`（默认 `/`，必须以 `/` 开头）、`status`（`active`/`disabled`）。
- 凭据加密存储：`AEGIS_SECRET_KEY`（base64 32 字节）启用 AES-256-GCM；密文布局为 `[12 字节随机 nonce][ciphertext+tag]`，AAD 绑定目标 id（`deployment_target:<id>`），因此密文不能跨行复用。接口只返回 `hasSecret`，从不返回明文或密文；未配置密钥时仍可创建无凭据目标，但写入凭据返回 400。带凭据的目标必须使用 `https` 端点（仅回环地址允许 `http`），否则创建/更新直接 400，避免把 Bearer 凭据以明文发到网络上。
- 探测：`POST /api/v1/deployment-targets/{id}/check` 对 `endpoint + healthPath` 发 GET，超时由 `AEGIS_DEPLOY_CHECK_TIMEOUT`（默认 5s，最大 1m）控制；有凭据时发送 `Authorization: Bearer <secret>`（仅在本次探测的内存中解密）。2xx 记为健康，其余记录 `http <code>`、`timeout` 或脱敏后的网络错误；结果写入 `last_check_at/last_check_ok/last_check_detail`。默认客户端最多 3 跳、不跨主机，并拒绝 `https`→`http` 降级。
- 审计：创建、修改、删除、探测分别写 `deployment_target.create|update|remove|check`（objectType `deployment_target`），与状态变更同事务；detail 只含名称或探测结果，绝不含凭据。
- 本批边界（仍不做）：部署任务、回滚、审批流（P4.2/P4.3）、独立密钥管理页面、部署策略与业务审计保留策略；删除为硬删除（尚无任务引用目标）。

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

User 字段为 `id/username/displayName/role/disabled/createdAt`，无密码或哈希。用户名为 3–64 个 ASCII 字母、数字、下划线、点、短横线，首字符须为字母或数字；匹配不区分大小写。密码为 12–72 UTF-8 字节；displayName 最多 256 字节，省略/空串回退 username。角色为 admin/member/viewer；只有 admin 能管理用户与查询审计，所有角色可修改自己的密码并管理自己的会话。禁用账号会立即撤销其全部会话，且只有密码正确时登录才返回 403 `account_disabled`，不会向无凭据者泄露禁用状态。禁用或降级最后一个可用管理员返回 400，角色变更对既有会话即时生效（每次请求读取数据库角色）。当前没有公共注册接口。

常见错误：400 `invalid_request`/`validation_failed`；401 `unauthorized`；403 `origin_denied`/`forbidden`；409 `conflict`；415 `unsupported_media_type`；429 `rate_limited`（Retry-After 为窗口剩余秒数，向上取整）；503 `not_ready`/`dependencies_unavailable`；500 `internal_error`。未知路径、错误方法和额外尾斜杠均为统一 404 `not_found`，不自动重定向。Origin/CSRF 拦截优先于正文与身份验证。

## 会话、迁移与运行边界

- JWT 验证 HS256、issuer、audience、到期时间、PostgreSQL 会话状态和当前数据库用户角色；auth 核心继续使用标准 context.Context，可直接用于 Go 测试/非 HTTP 调用。`Login` 默认非持久会话，`LoginWithRemember` 显式控制 remember，`Tokens.RefreshToken/Remember/SessionExpiresAt` 均为 JSON 隐藏字段。
- Refresh 为密码学随机值，只存 SHA-256 哈希，事务性单次消费和轮换；历史令牌用于重放检测、撤销。refreshTTL 控制**绝对会话期限**，不是滑动窗口。
- PostgreSQL 版本化 schema_migrations 在 advisory 事务锁下执行，未知更高版本或缺号会拒绝启动。首管理员初始化独立加锁，多个实例同时启动也只创建一位管理员。
- Refresh/Logout 统一锁定 session 行，再读取 token 状态；同一会话跨多个实例串行化刷新与撤销，重放撤销会提交到共享数据库。数据库事务使用 READ COMMITTED。
- SQLite 开发库不会自动导入 PostgreSQL。现阶段开发数据可重新初始化；如有需要保留的用户，必须先备份并单独编写审核过的离线迁移，保留 bcrypt 哈希、校验用户名大小写唯一性并撤销旧会话，不能直接复制文件或将 SQLite 挂共享盘。
- /readyz 同时检查 PostgreSQL 和 Redis；Redis 故障使就绪返回 503，登录也 503。已存在 Bearer 会话在 PostgreSQL 正常时仍可鉴权；负载均衡是否摘除整个实例取决于运维策略。
- 请求 ID、slog JSON 访问日志、鉴权操作日志、panic 恢复、安全响应头、HTTP 超时、信号触发优雅退出；日志不记录密码、Cookie、令牌、正文。
- 持久化审计记录操作人、时间、动作、对象、结果与请求 ID：登录成功、创建用户、修改/重置密码（含当前密码错误失败）、禁用/启用、角色变更、会话撤销、项目成员增删改（project.member_add / project.member_update / project.member_remove）、构建触发（build.trigger）、取消（build.cancel）与重试（build.retry）、部署目标增删改查与探测（deployment_target.create / deployment_target.update / deployment_target.remove / deployment_target.check）。构建、成员与部署目标审计都和状态变更在同一事务提交；幂等重复取消不重复记账，detail 只写项目/成员与构建号或角色（重试额外写 `retry_of=<原构建号>`），不写命令或凭据。失败登录因限流兜底仅进结构化日志，不写审计表。审计查询走 GET /api/v1/audit（admin），保留策略待业务审计里程碑设计。
- 过期会话与 refresh 历史由后台任务每小时清理（启动时先执行一次）：删除超过绝对期限的会话及其全部 refresh 令牌记录。重放检测只需保留到会话有效期，这与文档化的保留边界一致。

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
# 真实进程生命周期验收（启动/就绪/优雅退出/重启持久化），可复用同一测试库：
$env:AEGIS_OPS_DATABASE_URL = $env:AEGIS_TEST_DATABASE_URL
$env:AEGIS_OPS_REDIS_URL = 'redis://localhost:6379/2'
go test ./cmd/server/ -run TestRealProcessLifecycle -count=1
```

分层测试覆盖 auth 核心（持久化、并发轮换/重放撤销、remember、绝对期限、版本迁移、多连接池会话互通、改密/重置/禁用/角色/最后管理员保护、会话列表与撤销、审计、过期清理）、projects 注册表（校验、大小写唯一、归档只读与恢复、分页与状态过滤、事务内审计、可见性读过滤与成员增删改、创建者不可移除与 404 枚举语义）、构建流水线（触发校验与每项目编号、成功与失败退出码、超时、取消与幂等、重启恢复为 interrupted、跨副本 worker 租约互斥与待命接管、SSE 推送与写超时豁免/终态收尾/非法游标、游标日志分页与截断、RBAC 与未接线 503）、部署目标（校验与默认值、大小写唯一与冲突、凭据 AES-256-GCM 加密与 AAD 绑定、更新时保留/替换/清空语义、非配置密钥时拒绝写入、探测成功/非 2xx/超时/网络错误与结果落库、事务内审计、admin-only RBAC 与未接线 503）、HTTP（Cookie flags、Secure 显式配置、默认/session/持久模式、轮换、幂等退出、缺 Cookie、JSON 降级拒绝、CSRF、credentialed CORS、Bearer/RBAC、严格 JSON、限流/错误封装、账号管理与项目/构建/部署目标端点、未接线依赖 503）、真实进程生命周期（编译真实二进制、随机端口、readyz、登录与项目创建、Windows CTRL_BREAK/SIGTERM 优雅退出且退出码 0、重启后 access token/refresh Cookie/项目数据存活），以及真实 httptest.Server + CookieJar 登录→刷新→退出链路。

## 架构

cmd/server 负责配置、PostgreSQL 连接池、Redis、迁移与标准库 http.Server 生命周期（监听器先 net.Listen 再 Serve，日志报告实际绑定地址；worker 通过租约启动并随信号上下文退出；进程生命周期验收测试见 cmd/server/ops_test.go）；internal/config 校验连接 URL、连接数、登录配额、凭据密钥与可信代理；internal/dbmigrate 拥有唯一版本化迁移账本（auth 表与后续模块表共用）；internal/auth 负责用户/密码/会话/JWT；internal/projects 是项目注册表（校验、活跃/归档生命周期、可见性与成员、分页与事务内审计，读写都带可见性谓词，不依赖 Gin）；internal/builds 是构建流水线（触发与编号、单 worker 租约、SKIP LOCKED 领取、临时目录执行、取消看门狗、批量日志与重启恢复，不依赖 Gin）；internal/deployments 是 admin-only 部署目标注册表（HTTP(S) 目标、AES-256-GCM 凭据加密、连通性探测与事务内审计，不依赖 Gin）；internal/ratelimit 负责 Redis 共享限流；internal/httpapi 负责 Gin 路由与 Cookie、Origin、CORS、Bearer/RBAC。JSON 解码按请求校验，不更改 Gin 全局 binding。Gin 1.11 与 Go 1.23 兼容。

本地双实例样例见 [compose.yaml](../compose.yaml) 与 [部署说明](../deploy/README.md)。应用副本可横向扩展，但数据库、Redis、入口网关的高可用需另行部署；样例单节点依赖不等于整套服务高可用。数据库集成测试未配置 URL 会明确 Skip，不能将跳过当作生产验证通过。
