# 首版部署、权限与浏览器鉴权约定

## 范围和部署决定

产品目标已明确为多人使用、多实例部署。采用 PostgreSQL 共享用户与会话、Redis 共享登录限流，多个 Go/Gin 应用副本通过负载均衡提供同源接口，无需粘性会话。SQLite 不再作为运行存储，旧开发数据不自动导入。业务数据访问按模块逐步抽出 Repository，Gin Context 不进入业务服务。构建 worker 为进程内单并发；副本之间用 PostgreSQL 会话级 advisory lock 竞争同一 worker 租约，只有持锁副本恢复中断构建并领取任务，其余副本待命并在锁释放后接管；领取继续使用 SKIP LOCKED 兜底。该租约保证同一时刻只有一个 worker，不提供并行吞吐。多租户尚未实现；项目可见性与成员（服务端读过滤、写权限叠加与前端成员管理）已交付。

数据库迁移与首次管理员创建使用 advisory 事务锁；刷新与退出锁定共享 session 行，跨实例并发消费旧令牌会触发一致撤销。所有实例须使用一致 JWT 密钥、会话配置与 Redis namespace。登录限流失败返回503，无进程内fallback。入口代理CIDR由 AEGIS_TRUSTED_PROXIES 明确指定，默认不信任转发头。部署样例和运维边界见 [部署说明](../deploy/README.md)。

推荐通过一个 HTTPS 域名提供前端，并把 /api 代理到 Go 服务。开发使用 http://localhost:5173，Vite 已把 /api 代理到 127.0.0.1:8080。AEGIS_ALLOWED_ORIGINS 必须包含浏览器实际 Origin（协议、主机、端口完全匹配）。生产设置 AEGIS_COOKIE_SECURE=true。不要混用 localhost 和 127.0.0.1。

运行指标：设置 `AEGIS_METRICS_ADDR`（默认关闭）时，服务在**独立监听地址**提供 `GET /metrics`，输出 Prometheus 文本格式（`text/plain; version=0.0.4`），含 HTTP 请求计数/延迟直方图与构建状态 gauge，标签使用路由模板而不是原始路径。该端点没有用户鉴权（scraper 无法持会话），只应绑定回环地址或仅经受保护的代理暴露，且不要与 API 端口共用。

## 真实接口契约

机器可读契约见 [OpenAPI](../backend/openapi.yaml)，启动参数见 [后端文档](../backend/README.md)。响应使用统一 data/error/requestId 包装；时间字段命名含义和单位必须在各业务 schema 明确。

