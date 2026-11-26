# AegisFlow Go 后端

Go 1.27（构建、CI 与镜像使用 1.27 线最新补丁；`go.mod` 最低要求 1.26），Gin 1.12 + slog，PostgreSQL（pgx）+ Redis 共享限流，多应用实例部署，bcrypt，HS256 JWT。浏览器鉴权采用短期 Bearer access token + HttpOnly refresh Cookie；推荐前后端通过**同源代理**部署。项目注册表、最小构建闭环（触发、列表、详情、取消、游标日志）、部署目标、部署任务、部署审批与审批 Webhook 通知、审查中心（门禁式变更审查的请求/决策/取消、编号与审计、签名外部结果回写与审查事件通知）、安全扫描（外部工具签名回写结果登记、按项目可见性查询）与攻击防御事件（外部检测工具签名回写、admin 查询与确认+审计）已实现；知识库、自动化策略、独立密钥管理、LDAP/OIDC 与多租户等其余业务接口尚未实现。完整接口契约见 [openapi.yaml](openapi.yaml)。

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

环境配置见 [.env.example](.env.example)。AEGIS_DATABASE_URL 和 AEGIS_REDIS_URL 均必填，无 SQLite/进程内限流自动回退；旧 AEGIS_DATABASE_PATH 会使启动失败。每个实例默认最多 20 个数据库连接、空闲 5 个，可通过 AEGIS_DB_MAX_OPEN/IDLE 调整，连接总数需按副本数控制。**AEGIS_ACCESS_TTL** 是 access token 有效期（默认 15m，必须不超过 1h 且短于 AEGIS_REFRESH_TTL）；**AEGIS_REFRESH_TTL** 是会话绝对期限（默认 720h，refresh 轮换保留原绝对期限、不延长）；AEGIS_BUILD_TIMEOUT 是单次构建的执行上限（默认 10m，最大 24h）；AEGIS_LOGIN_RATE_LIMIT 是每 IP 每分钟的登录配额（默认 10，取值 1-1000，仅供测试环境在单地址多账号时放宽）；AEGIS_AUDIT_RETENTION 是审计保留窗口（默认 `0` 永久保留；取值 24h–8760h 时每小时清理更早的审计行）。**AEGIS_SECRET_KEY** 是部署目标凭据的 AES-256-GCM 密钥（base64 恰好 32 字节；留空表示禁用凭据存储，无凭据目标仍可用，写入凭据返回 400）；**AEGIS_SECRET_KEY_PREVIOUS** 是轮换期间只用于解密旧密文的退役密钥（同样 base64 32 字节，必须与当前钥不同，且不能单独配置；写入永远用当前钥）；**AEGIS_DEPLOY_CHECK_TIMEOUT** 是单次部署目标连通性探测的上限（默认 5s，最大 1m）；**AEGIS_DEPLOY_TIMEOUT** 是单次部署执行的上限（默认 10m，最大 1h）；**AEGIS_DEPLOY_HEALTH_ATTEMPTS** 是部署端点返回 2xx 后对目标健康路径的复查次数（默认 3，0 关闭，最大 20），**AEGIS_DEPLOY_HEALTH_INTERVAL** 是两次复查的间隔（默认 2s，1s–1m）。**AEGIS_BUILD_SANDBOX** 选择构建执行方式：`none`（默认，开发用）以服务账号执行并在启动时打印警告；`container` 为每个构建启动一次性容器，配合 `AEGIS_BUILD_IMAGE`（默认 `alpine:3.20`）、`AEGIS_BUILD_CPU`（默认 2）、`AEGIS_BUILD_MEMORY`（默认 2g）、`AEGIS_BUILD_PIDS`（默认 512）使用，启动时探测容器运行时，不可用即拒绝启动；`user` 在 Linux 上通过 `setpriv` 降权到 `AEGIS_BUILD_USER` 指定的专用账号，服务必须以 root 启动，否则启动失败（Windows 不支持该模式）。**AEGIS_BUILD_DISK**（默认 2g，范围 16MiB–1TiB）限制单次构建的工作区大小；**AEGIS_BUILD_CLONE**（默认 false）开启仓库 clone，**AEGIS_BUILD_ALLOW_LOCAL_REPO**（默认 false）额外允许 `file:///` 与绝对本地路径。数据库和 Redis 应使用隔离网络、访问控制、TLS 与备份保护。

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
| GET | /readyz | 无鉴权，PostgreSQL + Redis 检查（结果缓存 1s） | `{status:"ready"}` |
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
| GET | /api/v1/deployment-targets/options | Bearer（所有已登录角色） | `{items}`，启用中目标的只读投影（id/name/environment/endpoint） |
| GET | /api/v1/deployment-targets | Bearer + admin；`?limit=&offset=` | `{items,total,limit,offset}`，按名称排序 |
| POST | /api/v1/deployment-targets | Bearer + admin；`{name,endpoint,kind?,environment?,healthPath?,deployPath?,secret?,status?}` | DeploymentTarget，201；名称重复 409 |
| GET | /api/v1/deployment-targets/{id} | Bearer + admin | DeploymentTarget |
| PATCH | /api/v1/deployment-targets/{id} | Bearer + admin；至少一字段；`secret` 省略保留、空串清除、非空替换 | DeploymentTarget；重名 409 |
| DELETE | /api/v1/deployment-targets/{id} | Bearer + admin | `{removed:true}`；被部署历史引用 409 |
| POST | /api/v1/deployment-targets/{id}/check | Bearer + admin；**`{}`** | DeploymentTargetCheck（探测结果同时落库并写审计） |
| GET | /api/v1/projects/{id}/deployments | Bearer（可见者）；`?limit=&offset=&status=` | 部署分页，最新在前；不可见项目 404 |
| POST | /api/v1/projects/{id}/deployments | Bearer + 项目写权限（admin，或可写项目的 member）；`{targetId,version,buildId?,reviewId?}` | Deployment，201；`queued`，命中审批环境为 `awaiting_approval`；命中审查门禁环境缺 `reviewId` 或审查未批准 400；不可见 404 |
| GET | /api/v1/deployments | Bearer（所有角色）；`?limit=&offset=&projectId=&status=` | 部署分页，最新在前，仅含可见项目的部署 |
| GET | /api/v1/deployments/{id} | Bearer（可见者） | Deployment；不可见 404 |
| GET | /api/v1/deployments/{id}/events | Bearer（可见者）；`?after=&limit=`（after 为事件 id 游标，limit 1-1000，默认 200） | `{items}`，按事件 id 升序；不可见 404 |
| POST | /api/v1/deployments/{id}/cancel | Bearer + 项目写权限；**`{}`** | Deployment；重复取消幂等 200，终态 409 |
| POST | /api/v1/deployments/{id}/rollback | Bearer + 项目写权限；**`{reviewId?}`** | 新 Deployment，201（queued；命中审批环境为 awaiting_approval）；无更早成功 400，源非终态 409；命中审查门禁环境缺 `reviewId` 或审查未批准 400 |
| GET | /api/v1/approvals | Bearer + admin；`?limit=&offset=&status=` | `{items,total,limit,offset}`，最新在前 |
| POST | /api/v1/approvals/{id}/approve | Bearer + admin；**`{}`** | Approval；重复批准幂等 200，冲突 409 |
| POST | /api/v1/approvals/{id}/reject | Bearer + admin；`{reason?}`（可选、trim、单行、≤512 字节） | Approval；重复拒绝幂等 200，冲突 409 |
| GET | /api/v1/projects/{id}/reviews | Bearer（可见者）；`?status=&limit=&offset=` | `{items,total,limit,offset}`，最新在前；不可见项目 404 |
| POST | /api/v1/projects/{id}/reviews | Bearer + 项目写权限（admin，或可写项目的 member）；`{subject,description?,version?,externalUrl?}` | Review，201（pending）；不可见 404 |
| GET | /api/v1/reviews | Bearer（所有角色）；`?limit=&offset=&projectId=&status=` | `{items,total,limit,offset}`，最新在前，仅含可见项目的审查；不可见 `projectId` 过滤返回空列表（与全局部署列表一致，不可枚举） |
| GET | /api/v1/reviews/{id} | Bearer（可见者） | Review；不可见 404 |
| POST | /api/v1/reviews/{id}/approve | Bearer + 全局 admin 或项目 maintainer；**`{reason?}`**（可选、trim、单行、≤512 字节） | Review；重复批准幂等 200，终态 409 |
| POST | /api/v1/reviews/{id}/reject | Bearer + 全局 admin 或项目 maintainer；**`{reason}`**（必填、trim、单行、≤512 字节） | Review；重复拒绝幂等 200，终态 409 |
| POST | /api/v1/reviews/{id}/cancel | Bearer + 发起人本人或 admin/maintainer；**`{}`** | Review；重复取消幂等 200，终态 409 |
| POST | /api/v1/reviews/{id}/external-results | **HMAC 签名（无会话）**：`X-AegisFlow-Timestamp` + `X-AegisFlow-Signature` + `X-AegisFlow-Delivery`，`Content-Type: application/json`，`{source,verdict,summary?,url?}` | ReviewExternalResult，201；重复 `deliveryId` 幂等 200 返回既有记录；签名/时间戳/头缺失 401；未知审查 404；未配置 `AEGIS_REVIEW_WEBHOOK_SECRET` 503 |
| GET | /api/v1/reviews/{id}/external-results | Bearer（可见者） | `{items}`，按 `receivedAt` 倒序最多 50 条；不可见 404 |
| GET | /api/v1/projects/{id}/scans | Bearer（可见者）；`?verdict=&limit=&offset=` | `{items,total,limit,offset}`，最新在前；不可见项目 404 |
| POST | /api/v1/projects/{id}/scans | **HMAC 签名（无会话）**：`X-AegisFlow-Timestamp` + `X-AegisFlow-Signature` + `X-AegisFlow-Delivery`，`Content-Type: application/json`，`{source,tool?,version?,verdict,summary?,reportUrl?}` | Scan，201；重复 `deliveryId` 幂等 200 返回既有记录；签名/时间戳/头缺失 401；未知项目 404；未配置 `AEGIS_INGEST_WEBHOOK_SECRET` 503 |
| GET | /api/v1/scans | Bearer（所有角色）；`?limit=&offset=&projectId=&verdict=` | `{items,total,limit,offset}`，最新在前，仅含可见项目的扫描；不可见 `projectId` 过滤返回空列表（不可枚举） |
| GET | /api/v1/scans/{id} | Bearer（可见者） | Scan（含 projectName）；不可见/不存在 404 |
| POST | /api/v1/security-events | **HMAC 签名（无会话）**：同上三个签名头，`{source,severity,eventType,summary,occurredAt?,detailsUrl?}` | SecurityEvent，201；重复 `deliveryId` 幂等 200；`occurredAt` 不得晚于 now+300s；未配置 `AEGIS_INGEST_WEBHOOK_SECRET` 503 |
| GET | /api/v1/security-events | Bearer + admin；`?severity=&status=&source=&limit=&offset=` | `{items,total,limit,offset}`，按 `occurredAt` 倒序；非 admin 403 |
| GET | /api/v1/security-events/{id} | Bearer + admin | SecurityEvent；不存在 404；非 admin 403 |
| POST | /api/v1/security-events/{id}/ack | Bearer + admin；**`{}`** | SecurityEvent；open→acknowledged，重复确认幂等 200 且不重复审计；不存在 404 |