| 接口 | 请求 | 响应及效果 |
| --- | --- | --- |
| POST /api/v1/auth/login | JSON username/password/remember | data.user/accessToken/expiresIn；设置 refresh Cookie；禁用账号密码正确时 403 account_disabled |
| POST /api/v1/auth/refresh | JSON {}，携带 Cookie | 轮换 Cookie，返回新 accessToken 和服务端用户 |
| POST /api/v1/auth/logout | JSON {}，携带 Cookie | 撤销整个会话并删除 Cookie；幂等 |
| GET /api/v1/auth/me | Bearer accessToken | 读取数据库会话与用户当前角色 |
| POST /api/v1/auth/password | Bearer，JSON currentPassword/newPassword | 修改自己密码；撤销除当前会话外的全部会话；当前密码错误返回 401 并记入审计 |
| GET /api/v1/auth/sessions | Bearer | 自己的活跃会话列表，标记当前会话；不返回任何令牌 |
| DELETE /api/v1/auth/sessions | Bearer | 撤销除当前会话外的全部会话，返回撤销数量 |
| DELETE /api/v1/auth/sessions/{id} | Bearer | 撤销自己的指定会话；未知或他人会话 404，重复撤销幂等成功 |
| GET /api/v1/users | 管理员 Bearer，limit/offset | 用户分页 {items,total,limit,offset}，不返回密码哈希 |
| POST /api/v1/users | 管理员 Bearer，JSON 用户字段 | 创建用户，返回 201 |
| PATCH /api/v1/users/{id} | 管理员 Bearer，JSON role/disabled 至少一项 | 变更角色或禁用状态；禁用立即撤销该用户全部会话 |
| POST /api/v1/users/{id}/password-reset | 管理员 Bearer，JSON password | 重置密码并撤销该用户全部会话 |
| GET /api/v1/audit | 管理员 Bearer，limit/offset | 审计分页查询，最新在前 |
| GET /api/v1/projects | Bearer（所有角色），limit/offset/status? | 项目分页，按名称排序；只返回调用者可见的项目 |
| POST /api/v1/projects | admin/member Bearer，JSON 项目字段（含可选 buildCommand、visibility） | 创建项目（active 状态，默认 public），201；创建者为 maintainer |
| GET /api/v1/projects/{id} | Bearer（可见者） | 项目详情；private 且不可见时 404 |
| PATCH /api/v1/projects/{id} | admin/member Bearer，JSON 至少一字段（含 buildCommand、visibility） | 修改字段、归档/恢复或改可见性；归档项目只接受恢复；不可见 404 |
| GET /api/v1/projects/{id}/members | Bearer（可见者） | 成员列表（userId/username/role/createdAt），maintainer 在前 |
| POST /api/v1/projects/{id}/members | admin/member Bearer，JSON `{userId,role}` | 201；重复成员 409，账号不存在 404，不可见 404 |
| PATCH /api/v1/projects/{id}/members/{userId} | admin/member Bearer，JSON `{role}` | 改成员角色；无该成员 404 |
| DELETE /api/v1/projects/{id}/members/{userId} | admin/member Bearer | 移除成员；创建者不可移除（400） |
| POST /api/v1/projects/{id}/builds | admin/member Bearer，JSON branch? | 入队构建，201（queued）；项目须活跃且配置 buildCommand；不可见 404 |
| GET /api/v1/builds | Bearer（所有角色），limit/offset/projectId? | 构建分页，最新在前；只含可见项目的构建 |
| GET /api/v1/builds/{id} | Bearer（可见者） | 构建详情（状态、命令快照、时间戳）；不可见 404 |
| GET /api/v1/builds/{id}/logs | Bearer（可见者），after/limit | 日志事件 {items}，按事件 id 升序；after 为游标（SSE 回退通道）；不可见 404 |
| GET /api/v1/builds/{id}/events | Bearer（可见者），after 或 Last-Event-ID | SSE：status/logs/done 帧与心跳；按事件 id 续传；不可见 404 |
| POST /api/v1/builds/{id}/cancel | admin/member Bearer，JSON {} | 取消排队/运行中的构建；重复取消幂等，终态 409；不可见 404 |
| POST /api/v1/builds/{id}/retry | admin/member Bearer，JSON {} | 为终态构建入队新构建（201，queued）；沿用原构建分支、使用项目当前命令；排队/运行中 409；不可见 404 |
| GET /api/v1/projects/{id}/reviews | Bearer（可见者），status/limit/offset | 审查分页，最新在前；不可见项目 404 |
| POST /api/v1/projects/{id}/reviews | Bearer + 项目写权限，JSON `{subject,description?,version?,externalUrl?}` | 201（pending）；项目内 `number` 自增；不可见 404 |
| GET /api/v1/reviews | Bearer（所有角色），limit/offset/projectId?/status? | 全局审查分页，最新在前；仅含可见项目的审查，不可见 `projectId` 过滤返回空列表（不可枚举） |
| GET /api/v1/reviews/{id} | Bearer（可见者） | 审查详情；不可见 404 |
| POST /api/v1/reviews/{id}/approve | Bearer + 全局 admin 或项目 maintainer，JSON `{reason?}` | 200；重复批准幂等、终态 409 |
| POST /api/v1/reviews/{id}/reject | Bearer + 全局 admin 或项目 maintainer，JSON `{reason}`（必填） | 200；重复拒绝幂等、终态 409 |
| POST /api/v1/reviews/{id}/cancel | Bearer + 发起人或 admin/maintainer，JSON {} | 200；重复取消幂等、终态 409 |
| POST /api/v1/reviews/{id}/external-results | **HMAC 签名（无用户会话）**：`X-AegisFlow-Timestamp`（±300s）+ `X-AegisFlow-Signature`（HMAC-SHA256）+ `X-AegisFlow-Delivery`（8–128 `[A-Za-z0-9._-]`），JSON `{source,verdict,summary?,url?}` | 201；重复 `deliveryId` 幂等 200 返回既有记录；签名/时间戳/头缺失 401；未知审查 404；未配置 `AEGIS_REVIEW_WEBHOOK_SECRET` 503 |
| GET /api/v1/reviews/{id}/external-results | Bearer（可见者） | `{items}`，按 receivedAt 倒序最多 50 条；不可见 404 |

accessToken 仅存页面内存，不放 localStorage/sessionStorage。refresh token 不返回 JSON，由 HttpOnly、SameSite=Strict、Path=/api/v1/auth 的 Cookie 保存，无 Domain。勾选保持登录时 Cookie 持久化到绝对会话期限，未勾选使用 session Cookie；浏览器恢复会话可能保留 session Cookie，因此不承诺关闭窗口立即退出。主动退出以服务端撤销为准。默认绝对期限 30 天，由后端配置决定，刷新不延长。

所有鉴权 POST 需要有效白名单 Origin，并拒绝 Sec-Fetch-Site: cross-site。CORS 凭据只对精确白名单来源开放。即使调用者不是浏览器，也必须显式传 Origin；不提供 JSON refresh token 降级通道。跨站部署不在首版支持范围。

### 构建执行边界（P3 第一批）

- 构建状态为 queued/running/succeeded/failed/cancelled/interrupted；interrupted 由持有 worker 租约的实例在启动恢复时产生，表示进程中断而非业务失败。
- 触发时固化命令、分支、仓库地址（repoUrl）与触发人快照，构建号在项目内自增；命令在临时目录用系统 shell 执行，超时由 AEGIS_BUILD_TIMEOUT 控制。开启 `AEGIS_BUILD_CLONE=true` 且项目有 repoUrl 时，先用 `git clone --depth 1 --single-branch --branch <分支>` 拉到 `repo/` 再在其中执行；clone 在宿主侧完成（容器无网络）。只支持无凭据的 https / `git@` 地址，URL 含内联凭据直接失败，私有仓库凭据本批不支持；`file:///` 与绝对路径需 `AEGIS_BUILD_ALLOW_LOCAL_REPO=true`（默认关闭）。clone 失败会记录脱敏摘要并把构建置 failed。
- 单次构建的工作区大小受 `AEGIS_BUILD_DISK`（默认 2g）约束：worker 每 2 秒统计目录大小，超限即取消构建、杀容器并置 failed；这是看门狗而非文件系统配额，允许一个轮询周期内的少量越界。遍历中出现任何错误（目录不可读、stat 失败、条目在统计途中消失）都视为无法确认上限，立即取消构建、置 failed 并在 system 日志说明原因（fail closed）。
- 日志与状态提供两条读取通道：`GET /builds/{id}/events` 的 SSE 推送（`status`/`logs`/`done` 帧，`Last-Event-ID` 或 `after` 续传，15s 心跳，终态后静默收尾，单连接上限 10 分钟，并发连接上限默认 64（超限时新流返回 503 `not_ready`）；服务关停时向活跃流发 `{"reason":"shutdown"}` 帧后立即返回，不阻塞优雅退出）与保留的 `after` 游标轮询。SSE 必须携带 Bearer，因此前端用 fetch 流式读取而不是 `EventSource`，token 不放进 URL。
- 构建不改变账号/权限数据；触发、取消与重试在状态变更的同一事务写入审计（detail 不含命令），日志读取沿用全局角色授权（admin/member 可写，viewer 只读），每次请求由服务端校验，不依赖前端隐藏按钮。
- 日志脱敏已实现：写入 `build_log_events` 前，对 stdout/stderr 与 system 行按既定模式（`AEGIS_*=...`、`Bearer ...`、`Authorization: Basic <base64>`/`Basic <base64>`、任意 scheme 的带内联凭据 URL（含 `postgres://`/`redis://`）、常见 token 前缀、JWT、私钥头）替换为 `[redacted]`，构建结束时追加一条 system 统计行（只记数量，不记原值）。命中却无法确认已移除的行整行丢弃并计数（fail closed，D6）。多行 PEM 私钥由跨行状态机处理：从 BEGIN 到匹配 END 的每一行都写 system `[redacted]`，块内内容不落库，缺少 END 时该流后续行持续脱敏；自由文本里的口令仍不在覆盖范围内。
- 执行环境：`AEGIS_BUILD_SANDBOX=container`（生产推荐）时每个构建运行在一次性容器中——`--network none`、只读根文件系统、`--cap-drop ALL`、`no-new-privileges`，只有构建目录可写，CPU/内存/进程数由 `--cpus`/`--memory`/`--pids-limit` 硬限制；非 Windows 平台容器还以服务进程的 uid/gid 通过 `--user <uid>:<gid>` 运行，使挂载的 0700 工作区可写且容器内非 root（Windows 不加该参数，由 Docker Desktop 做用户映射）；超时、取消或服务退出时会显式杀掉容器。`user` 模式在 Linux 上把 clone 与命令都经 `setpriv` 降权到 `AEGIS_BUILD_USER`，工作区 chown 给该账号，服务必须以 root 启动、启动时校验账号、账号 uid 不为 0（root 不能作为构建账号）与 `setpriv`，Windows 不支持。`none`（默认，仅开发）仍以服务进程身份执行，没有专用执行账号与资源上限。所有模式都只注入最小环境变量白名单（不继承 `AEGIS_*` 服务凭据），`HOME` 与临时目录都指向 0700 的构建目录。

### 部署目标与密钥（P4 第一批）

- `/api/v1/deployment-targets` 的列表、创建、详情、修改、删除与探测六个接口仅全局 admin 可用；member/viewer 一律 403。部署任务、回滚与审批见下文章节（P4.2/P4.3）。
- 目标是全局对象。字段：name（3–64 字节、单行、大小写不敏感唯一）、kind（目前仅 `http`）、environment（`development`/`staging`/`production`，默认 development）、endpoint（http/https，禁止 userinfo、query、fragment）、healthPath（默认 `/`，必须以 `/` 开头，≤256 字节）、status（`active`/`disabled`）。
- 凭据加密存储：`AEGIS_SECRET_KEY`（base64 恰好 32 字节）启用 AES-256-GCM，密文布局 `[12 字节随机 nonce][ciphertext+tag]`，AAD 为 `deployment_target:<目标 id>`，因此密文不能跨目标复用。凭据只在探测请求内解密并以 `Authorization: Bearer <secret>` 发送；接口只返回 `hasSecret`，审计、日志与错误信息都不含凭据。未配置密钥时仍可创建无凭据目标，写入凭据返回 400，不会退化为明文存储。带凭据的目标必须使用 `https` 端点（仅回环地址允许 `http`），否则创建/更新返回 400。轮换：新钥放 `AEGIS_SECRET_KEY`、退役钥放 `AEGIS_SECRET_KEY_PREVIOUS`（只解密、不加密；两键相同或只配退役钥时启动失败），存量密文由维护命令 `go run ./cmd/rekey`（默认 dry run、`-apply` 才写）重加密，可重复执行；全部完成后才能移除退役钥。
- 目标选项：`GET /api/v1/deployment-targets/options` 对**任何已登录角色**返回启用中目标的只读投影（id/name/environment/endpoint），供部署表单选择；完整列表、健康/部署路径、凭据与写操作仍 admin-only。
- 连通性探测：`POST /api/v1/deployment-targets/{id}/check` 对 `endpoint + healthPath` 发 GET，超时由 `AEGIS_DEPLOY_CHECK_TIMEOUT`（默认 5s，最大 1m）控制；2xx 记为健康，其余记录 `http <code>`、`timeout` 或脱敏后的网络错误，并更新 `last_check_at/last_check_ok/last_check_detail`。探测结果与 `deployment_target.check` 审计同事务提交。默认客户端最多 3 跳、不跨主机，且拒绝 `https`→`http` 降级。
- 审计动作：`deployment_target.create` / `deployment_target.update` / `deployment_target.remove` / `deployment_target.check`（objectType `deployment_target`），detail 只写名称或探测结果。删除为硬删除，但被部署历史引用的目标返回 409（保留历史快照）。