**所有 auth POST（包括 login）强制恰好一个 Origin，且精确匹配允许列表**；缺失、不匹配、`null` 或跨站请求返回 `403 origin_denied`，在登录、轮换和退出之前拦截。非浏览器 curl/脚本也必须带该 Origin。其他请求如果携带 Origin 也须符合允许列表。全局拒绝 `Sec-Fetch-Site: cross-site`。允许来源响应包含 `Access-Control-Allow-Origin: <原始允许源>`、`Access-Control-Allow-Credentials: true`，`Vary: Origin, Sec-Fetch-Site`；OPTIONS 额外 Vary 请求方法与请求头，允许 `GET, POST, PATCH, DELETE, OPTIONS` 和 Authorization/Content-Type（覆盖全部 PATCH/DELETE 端点）。

### 项目可见性与成员（P1 + P2）

- `projects.visibility` 为 `public`（默认）或 `private`。全局 admin 永远可见；其他角色可见 public 项目，以及自己创建或被加入的项目（`project_members`）。
- 可见性在服务端逐请求解析：项目列表、项目详情、构建列表/详情/日志/SSE、成员列表、触发构建与改项目都带同一条可见性谓词。**不可见一律 404**（而非 403），避免用私有 id 枚举项目。
- **写权限叠加（P2）**：全局角色是上限（viewer 永不可写、admin 永远可写）；private 项目另外要求调用者是该项目 `maintainer`，否则 **403**（可见但无权）。public 项目保持「任意 member 可写」。判定集中在 `projects.AuthorizeWrite`，项目修改、归档、触发/取消/重试构建、成员增删改都复用它；构建侧在自己的事务里传入同一个 `*sql.Tx`。
- 成员变更与状态同事务写审计：`project.member_add` / `project.member_update` / `project.member_remove`（objectType `project`，detail 只写用户名与角色）。
- 创建者在同一事务内写入 maintainer 成员行，语义上是 owner：不能被移除（400）。成员目标是真实账号（不存在 404），重复添加 409。
- 迁移 v5 为存量项目补写创建者的 maintainer 行，并把 `visibility` 默认为 `public`，因此升级后行为不变。
- 已知的决策待确认点：public 项目的成员管理目前允许任意 admin/member（P1 行为），设计稿矩阵写作「member 仅成员列表」；若按后者收紧，只需调整 `AuthorizeWrite` 的一个分支。

### 构建执行（P3 第一批）