### 部署任务（P4.2）

- 接口：`POST|GET /api/v1/projects/{projectId}/deployments`、`GET /api/v1/deployments?projectId&status&limit&offset`（全局列表，与构建列表同形，按可见性过滤）、`GET /api/v1/deployments/{id}`、`GET /api/v1/deployments/{id}/events?after&limit`、`POST /api/v1/deployments/{id}/cancel`、`POST /api/v1/deployments/{id}/rollback`；授权按项目写权限叠加：admin 全量，member 对可写项目（public，或 private 且为 maintainer）可创建/取消/回滚，viewer 只读不可写；列表、详情与事件按项目可见性过滤，不可见一律 404。审批接口仍为 admin-only。
- 协议：对目标执行 `POST <endpoint 去尾斜杠><deployPath>`（`deployPath` 默认 `/deploy`），JSON 体 `{deploymentId, project, version, environment, rollback, rollbackVersion}`；目标有凭据时携带 `Authorization: Bearer`，只在执行内存解密。2xx 之后还会按目标的 `healthPath` 复查（默认 3 次、间隔 2s，`AEGIS_DEPLOY_HEALTH_ATTEMPTS` 设为 0 可关闭）：复查 2xx 才记 `succeeded`，始终不健康则记 `failed`（detail `health check failed: <原因>`，每次复查都写一条 system 事件）；部署端点非 2xx、超时（`AEGIS_DEPLOY_TIMEOUT`，默认 10m，最大 1h）与网络错误同样记失败，detail 脱敏且不含凭据；部署请求本身不重试。
- 状态：`queued → running → succeeded/failed/cancelled`，重启恢复把遗留 `running` 置 `interrupted`；命中审批环境时创建即 `awaiting_approval`（见「部署审批（P4.3）」）。单 worker 持独立 advisory 租约，`SKIP LOCKED` 领取，300ms 看门狗在取消/终态时中止 HTTP 请求。
- 回滚：仅对 `succeeded`/`failed` 的源部署可用，取同项目+目标中比源更早的最近一次 `succeeded` 版本，新建一条部署（`rollbackOf` 指向源，payload `rollback=true`、`rollbackVersion` 为该版本；命中审批环境时同样进入 `awaiting_approval`）；无更早成功返回 400，源非终态 409；重复回滚会创建多条部署（部署非幂等）。
- 审计：`deployment.create`（detail `target=<name> version=<v> approval=required|none`，命中审查门禁时追加 ` review=<id>`）、`deployment.cancel`（`from=<status>`）、`deployment.rollback`（`rollback_of=<id> version=<v>`，命中审查门禁时追加 ` review=<id>`），objectType `deployment`，与状态变更同事务；重复取消幂等且不重复记账。
- 审查门禁（P5-A E2）：`AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS`（逗号分隔的 development/staging/production，默认空 = 不启用，显式空值同样关闭）列出的目标环境要求部署创建/回滚必须引用**已批准**审查。请求体可选 `reviewId`；提供时必须属于同一项目且 `status=approved`，不存在、不可见或未批准统一 400 `validation_failed`（不可枚举）；环境命中门禁列表时 `reviewId` 必填。部署行以 `review_id` 快照该审查（无审查为 NULL），响应含 `reviewId`；门禁在审批状态机之前校验，与 P4.3 审批叠加。门禁默认关闭，升级后既有部署行为不变。

### 部署审批（P4.3）

- 只有 `AEGIS_DEPLOY_APPROVAL_ENVIRONMENTS` 中列出的目标环境（逗号分隔，默认 `production`；显式留空表示不需要审批）需要审批，`AEGIS_APPROVAL_TTL` 默认 24h（范围 1h–168h）。
- 创建部署或回滚时命中审批环境：同一事务写入 `awaiting_approval` 部署、`pending` 审批（`expiresAt=now+TTL`）、`deployments.approval_id`，并写 `deployment.create`/`deployment.rollback` 与 `approval.request` 两条审计；其他环境保持 `queued`。
- 接口（全部 admin-only，member/viewer 403）：`GET /api/v1/approvals?status=&limit=&offset=`、`POST /api/v1/approvals/{id}/approve`（body `{}`）、`POST /api/v1/approvals/{id}/reject`（body `{reason?}`，reason 可选、trim、单行、≤512 字节、拒绝控制字符）。
- 批准：审批 `approved`，部署 `awaiting_approval → queued` 并立即唤醒 worker；重复批准返回 200 与同一审批且不重复写审计。拒绝：审批 `rejected`（保存 reason），部署 `rejected`（detail `approval rejected`）；重复拒绝同样幂等。
- 冲突：审批非 pending，或对应部署已不再 `awaiting_approval`（例如已被取消）时返回 409；此时审批保持 pending，由过期扫描关闭。
- 过期：持租约的 worker 每 30s（接管时先跑一次）扫描 `pending AND expires_at<=now`，每批最多 100 条、`FOR UPDATE SKIP LOCKED`；审批 `expired`（reason `approval expired`），部署 `rejected`（detail `approval expired`），每行写 `approval.expire` 审计（actor 为空）。
- 取消交互：取消 `awaiting_approval` 部署时同一事务把审批置 `expired`（reason `deployment cancelled`，decided_at=now）并写 `approval.expire`；审批状态机没有 cancelled，因此复用 expired，重复取消不重复记账。
- 审计：`approval.request`（`deployment=<id>`）、`approval.approve`（`deployment=<id>`）、`approval.reject`（`deployment=<id> reason=<...>` 可选）、`approval.expire`（`deployment=<id> reason=approval expired|deployment cancelled`），objectType `approval`，与状态变更同事务。
- 通知（可选，P4.4）：`AEGIS_APPROVAL_WEBHOOK_URL` 非空时，审批请求与批准/拒绝/过期事件在和状态变更同一事务写入 outbox，并由独立 notifier 以至少一次语义投递。请求体是事件快照，不含凭据；可配置 `AEGIS_APPROVAL_WEBHOOK_SECRET` 做 HMAC-SHA256 签名，接收方按 `X-AegisFlow-Delivery` 幂等去重。投递失败重试后置 `failed`，不影响审批状态，也不写新的业务审计动作。URL 除回环地址外必须 https，超时默认 5s（最大 30s）。同一通道自 P5-A E4 起也投递 `review.*` 事件（`review_notifications` outbox），接收方按 `X-AegisFlow-Event` 前缀路由，签名与重试语义一致。

### 审查中心（P5-A，门禁式）

- 定位：对项目的一次变更（版本标签或外部构建链接）发起审查，由项目 maintainer 批准或拒绝，全程审计，并允许发起人/维护者取消；E2 起审查可接入部署门禁（见「部署审查门禁」），E4 起外部工具可通过签名接口回写只读结果并触发审查事件通知。
- 接口：`POST|GET /api/v1/projects/{projectId}/reviews`、`GET /api/v1/reviews`（全局列表，任何已登录角色，按项目可见性过滤，支持 `projectId`/`status`/分页）、`GET /api/v1/reviews/{id}`、`POST /api/v1/reviews/{id}/approve|reject|cancel`、`GET /api/v1/reviews/{id}/external-results`（外部结果只读列表，项目可见者）。
- 授权：发起复用项目写权限（`projects.AuthorizeWrite`：admin，或可写项目的 member；不可见 404、可见但无权 403）；批准/拒绝要求**全局 admin 或该项目 maintainer**（`projects.AuthorizeDecide`，比写权限更严，public 项目的普通 member 不能决策）；取消允许发起人本人或 admin/maintainer。项目内列表与详情按项目可见性过滤，不可见一律 404；全局列表同样按可见性过滤，但 `projectId` 过滤不可见项目时返回空列表（与 `GET /api/v1/deployments` 一致，不可枚举）。
- 校验：`subject` 必填、3–200 字节、单行（trim）；`description` ≤2000 字节；`version` ≤64 字节单行（trim）；`externalUrl` 可选、仅 http(s)、禁止 userinfo/fragment、≤2048 字节，只展示不抓取；批准/拒绝的 `reason` trim、单行、≤512 字节，拒绝必填。
- 编号与状态：`number` 在 `projects` 行锁下项目内自增；状态机 `pending → approved | rejected | cancelled`，终态不可变，非 pending 决策返回 409；已处于目标终态的重复请求幂等返回 200 且不重复审计。`decidedBy/decidedAt` 记录决策或取消者与时间，`reason` 记录批准/拒绝理由。
- 审计：`review.request`（detail `project=<id> number=<n>`）、`review.approve` / `review.reject` / `review.cancel`（objectType `review`，detail 只写编号与理由），与状态变更、编号分配同事务提交；审查正文（subject/description/version/externalUrl）不进入审计。
- 迁移：schema v11 新增 `reviews` 表、`projects.review_count`、`UNIQUE(project_id,number)` 与 `(project_id,status,created_at)`、`(status,created_at)` 索引；schema v12 为 `deployments` 增加可空 `review_id`（引用 `reviews(id)`、`ON DELETE SET NULL`）与部分索引。