- 状态机六态：`queued → running → succeeded/failed/cancelled`，只有持有 worker 租约的实例在启动后把遗留的 `running` 统一转为 `interrupted`。`interrupted` 表示进程被打断而不是业务失败，需要重新触发。租约是 PostgreSQL 会话级 advisory lock（按 database + schema 隔离）：同一部署只有一个副本执行与恢复，其余副本待命并在下一重试周期接管；连接或进程消失时锁自动释放。持有人在执行期间每 5 秒（单次 3 秒超时）用租约专用连接探活，失败即记录告警、取消本次执行上下文并回到待命循环，避免锁已释放却继续领取、与接管副本并行。它保证"同一时刻一个 worker"，不提供多 worker 并行吞吐。
- 触发时固化命令、分支、仓库地址（`repoUrl`）与触发人快照，构建号在每个项目内自增；之后修改项目不影响已触发的构建。命令在系统临时目录执行（Unix `sh -c`、Windows `cmd /c`），结束后删除目录。目录显式以 0700 创建；构建命令只获得显式白名单环境变量（`PATH`、`HOME`、`TMPDIR`/`TMP`/`TEMP`、`LANG` 等，Windows 另加 `SYSTEMROOT` 等系统变量），**不继承** `AEGIS_*` 服务凭据。
- 仓库 clone（`AEGIS_BUILD_CLONE=true` 且构建有 `repoUrl`）：开始时用 `git clone --depth 1 --single-branch --branch <构建分支>` 把仓库拉到 `repo/`，命令在该目录执行；clone 在宿主侧完成（容器模式容器无网络），并沿用同一执行身份（`user` 模式下即为构建账号）。只支持无凭据的 https / `git@` 地址；含内联凭据的 URL 直接失败，私有仓库凭据本批不支持；`file:///` 与绝对路径仅在 `AEGIS_BUILD_ALLOW_LOCAL_REPO=true` 时允许。clone 失败写一条脱敏后的 `git clone failed: ...` system 日志并把构建置 `failed`。未开启 clone 时若项目配了 `repoUrl`，会显式记录"cloning is disabled"并在空工作区执行。
- 每个构建工作区受 `AEGIS_BUILD_DISK` 限制：worker 每 2 秒统计 `repo/` 与生成文件的大小，超限即取消构建（含容器）并写 system 日志置 `failed`。看门狗 fail-closed：遍历中出现任何错误（目录不可读、`stat`/`Info` 失败、条目在统计途中消失）都视为"无法确认上限"，同样立即取消构建、置 `failed`，并在 system 日志写明原因；不会把"读不到大小"当成"未超限"放行。这是尽力而为的看门狗，不是文件系统配额，正常统计时超限最多会越过上限一个轮询周期的写入量。
- worker 随服务进程启动、单并发运行；持有租约的副本用 `SELECT ... FOR UPDATE SKIP LOCKED` 领取任务，`AEGIS_BUILD_TIMEOUT`（默认 10m，最大 24h，启动时经 config 接到 worker）到期、收到取消请求、服务退出或磁盘超限时终止整个进程树：Unix 使用独立进程组并对组发 `SIGKILL`，Windows 使用 `taskkill /PID <pid> /T /F`（失败回退 `Process.Kill`），并设置 10 秒 `WaitDelay` 兜底被继承的输出管道。取消通过 300ms 状态看门狗传播到正在运行的命令；看门狗读库失败只记录错误并在下个周期重试，不当作取消，终态写入失败同样记录错误日志。待命副本不领取、不恢复，只定期重试租约。
- 日志按行写入 `build_log_events`，批量 500ms 落库；单行超过 4096 字节按 rune 边界截断并追加 `…[truncated]`，但读取继续到 EOF——单行超长不再丢失后续输出或让子进程卡在写满的管道上；单构建最多记录 5000 条事件，超限后追加一条 system 截断说明。读取有两条通道：`GET /builds/{id}/events` 的 SSE 推送（`status`/`logs`/`done` 帧，`Last-Event-ID` 或 `after` 续传，15s 心跳，终态后等日志静默 3s 再结束，单连接上限 10 分钟后发 `reconnect`，并发连接上限默认 64（超出时新流立即返回 503 `not_ready`），单帧写入有 10s 超时以隔离不读数据的慢客户端；服务关停时向活跃流发 `{"reason":"shutdown"}` 帧后立即返回，不阻塞优雅退出），以及保留的 `after` 游标轮询作为回退。SSE 由前端用 `fetch` 流式读取并携带 Bearer——`EventSource` 无法设置 Authorization 头，token 不放进 URL。
- 执行隔离由 `AEGIS_BUILD_SANDBOX` 决定。`container` 模式下每个构建是一个一次性容器：`--network none`、`--read-only` 根文件系统、`--cap-drop ALL`、`no-new-privileges`，只把构建目录挂到 `/workspace`，CPU/内存/进程数由 `--cpus`/`--memory`/`--pids-limit` 限制；在非 Windows 平台还以服务进程的 uid/gid 通过 `--user <uid>:<gid>` 运行（绑定挂载的 0700 工作区因此对容器可写，且容器内进程不是 root）；Windows（Docker Desktop）的宿主 uid 不映射进其 Linux VM，因此不添加该参数，隔离依赖 Docker Desktop 自身的文件共享与用户映射。超时、取消或服务退出时显式 `docker kill` 容器，避免留下孤儿进程。`user` 模式在 Linux 上把 clone 与命令都经 `setpriv` 降权到 `AEGIS_BUILD_USER`，工作区 chown 给该账号，启动时校验 root、账号存在、目标账号 uid 不为 0（root 不能作为构建账号）与 `setpriv` 可用，否则拒绝启动；Windows 不支持该模式。`none` 模式仍以服务账号执行，只用于开发。所有模式都只注入最小环境变量白名单，不继承 `AEGIS_*` 服务凭据。
- 输出脱敏已实现：所有落库行（stdout/stderr 与 system 生命周期行）在写入前经过同一套规则——`AEGIS_*=...`、`Bearer ...`、`Authorization: Basic <base64>`/`Basic <base64>`、任意 scheme 的内联凭据 URL（`postgres://`/`redis://`/`mysql://`、带 userinfo 的 `https://` 等，用户部分可省略）替换为 `[redacted]`；`ghp_`/`github_pat_`/`glpat-`/`AKIA`/`xox[baprs]-`/`sk-` 前缀、JWT、PEM 私钥头替换为对应的 `[redacted-*]` 标记。多行 PEM 私钥由跨行状态机处理：从 `-----BEGIN ... PRIVATE KEY-----`（RSA/EC/OPENSSH/普通）到匹配的 `-----END ... PRIVATE KEY-----` 之间的每一行（含首尾）都以 system 流写成 `[redacted]`，块内任意内容不落库；缺少 END 时该流后续行持续脱敏（fail closed）；stdout/stderr 各自维护块状态，块外正常输出不受影响。构建结束时追加一条 system 统计行（只记数量）。命中但无法确认移除的行整行丢弃并计数（fail closed）。自由文本口令仍不覆盖。
- 本批边界（仍不做）：私有仓库凭据（部署密钥 / token）、构建阶段与产物、构建缓存、多 worker 并行吞吐。`container` 与 `user` 模式已提供隔离和资源上限；`none` 模式仅用于开发。

### 部署目标（P4 第一批）

- `deployment_targets` 是全局管理对象：`/api/v1/deployment-targets` 的列表、创建、详情、修改、删除与连通性探测仅 admin 可用，member/viewer 返回 403；只有目标选项 `GET /api/v1/deployment-targets/options` 对任何已登录角色开放（见下）。`kind` 目前只有 `http`。
- 目标字段：`name`（3–64 字节、单行、大小写不敏感唯一）、`environment`（`development`/`staging`/`production`，默认 development）、`endpoint`（http/https，禁止 userinfo、query、fragment）、`healthPath`（默认 `/`，必须以 `/` 开头）、`deployPath`（默认 `/deploy`，必须以 `/` 开头，≤256 字节）、`status`（`active`/`disabled`）。
- 网络边界：部署目标 endpoint 由 admin 配置，admin 是可信配置者，目标 endpoint 属于受信配置面；允许私网与回环地址（自托管内网场景），但拒绝 link-local 与云元数据地址（`169.254.0.0/16`、`fe80::/10`、`100.100.100.200`、`fd00:ec2::254`）——IP 字面量在创建/更新时返回 400，主机名在运行时连接建立前按解析结果再次校验，覆盖 DNS rebinding。
- 凭据加密存储：`AEGIS_SECRET_KEY`（base64 32 字节）启用 AES-256-GCM；密文布局为 `[12 字节随机 nonce][ciphertext+tag]`，AAD 绑定目标 id（`deployment_target:<id>`），因此密文不能跨行复用。接口只返回 `hasSecret`，从不返回明文或密文；未配置密钥时仍可创建无凭据目标，但写入凭据返回 400。带凭据的目标必须使用 `https` 端点（仅回环地址允许 `http`），否则创建/更新直接 400，避免把 Bearer 凭据以明文发到网络上。
- 密钥轮换（第一批）：把新钥设为 `AEGIS_SECRET_KEY`、旧钥设为 `AEGIS_SECRET_KEY_PREVIOUS`，解密先试当前钥、失败再回退旧钥；**加密只用当前钥**，所以轮换后新写入的密文不再依赖旧钥。两键相同、或只配退役钥都会启动失败；解密失败的错误与具体用哪把钥匙无关，不构成探测面。存量密文用维护命令 `go run ./cmd/rekey`（见 `cmd/rekey`）重加密：默认 dry run 只报告不写，确认后加 `-apply`；它只重写能用退役钥打开的行，两把钥都打不开的行计入 `failed` 并保持原样。命令可重复执行（已迁移的行计入 `already_current`），全部完成后才能从配置里移除 `AEGIS_SECRET_KEY_PREVIOUS`。
- 目标选项：`GET /api/v1/deployment-targets/options` 向任何已登录角色返回启用中目标的只读投影（id/name/environment/endpoint），部署表单据此选择；完整目标列表、探测与凭据仍 admin-only。
- 探测：`POST /api/v1/deployment-targets/{id}/check` 对 `endpoint + healthPath` 发 GET，超时由 `AEGIS_DEPLOY_CHECK_TIMEOUT`（默认 5s，最大 1m）控制；有凭据时发送 `Authorization: Bearer <secret>`（仅在本次探测的内存中解密）。2xx 记为健康，其余记录 `http <code>`、`timeout` 或脱敏后的网络错误；结果写入 `last_check_at/last_check_ok/last_check_detail`。默认客户端最多 3 跳、不跨主机，并拒绝 `https`→`http` 降级。
- 审计：创建、修改、删除、探测分别写 `deployment_target.create|update|remove|check`（objectType `deployment_target`），与状态变更同事务；detail 只含名称或探测结果，绝不含凭据。
- 本批边界（P4.1 范围）：独立密钥管理页面与部署策略；删除为硬删除，但被部署历史引用的目标返回 409（保留历史快照）。审计保留策略已由 `AEGIS_AUDIT_RETENTION` 覆盖（见下文）。前端页面见「部署目标」相关 UI。

### 部署任务（P4.2）

- 协议：`POST <endpoint 去尾斜杠><deployPath>`（`deployPath` 默认 `/deploy`，可随目标配置），JSON 体 `{deploymentId, project, version, environment, rollback, rollbackVersion}`；目标有凭据时携带 `Authorization: Bearer`，只在执行时内存解密，绝不进入事件、审计或错误信息。部署端点返回 2xx 后还会对 `endpoint + healthPath` 复查（`AEGIS_DEPLOY_HEALTH_ATTEMPTS`，默认 3，0 关闭；`AEGIS_DEPLOY_HEALTH_INTERVAL`，默认 2s）：只有复查也是 2xx 才记 `succeeded`，否则记 `failed`（detail `health check failed: <原因>`），每次复查写一条 system 事件且共用 `AEGIS_DEPLOY_TIMEOUT` 的总预算。部署端点非 2xx 记 `http <code>`，超时（`AEGIS_DEPLOY_TIMEOUT`，默认 10m，最大 1h）与网络错误记脱敏 detail；部署请求本身不重试。
- 状态：`queued → running → succeeded/failed/cancelled`，重启恢复把遗留 `running` 置 `interrupted`；命中审批环境时创建即 `awaiting_approval`（见下一节），否则直接排队。
- 单 worker：独立 PostgreSQL advisory 租约 `aegisflow:deployments:worker`，只有持锁副本恢复 `interrupted` 并用 `SELECT ... FOR UPDATE SKIP LOCKED` 领取；300ms 状态看门狗在取消/终态时中止 HTTP 请求；`finish` 用独立 5s 上下文落终态。上游响应按行写入 `deployment_events`（批量 500ms；单行 ≤4096 字节、最多 5000 行 response，system 生命周期行不计入上限）。
- 查询：项目内列表 `GET /api/v1/projects/{projectId}/deployments`，全局列表 `GET /api/v1/deployments?projectId&status&limit&offset`（与构建列表同形，任何已登录角色可用，按项目可见性过滤）；详情、事件游标（`after&limit`）与取消/回滚都在 `/api/v1/deployments/{id}` 下。
- 目标快照：创建时固化 `target_name/target_endpoint/target_environment`，之后改名或改端点不影响历史；执行使用目标当前配置，目标在执行前被禁用/删除会置 `failed`。
- 回滚：`POST /api/v1/deployments/{id}/rollback` 仅接受 `succeeded`/`failed` 源，取同项目+目标中比源部署更早的最近一次 `succeeded` 版本，新建一条部署（通常为 `queued`，命中审批环境时为 `awaiting_approval`；`rollbackOf` 指向源，payload `rollback=true`、`rollbackVersion` 为该版本）；无更早成功返回 400，源非终态 409；重复回滚会创建多条部署（非幂等，审计可追溯）。
- 审查门禁（P5-A E2）：`AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS`（逗号分隔的 development/staging/production，默认空 = 不启用，显式空值同样关闭）列出必须引用**已批准**审查的环境。创建与回滚都接受可选 `reviewId`；提供时必须属于同一项目且 `status=approved`（不存在、不可见或未批准统一返回 400 `validation_failed`，不保留可用于枚举的差异），目标环境命中门禁列表时 `reviewId` 必填。部署行 `review_id` 记录创建时的审查快照（无审查为 NULL），接口响应含 `reviewId`；审计 detail 追加 `review=<id>`，不写审查正文。门禁在审批状态机之前校验，与 P4.3 审批叠加（先审后批）；门禁默认关闭，因此升级后的既有部署行为完全不变。
- 权限与审计：部署端点按项目写权限授权——admin 全量；member 对可写项目（public，或 private 且为 maintainer）可创建、取消与回滚；viewer 只读不可写。列表与详情按项目可见性过滤，不可见一律 404（防枚举）；审批接口仍 admin-only。`deployment.create`（detail `target=<name> version=<v> approval=none|required`，命中审查门禁时追加 ` review=<id>`）、`deployment.cancel`（`from=<status>`）、`deployment.rollback`（`rollback_of=<id> version=<v>`，命中审查门禁时追加 ` review=<id>`）与状态变更同事务，重复取消失败不重复记账。
- 本批边界：部署产物与构建强制关联（`buildId` 可选，提供时必须是同项目 `succeeded` 构建）、自定义 HTTP 方法与路径、自动重试均未做。