### 部署审查门禁（P5-A E2）

- 配置：`AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS` 为逗号分隔的 `development`/`staging`/`production` 子集；默认空 = 不启用，显式空值同样关闭（与审批默认 `production` 不同，门禁必须显式开启），非法环境值启动失败。
- 校验：创建部署与回滚的请求体可带可选 `reviewId`。提供时必须引用**同一项目**的 `approved` 审查；不存在、不可见、跨项目或未批准统一返回 400 `validation_failed`，不保留可用于枚举的差异。目标环境命中门禁列表时 `reviewId` 必填，缺失同样 400。
- 快照与审计：部署行的 `review_id` 记录创建时的审查，接口响应的 `reviewId` 为空表示未走门禁；`deployment.create` / `deployment.rollback` 的审计 detail 追加 ` review=<id>`，不写审查正文。
- 与审批叠加：门禁在审批状态机之前校验；命中审批环境时仍按 P4.3 进入 `awaiting_approval`，批准后才执行。门禁默认关闭，升级后既有部署行为不变（历史行 `review_id` 为 NULL，响应 `reviewId` 为空串）。

### 审查外部结果与通知（P5-A E4）

- 外部结果回写（入站）：`POST /api/v1/reviews/{id}/external-results` 是**无会话的机器接口**，由独立的 `AEGIS_REVIEW_WEBHOOK_SECRET`（16–256 字节）保护；未配置时该接口返回 503 `dependencies_unavailable`（消息 `review results are not configured`），设置不合法则启动失败。出站审查通知**复用** `AEGIS_APPROVAL_WEBHOOK_URL`/`AEGIS_APPROVAL_WEBHOOK_SECRET`/`AEGIS_APPROVAL_WEBHOOK_TIMEOUT`，同一接收端同时收到 `approval.*` 与 `review.*` 事件，不新增出站配置。
- 签名与防重放：请求头 `X-AegisFlow-Timestamp`（Unix 秒，与服务器时间差 ≤300s）、`X-AegisFlow-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + rawBody)>`（对原始请求体字节签名，不是对 JSON 重编码后的内容）、`X-AegisFlow-Delivery`（8–128 字符 `[A-Za-z0-9._-]`，全局幂等键）。缺头、签名错误、时间戳超出窗口或格式非法统一 401；时间窗 + 签名覆盖时间戳可阻止捕获重放，`deliveryId` 使合法重试幂等。
- 载荷与校验：严格 JSON（≤16 KiB、未知/`null` 字段拒绝）`{source,verdict,summary?,url?}`；`source` 必填 1–64 字节单行，`verdict ∈ passed|failed|warning`，`summary` ≤500 字节（保留换行、去除其他控制字符），`url` 可选、仅 http(s)、无 userinfo/fragment、≤2048 字节。同一 `deliveryId` 重复回写返回 200 + 既有记录（不新增）；未知审查 404；非法字段 400。**外部结果不改变审查状态机，也不改变部署门禁判定**，只是只读证据。
- 读取：`GET /api/v1/reviews/{id}/external-results`（Bearer，项目可见者）按 `receivedAt` 倒序最多 50 条；不可见/不存在 404。响应不含签名头或密钥。
- 出站通知：`review.requested`（创建）、`review.approved`/`review.rejected`（决策）、`review.cancelled`（取消）在与状态变更/审计同一事务写入 `review_notifications` outbox（`UNIQUE(review_id,event)` 防重复），payload 含 `deliveryId/event/occurredAt/project{id,name}/review{...快照...}`，不含凭据、Cookie、令牌或请求头；由独立 notifier 以 1s 轮询、`FOR UPDATE SKIP LOCKED` + claim lease、10s 起指数退避（上限 1h、最多 8 次）投递，成功 `delivered`、失败 `last_error` + `pending`/`failed`。投递头 `X-AegisFlow-Event`/`X-AegisFlow-Delivery`/`X-AegisFlow-Signature`（对 payload JSON 本体签名），至少一次语义，接收方按 delivery id 去重。通知失败不回滚审查状态。
- 审计与迁移：外部结果与通知投递**不新增**业务审计动作（外部结果不是本地状态变更；通知重试不写业务审计）。schema v13 新增 `review_external_results` 与 `review_notifications` 两表及索引；两表 `review_id` 外键级联删除。外部结果在前端审查详情页只读回显（来源、中文 verdict 徽章、保留换行的摘要与报告外链），E2E 覆盖签名回写后回显与重复 `deliveryId` 幂等。

## 客户端会话生命周期

1. 启动通过 Cookie 刷新，随后 /auth/me 获取真实用户；等待恢复完成才进入业务布局。
2. 登录只调用真实后端，不接受 mock JWT；旧前端遗留的本地缓存会被清理。
3. access 即将过期时自动刷新；API 客户端对 401 最多刷新并重试一次，403 不刷新。
4. 同页面共享一次刷新，Web Locks 串行化同源多标签页的 Cookie 轮换，BroadcastChannel 通知账号变更和退出。首版要求支持这些 API 的现代浏览器，且运行于 localhost 或 HTTPS 安全上下文；Web Locks 不可用时明确报错。
5. refresh 返回 401 时清空内存并回到登录。网络失败可重试，不把断网误判为账号失效。
6. 退出等待服务端撤销成功才清理状态；网络失败提示重试，不显示虚假退出成功。账号切换和退出清理 React Query 缓存。