### 部署审批（P4.3）

- 命中 `AEGIS_DEPLOY_APPROVAL_ENVIRONMENTS` 的环境：创建部署或回滚时在**同一事务**写入 `awaiting_approval` 部署、`pending` 审批（`expiresAt = now + AEGIS_APPROVAL_TTL`）、`deployments.approval_id`，并写 `deployment.create`（或 `deployment.rollback`）与 `approval.request` 两条审计。其余环境保持 `queued` 直接执行。变量未设置时默认 `production`；显式设为空值（含只有逗号/空白）时关闭审批，此时配置层返回非 nil 空切片，不会回落到 `production` 默认值。
- 审批接口全部 admin-only：`GET /api/v1/approvals?status=&limit=&offset=`（按创建时间倒序，可按 pending/approved/rejected/expired 过滤）、`POST /api/v1/approvals/{id}/approve`（body `{}`）、`POST /api/v1/approvals/{id}/reject`（body `{reason?}`，reason 可选、trim、单行、≤512 字节）。
- 批准：审批 `approved`，部署 `awaiting_approval → queued` 并立即唤醒 worker；重复批准返回同一审批（200）且不重复写审计。拒绝：审批 `rejected` + reason，部署 `rejected`（detail `approval rejected`）；重复拒绝同样幂等。
- 冲突：非 pending 审批、或审批对应部署已不再 `awaiting_approval`（例如已被取消）时返回 409；此时审批保持 pending，由过期扫描关闭。
- 过期：持租约 worker 每 30s（接管时先跑一次）扫描 `pending AND expires_at <= now`，每批最多 100 条、`FOR UPDATE SKIP LOCKED`；审批置 `expired`（reason `approval expired`），部署置 `rejected`（detail `approval expired`），并为每行写 `approval.expire` 审计（actor 为空）。
- 取消交互：取消 `awaiting_approval` 部署时，同一事务把审批置 `expired`（reason `deployment cancelled`，decided_at=now）并写 `approval.expire`；重复取消不会重复记账。审批状态机没有 cancelled，因此复用 `expired`。
- 审计：`approval.request`（detail `deployment=<id>`）、`approval.approve`（`deployment=<id>`）、`approval.reject`（`deployment=<id> reason=<...>` 可选）、`approval.expire`（`deployment=<id> reason=approval expired|deployment cancelled`），objectType `approval`。
- 本批边界：多级会签仍未实现；前端审批页面已接入（#42）；通知渠道见 P4.4。

### 审批通知（P4.4）

- 可选配置：`AEGIS_APPROVAL_WEBHOOK_URL` 非空时启用；为空时完全不写 outbox，启用后只对新事件入队、不回填历史审批。URL 必须是 http(s)，除回环地址外只允许 https，且禁止 userinfo/fragment。`AEGIS_APPROVAL_WEBHOOK_SECRET` 可选（16–256 字节），配置后对**原始 JSON 请求体**计算 HMAC-SHA256，写入 `X-AegisFlow-Signature: sha256=<hex>`；`AEGIS_APPROVAL_WEBHOOK_TIMEOUT` 默认 5s、最大 30s。
- 事件：`approval.requested`、`approval.approved`、`approval.rejected`、`approval.expired`。请求与三种终态在**审批状态变更的同一事务**写入 `approval_notifications` outbox，payload 记录事件发生时的审批、部署编号、项目名、目标、环境、版本与状态；不包含目标凭据、服务凭据、Cookie 或令牌。
- 投递：独立 notifier 轮询 outbox（不占部署 worker 租约），每行按 `FOR UPDATE SKIP LOCKED` 领取，多个副本可并行；成功 2xx 置 `delivered`。失败按 10s 起的指数退避重试，最多 8 次，之后置 `failed` 并保留脱敏后的 `last_error`。语义是**至少一次**：接收方应按 `X-AegisFlow-Delivery`（同时是 payload `deliveryId`）幂等去重。
- 通道复用（P5-A E4，审查通知）：同一 `AEGIS_APPROVAL_WEBHOOK_URL`/`AEGIS_APPROVAL_WEBHOOK_SECRET`/`AEGIS_APPROVAL_WEBHOOK_TIMEOUT` 现在同时投递 `review.requested|approved|rejected|cancelled` 事件；接收方按 `X-AegisFlow-Event` 前缀路由。审查通知使用独立 outbox `review_notifications`，由 reviews 包的 notifier 投递，签名、header 与重试语义与审批通知完全一致（见「审查中心」）。不新增出站配置。
- 可观测与边界：日志只记录审批 id、事件、尝试次数与 `http <code>`/`network error`，不记录 URL、签名密钥或 payload；通知失败不回滚、不改变审批或部署状态。没有管理 API、手动重放或通知历史页面；失败行保留在表中供运维排查。
- 审计：通知 outbox 与审批审计同事务；不新增一条独立审计动作，避免把投递重试写进业务审计。OpenAPI 无新增路由。

### 审查中心（P5-A，门禁式）

- 定位：对项目的一次变更（版本标签或外部构建链接）发起审查，由项目 maintainer 批准或拒绝，全程审计，并允许发起人/维护者取消；E2 起审查可作为部署门禁（见上节与「审查门禁」）。E4 起外部扫描/审查工具可通过**签名入站接口**回写只读结果，审查事件进入既有审批 Webhook 通知通道；本平台仍不自研扫描或 AI 审查引擎。
- 接口：`POST|GET /api/v1/projects/{projectId}/reviews`、`GET /api/v1/reviews`（全局列表，任何已登录角色，按项目可见性过滤，支持 `projectId`/`status`/分页）、`GET /api/v1/reviews/{id}`、`POST /api/v1/reviews/{id}/approve|reject|cancel`。发起复用项目写权限叠加（admin，或可写项目的 member）；批准/拒绝要求**全局 admin 或该项目 maintainer**（`projects.AuthorizeDecide`，比写权限更严：public 项目的普通 member 不能决策）；取消允许发起人本人，或 admin/maintainer。列表与详情按项目可见性过滤，不可见一律 404；全局列表按可见性过滤而非 404，`projectId` 过滤时不可见项目返回空列表（与 `GET /api/v1/deployments` 同语义，不可枚举）。
- 字段：`subject` 必填、3–200 字节、单行（trim 后校验）、`description` ≤2000 字节、`version` ≤64 字节单行（trim）、`externalUrl` 可选、仅 http(s)、禁止 userinfo/fragment、≤2048 字节；`externalUrl` 只做展示与校验，服务端不抓取。`number` 是项目内自增序号，在 `projects` 行锁下从 `review_count` 分配（与构建/部署编号同一模式）；并发创建不会重号。
- 状态机：`pending → approved | rejected | cancelled`，终态不可变。批准/拒绝/取消都只接受 pending，其余状态返回 409；重试已处于目标终态的请求幂等返回 200 与同一结果，且不重复写审计。`decidedBy/decidedByName/decidedAt` 记录决策（取消也记录取消人与时间），`reason` 保存批准的可选理由/拒绝的必填理由；取消不带理由，pending 时这些字段在 JSON 中省略。
- 审计：`review.request`（detail `project=<id> number=<n>`）、`review.approve`/`review.reject`/`review.cancel`（objectType `review`，detail 只写 `number=<n>` 及 reason（如有）），四项审计与编号分配、状态变更在同一事务提交；subject/description/version/externalUrl 正文不进入审计。重复的幂等请求不重复记账。
- 迁移：schema v11 新增 `reviews` 表、`projects.review_count`、`UNIQUE(project_id,number)` 与 `(project_id,status,created_at)`、`(status,created_at)` 索引；`project_id` 外键级联删除。schema v12 为 `deployments` 增加可空 `review_id`（引用 `reviews(id)`，`ON DELETE SET NULL`）与部分索引，部署可快照其审查门禁来源。schema v13（E4）新增 `review_external_results`（`delivery_id` 全局唯一、`UNIQUE`/索引 `(review_id,received_at DESC,id)`）与 `review_notifications` 通知 outbox（`UNIQUE(review_id,event)`、索引 `(status,next_attempt_at,id)`）；两表 `review_id` 外键均级联删除。
- 外部结果回写（P5-A E4，入站）：`POST /api/v1/reviews/{id}/external-results` 是**无会话的机器接口**，由 `AEGIS_REVIEW_WEBHOOK_SECRET`（16–256 字节，未设置时该接口返回 503 且消息为 `review results are not configured`，设置不合法启动失败）做 HMAC 保护。必填头：`X-AegisFlow-Timestamp`（Unix 秒，与服务器时间差 ≤300s，否则 401）、`X-AegisFlow-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + rawBody)>`、`X-AegisFlow-Delivery`（8–128 字符 `[A-Za-z0-9._-]`，幂等键）；body 为严格 JSON `{source,verdict,summary?,url?}`（≤16 KiB，未知/`null` 字段拒绝）。校验：`source` 必填 1–64 字节单行、`verdict ∈ passed|failed|warning`、`summary` ≤500 字节（保留换行、去除其他控制字符）、`url` 可选 http(s)、无 userinfo/fragment、≤2048 字节。签名/时间戳/头缺失或格式非法统一 401；未知审查 404；非法字段 400；同一 `deliveryId` 重复回写幂等返回 200 + 既有记录（不新增）。**外部结果只作为附加证据，不改变审查状态机，也不影响部署门禁判定**。
- 外部结果读取（P5-A E4）：`GET /api/v1/reviews/{id}/external-results` 为 Bearer 接口，沿用项目可见性（不可见/不存在 404），按 `receivedAt` 倒序最多返回 50 条 `ReviewExternalResult`；响应不含签名头或密钥。
- 审查通知（P5-A E4，出站）：`review.requested`（创建）、`review.approved`/`review.rejected`（决策）、`review.cancelled`（取消）在**与状态变更/审计同一事务**写入 `review_notifications` outbox，`UNIQUE(review_id,event)` 保证幂等重复不重复入队；重试/幂等决策路径在入队前已返回，不会产生第二条事件。payload 为 `{deliveryId,event,occurredAt,project:{id,name},review:{id,projectId,number,subject,description,version,status,requestedBy,requestedByName,decidedBy?,decidedByName?,reason?,externalUrl?}}`，不含凭据、Cookie、令牌或请求头。投递复用审批通道：`X-AegisFlow-Event`/`X-AegisFlow-Delivery`/`X-AegisFlow-Signature: sha256=...`（对 payload JSON 本体签名，与审批一致），1s 轮询、每次最多 20 条、`FOR UPDATE SKIP LOCKED` + claim lease、10s 起指数退避上限 1h、最多 8 次，成功置 `delivered`、失败记 `last_error` 并按 pending/failed 落库；通知失败不回滚审查状态，也不写业务审计动作。notifier 在 reviews 依赖存在且 webhook URL 配置时随 `cmd/server` 信号上下文启动/退出。
- 本批边界（P5-A E4）：外部结果不可编辑/删除、不做多来源聚合或按 verdict 自动改门禁；通知没有管理 API、手动重放或历史页面；前端审查详情页已回显外部结果（来源、verdict 徽章、摘要与报告外链，E4B 已交付）。

### 安全扫描与攻击防御事件（P5 剩余模块批次 A）