## 账号管理、审计与过期清理

- 修改自己密码需验证当前密码，成功后仅保留当前会话；管理员重置密码会撤销目标用户全部会话。两者都要求新密码 12-72 字节。
- 禁用账号立即撤销其全部会话并阻止登录；禁用状态只在密码正确时通过 403 account_disabled 透露，不会成为账号枚举信号。重新启用不恢复旧会话。
- 角色为全局角色，每次鉴权实时读取数据库，角色变更对既有会话即时生效，无需撤销会话。禁用或降级最后一个可用管理员被拒绝（400），防止系统失去管理入口。
- 持久化审计记录操作人（ID/用户名）、时间、动作、对象、结果、详情与请求 ID：登录成功、创建用户、修改密码（含当前密码错误的失败记录）、重置密码、禁用/启用、角色变更、会话撤销、项目成员增删改（project.member_add / project.member_update / project.member_remove）、构建触发（build.trigger）、取消（build.cancel）与重试（build.retry）、部署目标增删改与探测（deployment_target.create / deployment_target.update / deployment_target.remove / deployment_target.check）、部署创建/取消/回滚（deployment.create / deployment.cancel / deployment.rollback）、审批请求/批准/拒绝/过期（approval.request / approval.approve / approval.reject / approval.expire）、审查请求与批准/拒绝/取消（review.request / review.approve / review.reject / review.cancel，objectType `review`）。构建、成员、部署目标、部署任务、审批与审查审计都和状态变更同事务提交；重复取消/重复审批/重复审查决策幂等且不重复记账，detail 只写项目/成员与角色或目标名称与探测结果，不包含构建命令或凭据（重试写 `retry_of=<原构建号>`，拒绝写 `reason=<≤512>`，审查写 `number=<n>` 与 reason，不写审查正文；部署创建/回滚命中审查门禁时追加 `review=<id>`，只写审查 id）。失败登录受 Redis 限流约束，只进结构化日志不进审计表；审计查询仅管理员。
- 审计查询支持服务端筛选：`action`、`objectType`、`result`（success/failure）、`actor`（id 或用户名，用户名大小写不敏感）、`from`/`to`（RFC3339 或 `YYYY-MM-DD`，只给日期时 `to` 覆盖该 UTC 全天）。筛选同时作用于分页结果与 total，多个条件按 AND 组合；非法时间或未知 result 返回 400。审计页提供动作/对象/结果/操作人/日期范围筛选与重置。
- 审计保留策略：`AEGIS_AUDIT_RETENTION` 可选，未设置或为 `0` 时**永久保留**（默认，审计不应默认丢证据）；设置为 24h–8760h 之间的时长时，后台任务每小时删除早于该窗口的审计行（启动时先执行一次），与过期会话清理共用同一个循环。删除是运维显式选择，而不是按业务对象做归档。
- 过期会话及其 refresh 令牌历史由后台任务每小时清理（启动时先执行一次）。重放检测只需保留到会话绝对期限，到期即删；清理不影响活跃会话。

## 角色矩阵

下面是首版权限约定。除已提供的鉴权、账号管理、审计、项目、构建、部署目标、部署任务、审批与审查接口，其他服务尚未实现；页面守卫用于交互，不能代替后端对象级授权。角色是**全局的**；项目可见性（public/private）与项目成员（maintainer/viewer）已实现（P2；项目级权限设计文档本地维护，不入库）：成员身份既决定「能看哪个项目」，也决定「能改哪个项目」。

**可见性规则**：全局 admin 永远可见；其他角色可见 public 项目，以及自己创建或被加入的 private 项目。不可见一律返回 **404**（不是 403），避免用私有 id 枚举项目。构建、部署、日志与 SSE 继承所属项目的可见性。

**写权限**：全局角色是上限——viewer 永不可写、admin 永远可写；private 项目另外要求调用者是该项目 maintainer，否则 **403**（可见但无权）。public 项目保持「任意 member 可写」。项目修改、归档、触发/取消/重试构建、成员增删改都走同一条判定（`projects.AuthorizeWrite`）。成员变更写审计：`project.member_add` / `project.member_update` / `project.member_remove`（objectType project）。