- 定位：**集成优先，不自研扫描/检测引擎**。外部扫描或检测工具通过签名入站接口回写结果，AegisFlow 只负责登记、可见性、审计与展示；扫描结果只读，防御事件由管理员确认。两个入站接口共用 `AEGIS_INGEST_WEBHOOK_SECRET`（16–256 字节，未设置时两者返回 503 且消息为 `ingest is not configured`，设置不合法启动失败），与审查外部结果的 `AEGIS_REVIEW_WEBHOOK_SECRET` 相互独立，权限不串用；读取接口不依赖该密钥。
- 签名与防重放（与 E4 完全一致）：必填头 `X-AegisFlow-Timestamp`（Unix 秒，与服务器时间差 ≤300s，否则 401）、`X-AegisFlow-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + rawBody)>`（对原始请求体字节签名）、`X-AegisFlow-Delivery`（8–128 字符 `[A-Za-z0-9._-]`，全局幂等键）。body 为严格 JSON（≤16 KiB，未知/`null` 字段拒绝，`Content-Type: application/json`）；缺头、签名错误、时间戳越界或格式非法统一 401；`delivery_id` 重复回写返回 200 + 既有记录（不新增），首次 201。
- 扫描（`POST /api/v1/projects/{id}/scans`）：`source` 必填 1–64 字节单行；`tool`/`version` 可选、≤64 字节单行；`verdict ∈ passed|failed|warning`；`summary` ≤1000 字节（保留换行、去除其他控制字符）；`reportUrl` 可选 http(s)、无 userinfo/fragment、≤2048 字节，仅展示不抓取。`delivery_id` 全局唯一、`project_id` 外键级联删除。读取：项目内列表（`GET /api/v1/projects/{id}/scans?verdict=&limit=&offset=`，不可见项目 404）、全局列表（`GET /api/v1/scans?projectId=&verdict=&limit=&offset=`，admin 全量、其他角色按项目可见性过滤，不可见 `projectId` 过滤返回空页）、详情（`GET /api/v1/scans/{id}`，不可见/不存在 404）。Scan JSON 含 `projectName`（从 projects join），不含签名头或密钥。
- 防御事件（`POST /api/v1/security-events`）：`source` 必填 1–64 字节单行；`severity ∈ low|medium|high|critical`；`eventType` 必填 1–64 字节单行；`summary` 必填 1–1000 字节；`occurredAt` 可选 Unix 秒（默认 now，不得晚于 now+300s）；`detailsUrl` 同 reportUrl 规则。事件为全局对象：`GET /api/v1/security-events?severity=&status=&source=&limit=&offset=`（按 occurredAt 倒序）、`GET /api/v1/security-events/{id}`、`POST /api/v1/security-events/{id}/ack`（body `{}`）全部 admin-only，member/viewer 403。确认把 open→acknowledged，记录 `acknowledgedBy/acknowledgedByName/acknowledgedAt`，与 `security_event.ack` 审计同事务（objectType `security_event`，detail 只写 id/severity/eventType，不含 summary 原文）；重复确认幂等 200 且不重复审计；不存在 404。
- 迁移：schema v14 新增 `scans`（`delivery_id` 唯一，索引 `(project_id,received_at DESC,id)`、`(verdict,received_at DESC)`）与 `security_events`（索引 `(status,occurred_at DESC,id)`、`(severity,occurred_at DESC)`）；`scans.project_id` 外键级联删除。签名验证留在 httpapi（三个入站接口复用同一验签/读取辅助），业务包不依赖 Gin。
- 本批边界：不自研扫描/检测引擎、不做扫描引擎编排、结果聚合看板或按扫描 verdict 自动改部署门禁；扫描结果不可编辑/删除；防御事件没有关闭/忽略状态或批量确认。前端：`/scans` 列表与详情、`/defense` admin-only 列表与确认已接入。

### 运行指标（P4）

- 可选启用：`AEGIS_METRICS_ADDR=127.0.0.1:9090` 时服务在**独立监听地址**提供 `GET /metrics`（Prometheus 文本格式，`text/plain; version=0.0.4`）。默认关闭；该端点**没有用户鉴权**（scraper 无法持会话），因此只应绑定回环地址，或仅经受保护的代理暴露，且不要与 API 端口共用。
- 指标：`aegisflow_http_requests_total{method,route,status}`、`aegisflow_http_request_duration_seconds`（histogram，桶 5ms–10s），标签使用 **Gin 匹配到的路由模板**（如 `/api/v1/builds/:id`），未匹配请求归入 `unmatched`，因此原始路径、资源 id 不会进入标签；另有 `aegisflow_builds{status}` 在抓取时按状态统计构建数（含 0，保证序列稳定）。
- 抓取失败时 `aegisflow_builds` 整组缺失并记录错误日志，HTTP 计数器不受影响；指标端点自身不计入 HTTP 指标（它不在 API 监听器上）。
- 不提供告警规则与链路追踪：告警按部署环境在 Prometheus/Alertmanager 侧配置，追踪按需另行引入。

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
- /readyz 同时检查 PostgreSQL 和 Redis；Redis 故障使就绪返回 503，登录也 503。探测结果在进程内缓存 1 秒（成功与失败都缓存，探测超时仍为 2 秒），因此无鉴权的 /readyz 轮询不会按请求数放大对依赖的负载。已存在 Bearer 会话在 PostgreSQL 正常时仍可鉴权；负载均衡是否摘除整个实例取决于运维策略。
- 请求 ID、slog JSON 访问日志、鉴权操作日志、panic 恢复、安全响应头、HTTP 超时、信号触发优雅退出；日志不记录密码、Cookie、令牌、正文。
- 持久化审计记录操作人、时间、动作、对象、结果与请求 ID：登录成功、创建用户、修改/重置密码（含当前密码错误失败）、禁用/启用、角色变更、会话撤销、项目成员增删改（project.member_add / project.member_update / project.member_remove）、构建触发（build.trigger）、取消（build.cancel）与重试（build.retry）、部署目标增删改查与探测（deployment_target.create / deployment_target.update / deployment_target.remove / deployment_target.check）、部署创建/取消/回滚（deployment.create / deployment.cancel / deployment.rollback）、审批请求/批准/拒绝/过期（approval.request / approval.approve / approval.reject / approval.expire）、审查请求与批准/拒绝/取消（review.request / review.approve / review.reject / review.cancel，objectType `review`）、防御事件确认（security_event.ack，objectType `security_event`）。构建、成员、部署目标、部署任务、审批、审查与防御事件确认审计都和状态变更在同一事务提交；幂等重复取消/确认不重复记账，detail 只写项目/成员与构建号或角色（重试额外写 `retry_of=<原构建号>`）、审查编号与理由、防御事件 id/severity/eventType，不写命令或凭据，也不写审查正文或事件 summary 原文。部署创建/回滚命中审查门禁时 detail 追加 `review=<id>`（只写 id，不写审查正文）。签名扫描回写属于外部证据登记，不写业务审计动作。失败登录因限流兜底仅进结构化日志，不写审计表。审计查询走 GET /api/v1/audit（admin），支持 `action`/`objectType`/`result`/`actor`/`from`/`to` 服务端筛选（AND 组合，同时作用于分页与 total；`from`/`to` 接受 RFC3339 或 `YYYY-MM-DD`，只给日期时 `to` 覆盖该 UTC 全天）。
- 审计保留策略可选：`AEGIS_AUDIT_RETENTION` 未设置或为 `0` 时永久保留（默认）；设置 24h–8760h 后，与过期会话清理共用的小时任务会删除早于该窗口的审计行。删除是运维显式选择，默认不丢证据。
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
# 已知漏洞扫描（CI 的 security 任务执行同一命令，为阻断门禁；该任务用最新 stable Go 扫描标准库）：
go install golang.org/x/vuln/cmd/govulncheck@latest
& "$(go env GOPATH)/bin/govulncheck" ./...
# CI 的 windows 任务在 Windows 上跑不依赖 PostgreSQL/Redis 的部分：
gofmt -l ./cmd ./internal   # 输出为空
go vet ./...
go build ./...
go test ./internal/config ./internal/metrics -count=1
```

Go 工具链：构建与 CI 使用 Go 1.27 线的最新补丁（CI 的 `backend`/`e2e`/`windows` 任务通过 `go-version: '1.27.x'` 解析最新 1.27 补丁，`security` 任务用 `go-version: stable` 以扫描最新稳定版标准库），生产构建镜像为 `golang:1.27-alpine` 的固定 digest。`backend/go.mod` 的 `go 1.26.0` 只是依赖所需的最低 Go 版本，不随补丁升级，也不需要 `toolchain` 指令。

CI（[.github/workflows/ci.yml](../.github/workflows/ci.yml)）在 `backend`（Linux、真实 PostgreSQL + Redis、`-race`）、`frontend` 与 `e2e` 之外还运行：`security` 任务的 `govulncheck ./...`（阻断门禁；用最新 stable Go 扫描标准库）、`windows` 任务（Windows 上执行上面这组 gofmt/vet/build 与不依赖外部服务的单测，命令相同，本机 Windows 上可直接复核）和 `infra` 任务（`docker build -t aegisflow-api:ci ./backend`、占位环境变量下的 `docker compose config -q`、网关 `nginx -t`）。Windows 任务的测试范围只有 `internal/config` 与 `internal/metrics`，完整测试套件仍以 `backend` 任务为准。

分层测试覆盖 auth 核心（持久化、并发轮换/重放撤销、remember、绝对期限、版本迁移、多连接池会话互通、改密/重置/禁用/角色/最后管理员保护、会话列表与撤销、审计、过期清理）、projects 注册表（校验、大小写唯一、归档只读与恢复、分页与状态过滤、事务内审计、可见性读过滤与成员增删改、创建者不可移除与 404 枚举语义）、构建流水线（触发校验与每项目编号、成功与失败退出码、超时、取消与幂等、重启恢复为 interrupted、跨副本 worker 租约互斥与待命接管、SSE 推送与写超时豁免/终态收尾/非法游标、游标日志分页与截断、RBAC 与未接线 503）、部署目标（校验与默认值、大小写唯一与冲突、凭据 AES-256-GCM 加密与 AAD 绑定、更新时保留/替换/清空语义、非配置密钥时拒绝写入、探测成功/非 2xx/超时/网络错误与结果落库、事务内审计、admin-only RBAC 与未接线 503）、部署任务（项目/目标/版本/buildId 校验、每项目编号分配与目标快照、worker 成功/失败/超时/取消/重启恢复、部署后健康复查（首次通过/重试后通过/始终不健康/关闭）、事件游标分页、回滚版本选择与 400/409、事务内审计、目标被引用时 409、worker 租约互斥、项目写权限叠加与可见性 RBAC）、审查中心（subject/description/version/externalUrl 校验与项目内自增编号、并发创建不重号、可见性 404 与写权限 403/404、maintainer/admin 决策矩阵与发起人取消、pending→approved/rejected/cancelled 状态机与幂等、终态 409、事务内审计且 detail 不含审查正文、分页与状态过滤）、签名外部结果回写（HMAC 头/时间窗/内容校验/未知审查 404/幂等 200 且只一行/可见性读取/未配置 503）与审查通知 outbox（四类事件各一行、幂等重复不重复入队、payload 不含密钥或请求头、httptest 验证签名/header/重试/成功后 delivered/claim lease）、安全扫描与攻击防御事件（入站签名头/时间窗/严格 JSON/字段边界校验、delivery 幂等且只存一行、未知项目 404、项目可见性与全局列表过滤、防御事件 admin-only 读/确认与筛选、确认幂等且审计不含 summary 原文、未配置 503、OpenAPI 双向契约）、HTTP（Cookie flags、Secure 显式配置、默认/session/持久模式、轮换、幂等退出、缺 Cookie、JSON 降级拒绝、CSRF、credentialed CORS、Bearer/RBAC、严格 JSON、限流/错误封装、账号管理与项目/构建/部署目标/审查/扫描/防御事件端点、未接线依赖 503）、真实进程生命周期（编译真实二进制、随机端口、readyz、登录与项目创建、Windows CTRL_BREAK/SIGTERM 优雅退出且退出码 0、重启后 access token/refresh Cookie/项目数据存活），以及真实 httptest.Server + CookieJar 登录→刷新→退出链路。

## 架构

cmd/server 负责配置、PostgreSQL 连接池、Redis、迁移与标准库 http.Server 生命周期（监听器先 net.Listen 再 Serve，日志报告实际绑定地址；worker 通过租约启动并随信号上下文退出；进程生命周期验收测试见 cmd/server/ops_test.go）；cmd/rekey 是一次性维护命令，用当前钥与退役钥把存量部署凭据重加密（默认 dry run、`-apply` 才写）；internal/config 校验连接 URL、连接数、登录配额、凭据密钥与可信代理；internal/dbmigrate 拥有唯一版本化迁移账本（auth 表与后续模块表共用）；internal/auth 负责用户/密码/会话/JWT；internal/projects 是项目注册表（校验、活跃/归档生命周期、可见性与成员、分页与事务内审计，读写都带可见性谓词，不依赖 Gin）；internal/builds 是构建流水线（触发与编号、单 worker 租约、SKIP LOCKED 领取、临时目录执行、取消看门狗、批量日志与重启恢复，不依赖 Gin）；internal/metrics 是零依赖的 Prometheus 文本暴露（HTTP 计数/延迟直方图 + 构建状态 gauge，独立监听）；internal/deployments 是部署目标（CRUD 与探测 admin-only）与部署任务、审批（按项目写权限授权、按项目可见性过滤；目标注册表、AES-256-GCM 凭据加密、连通性探测、部署编号/快照/状态机、租约 worker、事件游标、回滚、审批与事务内审计，不依赖 Gin）；internal/reviews 是审查中心（项目内审查编号在项目行锁下分配、pending→approved/rejected/cancelled 状态机、maintainer/admin 决策与发起人取消、事务内审计与可见性过滤、外部结果幂等写入与倒序读取、review.* 通知 outbox 与 notifier；HMAC 验签留在 httpapi，服务层不依赖 Gin）；internal/ingest 是三个签名入站契约共用的载荷规则（delivery id、单行/摘要清洗、展示 URL）；internal/scans 是扫描结果登记（delivery 幂等写入、项目可见性过滤的列表/详情、projectName join，不依赖 Gin）；internal/securityevents 是防御事件登记（签名写入、admin-only 列表/详情、open→acknowledged 确认与事务内审计，不依赖 Gin）；internal/ratelimit 负责 Redis 共享限流；internal/httpapi 负责 Gin 路由与 Cookie、Origin、CORS、Bearer/RBAC，以及扫描/防御事件/外部结果三个入站接口共用的 HMAC 验签。JSON 解码按请求校验，不更改 Gin 全局 binding。Gin 1.12 与 Go 1.27 最新补丁兼容（`go.mod` 最低要求 Go 1.26）。

本地双实例样例见 [compose.yaml](../compose.yaml) 与 [部署说明](../deploy/README.md)。应用副本可横向扩展，但数据库、Redis、入口网关的高可用需另行部署；样例单节点依赖不等于整套服务高可用。数据库集成测试未配置 URL 会明确 Skip，不能将跳过当作生产验证通过。