| 操作 | admin | member | viewer | 实现状态 |
| --- | --- | --- | --- | --- |
| 读取自己的资料、退出、修改自己密码、查看/撤销自己的会话 | 允许 | 允许 | 允许 | 已实现 |
| 创建/查询用户、重置密码、禁用/启用、变更角色、查询审计 | 允许 | 禁止 | 禁止 | 已实现 |
| 读取项目（public，或自己创建/已被加入的 private） | 允许 | 允许 | 允许 | 已实现（可见性过滤） |
| 读取其他 private 项目 | 允许 | 禁止（404） | 禁止（404） | 已实现（404 防枚举） |
| 创建/修改项目、归档与恢复、修改可见性 | 允许 | public 允许；private 需项目 maintainer | 禁止 | 已实现（可见性 + maintainer 叠加） |
| 读取项目成员、增删改成员 | 允许 | public 允许；private 需项目 maintainer | 只读列表 | 已实现；创建者不可移除；成员变更写审计 |
| 读取构建/部署/审查/扫描/知识信息 | 允许 | 允许 | 允许 | 构建、部署与审查（含外部结果读取）已实现（按项目可见性过滤）；审批仍 admin-only；扫描、知识待业务接口落实 |
| 发起/取消/重试构建、发起审查 | 允许 | public 允许；private 需项目 maintainer | 禁止 | 构建与审查发起已实现（重试仅限终态，不可见 404；审查发起走同一写权限叠加） |
| 批准/拒绝审查、取消他人审查 | 允许 | 仅该项目的 maintainer；发起人可取消本人的 pending 审查 | 禁止 | 已实现（`projects.AuthorizeDecide`，比写权限更严；终态 409，重复决策/取消幂等 200） |
| 外部系统回写审查结果、接收审查通知 | —（HMAC，无用户会话） | — | — | 已实现（P5-A E4）：`AEGIS_REVIEW_WEBHOOK_SECRET` HMAC-SHA256 + ≤300s 时间窗 + delivery 幂等；只追加只读证据，不改审查状态；审查事件复用审批 Webhook 通道出站 |
| 发起部署、回滚及执行审批 | 允许 | 允许（项目可写时）；审批仅 admin | 禁止 | 部署触发与回滚已实现（项目写权限叠加，P4.2）；P5-A E2 增加可配置审查门禁：目标环境命中 `AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS` 时要求已批准审查，默认关闭；审批仍 admin-only（P4.3） |
| 管理部署目标、密钥、策略、设置、防御及业务审计 | 允许 | 禁止 | 禁止 | 部署目标已实现（admin-only，凭据加密存储，P4.1）；独立密钥管理、策略、设置、防御与其余 P5 业务模块（含其审计）待实现 |

后续对象涉及具体项目时，需在服务端查询对象所属项目并校验访问权限。不要根据客户端提交的 role 或隐藏按钮决定授权。

## 开发模式和验收

前端不再提供业务演示数据或演示开关：项目、构建、部署、审查、账号与审计始终调用真实接口，未接入模块恒定显示未实现声明，不用演示数据冒充业务结果。登录与鉴权没有 mock 回退。

后端验证：go test ./...、go vet ./...。前端验证：npm run build、npm run lint、npm test（session store 单元测试）。浏览器验证：先 npx playwright install chromium，再 npm run test:e2e；先设置 AEGIS_TEST_DATABASE_URL/AEGIS_TEST_REDIS_URL 运行后端真实集成测试；浏览器测试需另设置 AEGIS_E2E_DATABASE_URL/AEGIS_E2E_REDIS_URL 指向专用可丢弃服务，测试启动本地前后端，要求 8080、5173 端口空闲。E2E 后端显式设置 AEGIS_LOGIN_RATE_LIMIT=100，因为同一地址在一分钟内驱动多个账号登录；生产默认配额仍是 10。测试账号仅存在测试临时库；端到端配置使用随机 JWT 密钥，不使用生产数据。E2E 的 webServer 还会注入固定的一次性测试密钥 `AEGIS_REVIEW_WEBHOOK_SECRET`，仅供外部结果用例计算 HMAC 签名回写，不进入生产配置。

验收覆盖：错误密码、正确登录、Cookie 属性、页面恢复、并发刷新、多个标签页、退出撤销、非管理员 403、刷新 Cookie 失效及管理员页面守卫；账号管理覆盖 UI 创建用户、角色变更、重置密码、禁用后 403 登录拒绝、审计查询、自助修改密码保留当前会话、双会话撤销其他全部并即时踢出；构建闭环覆盖 UI 创建带构建命令的项目、触发构建、列表与详情、SSE 推送日志中出现进程输出与 build succeeded、viewer 只读且触发 403；项目可见性与成员覆盖 UI 创建私有项目与徽章、成员页邀请查看者、非成员深链得到「未找到」、查看者成员无管理控件且触发构建 403、提升维护者后可触发；部署与审批覆盖部署目标、发起部署、审批、取消与回滚主链路；审查中心覆盖 UI 发起审查（列表对话框）、批准（带理由）与状态/项目筛选、拒绝理由必填与详情展示、viewer 只读（无发起入口且发起 API 403）、非项目成员详情 404，以及部署表单关联已批准审查并在部署详情展示审查链接。审查详情页回显外部工具签名回写的只读结果（来源、中文「通过/失败/警告」徽章、保留换行的摘要、报告外链），E2E 覆盖签名回写后回显与重复 delivery 幂等。外部结果回写与审查通知（P5-A E4）的后端契约由后端集成测试验证签名、幂等与投递，前端外部结果回显与 E2E 由 E4B 补齐。剩余能力（多 worker 并行吞吐、构建阶段与产物、构建缓存、私有仓库凭据、部署多级会签与通知管理 API、多租户与单点登录）与生产备份演练见 PR 记录与本地维护的开发文档（不入库）。
