# 首版部署、权限与浏览器鉴权约定

## 范围和部署决定

产品目标已明确为多人使用、多实例部署。采用 PostgreSQL 共享用户与会话、Redis 共享登录限流，多个 Go/Gin 应用副本通过负载均衡提供同源接口，无需粘性会话。SQLite 不再作为运行存储，旧开发数据不自动导入。业务数据访问按模块逐步抽出 Repository，Gin Context 不进入业务服务。构建 worker 为进程内单并发；副本之间用 PostgreSQL 会话级 advisory lock 竞争同一 worker 租约，只有持锁副本恢复中断构建并领取任务，其余副本待命并在锁释放后接管；领取继续使用 SKIP LOCKED 兜底。该租约保证同一时刻只有一个 worker，不提供并行吞吐。多租户尚未实现；项目可见性与成员已交付第一批（读过滤），写权限叠加与前端成员管理待后续批次。

数据库迁移与首次管理员创建使用 advisory 事务锁；刷新与退出锁定共享 session 行，跨实例并发消费旧令牌会触发一致撤销。所有实例须使用一致 JWT 密钥、会话配置与 Redis namespace。登录限流失败返回503，无进程内fallback。入口代理CIDR由 AEGIS_TRUSTED_PROXIES 明确指定，默认不信任转发头。部署样例和运维边界见 [部署说明](../deploy/README.md)。

推荐通过一个 HTTPS 域名提供前端，并把 /api 代理到 Go 服务。开发使用 http://localhost:5173，Vite 已把 /api 代理到 127.0.0.1:8080。AEGIS_ALLOWED_ORIGINS 必须包含浏览器实际 Origin（协议、主机、端口完全匹配）。生产设置 AEGIS_COOKIE_SECURE=true。不要混用 localhost 和 127.0.0.1。

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

accessToken 仅存页面内存，不放 localStorage/sessionStorage。refresh token 不返回 JSON，由 HttpOnly、SameSite=Strict、Path=/api/v1/auth 的 Cookie 保存，无 Domain。勾选保持登录时 Cookie 持久化到绝对会话期限，未勾选使用 session Cookie；浏览器恢复会话可能保留 session Cookie，因此不承诺关闭窗口立即退出。主动退出以服务端撤销为准。默认绝对期限 30 天，由后端配置决定，刷新不延长。

所有鉴权 POST 需要有效白名单 Origin，并拒绝 Sec-Fetch-Site: cross-site。CORS 凭据只对精确白名单来源开放。即使调用者不是浏览器，也必须显式传 Origin；不提供 JSON refresh token 降级通道。跨站部署不在首版支持范围。

### 构建执行边界（P3 第一批）

- 构建状态为 queued/running/succeeded/failed/cancelled/interrupted；interrupted 由持有 worker 租约的实例在启动恢复时产生，表示进程中断而非业务失败。
- 触发时固化命令、分支、仓库地址（repoUrl）与触发人快照，构建号在项目内自增；命令在临时目录用系统 shell 执行，超时由 AEGIS_BUILD_TIMEOUT 控制。开启 `AEGIS_BUILD_CLONE=true` 且项目有 repoUrl 时，先用 `git clone --depth 1 --single-branch --branch <分支>` 拉到 `repo/` 再在其中执行；clone 在宿主侧完成（容器无网络）。只支持无凭据的 https / `git@` 地址，URL 含内联凭据直接失败，私有仓库凭据本批不支持；`file:///` 与绝对路径需 `AEGIS_BUILD_ALLOW_LOCAL_REPO=true`（默认关闭）。clone 失败会记录脱敏摘要并把构建置 failed。
- 单次构建的工作区大小受 `AEGIS_BUILD_DISK`（默认 2g）约束：worker 每 2 秒统计目录大小，超限即取消构建、杀容器并置 failed；这是看门狗而非文件系统配额，允许一个轮询周期内的少量越界。
- 日志与状态提供两条读取通道：`GET /builds/{id}/events` 的 SSE 推送（`status`/`logs`/`done` 帧，`Last-Event-ID` 或 `after` 续传，15s 心跳，终态后静默收尾，单连接上限 10 分钟）与保留的 `after` 游标轮询。SSE 必须携带 Bearer，因此前端用 fetch 流式读取而不是 `EventSource`，token 不放进 URL。
- 构建不改变账号/权限数据；触发、取消与重试在状态变更的同一事务写入审计（detail 不含命令），日志读取沿用全局角色授权（admin/member 可写，viewer 只读），每次请求由服务端校验，不依赖前端隐藏按钮。
- 日志脱敏已实现：写入 `build_log_events` 前，对 stdout/stderr 按既定模式（`AEGIS_*=...`、`Bearer ...`、带内联凭据的 `postgres://`/`redis://` URL、常见 token 前缀、JWT、私钥头）替换为 `[redacted]`，构建结束时追加一条 system 统计行（只记数量，不记原值）。命中却无法确认已移除的行整行丢弃并计数（fail closed，D6）。多行私钥正文与自由文本里的口令不在模式覆盖范围内。
- 执行环境：`AEGIS_BUILD_SANDBOX=container`（生产推荐）时每个构建运行在一次性容器中——`--network none`、只读根文件系统、`--cap-drop ALL`、`no-new-privileges`，只有构建目录可写，CPU/内存/进程数由 `--cpus`/`--memory`/`--pids-limit` 硬限制；超时、取消或服务退出时会显式杀掉容器。`user` 模式在 Linux 上把 clone 与命令都经 `setpriv` 降权到 `AEGIS_BUILD_USER`，工作区 chown 给该账号，服务必须以 root 启动、启动时校验账号与 `setpriv`，Windows 不支持。`none`（默认，仅开发）仍以服务进程身份执行，没有专用执行账号与资源上限。所有模式都只注入最小环境变量白名单（不继承 `AEGIS_*` 服务凭据），`HOME` 与临时目录都指向 0700 的构建目录。

### 部署目标与密钥（P4 第一批）

- `/api/v1/deployment-targets` 的列表、创建、详情、修改、删除与探测六个接口仅全局 admin 可用；member/viewer 一律 403。部署任务与回滚见下一节（P4.2），审批流尚未实现（P4.3）。
- 目标是全局对象。字段：name（3–64 字节、单行、大小写不敏感唯一）、kind（目前仅 `http`）、environment（`development`/`staging`/`production`，默认 development）、endpoint（http/https，禁止 userinfo、query、fragment）、healthPath（默认 `/`，必须以 `/` 开头，≤256 字节）、status（`active`/`disabled`）。
- 凭据加密存储：`AEGIS_SECRET_KEY`（base64 恰好 32 字节）启用 AES-256-GCM，密文布局 `[12 字节随机 nonce][ciphertext+tag]`，AAD 为 `deployment_target:<目标 id>`，因此密文不能跨目标复用。凭据只在探测请求内解密并以 `Authorization: Bearer <secret>` 发送；接口只返回 `hasSecret`，审计、日志与错误信息都不含凭据。未配置密钥时仍可创建无凭据目标，写入凭据返回 400，不会退化为明文存储。带凭据的目标必须使用 `https` 端点（仅回环地址允许 `http`），否则创建/更新返回 400。
- 连通性探测：`POST /api/v1/deployment-targets/{id}/check` 对 `endpoint + healthPath` 发 GET，超时由 `AEGIS_DEPLOY_CHECK_TIMEOUT`（默认 5s，最大 1m）控制；2xx 记为健康，其余记录 `http <code>`、`timeout` 或脱敏后的网络错误，并更新 `last_check_at/last_check_ok/last_check_detail`。探测结果与 `deployment_target.check` 审计同事务提交。默认客户端最多 3 跳、不跨主机，且拒绝 `https`→`http` 降级。
- 审计动作：`deployment_target.create` / `deployment_target.update` / `deployment_target.remove` / `deployment_target.check`（objectType `deployment_target`），detail 只写名称或探测结果。删除为硬删除，但被部署历史引用的目标返回 409（保留历史快照）。

### 部署任务（P4.2）

- 六个接口：`POST|GET /api/v1/projects/{projectId}/deployments`、`GET /api/v1/deployments/{id}`、`GET /api/v1/deployments/{id}/events?after&limit`、`POST /api/v1/deployments/{id}/cancel`、`POST /api/v1/deployments/{id}/rollback`；全部 admin-only，member/viewer 一律 403。
- 协议：对目标执行 `POST <endpoint 去尾斜杠><deployPath>`（`deployPath` 默认 `/deploy`），JSON 体 `{deploymentId, project, version, environment, rollback, rollbackVersion}`；目标有凭据时携带 `Authorization: Bearer`，只在执行内存解密。2xx 记为成功；其他状态、超时（`AEGIS_DEPLOY_TIMEOUT`，默认 10m，最大 1h）与网络错误记为失败，detail 脱敏且不含凭据；不自动重试。
- 状态：`queued → running → succeeded/failed/cancelled`，重启恢复把遗留 `running` 置 `interrupted`；`awaiting_approval`/`rejected` 预留给 P4.3，本批创建始终直接排队。单 worker 持独立 advisory 租约，`SKIP LOCKED` 领取，300ms 看门狗在取消/终态时中止 HTTP 请求。
- 回滚：仅对 `succeeded`/`failed` 的源部署可用，取同项目+目标中比源更早的最近一次 `succeeded` 版本，新建一条 `queued` 部署（`rollbackOf` 指向源，payload `rollback=true`、`rollbackVersion` 为该版本）；无更早成功返回 400，源非终态 409；重复回滚会创建多条部署（部署非幂等）。
- 审计：`deployment.create`（detail `target=<name> version=<v> approval=none`）、`deployment.cancel`（`from=<status>`）、`deployment.rollback`（`rollback_of=<id> version=<v>`），objectType `deployment`，与状态变更同事务；重复取消幂等且不重复记账。

## 客户端会话生命周期

1. 启动通过 Cookie 刷新，随后 /auth/me 获取真实用户；等待恢复完成才进入业务布局。
2. 登录只调用真实后端，不接受 mock JWT；旧 gp_session 本地缓存会被清理。
3. access 即将过期时自动刷新；API 客户端对 401 最多刷新并重试一次，403 不刷新。
4. 同页面共享一次刷新，Web Locks 串行化同源多标签页的 Cookie 轮换，BroadcastChannel 通知账号变更和退出。首版要求支持这些 API 的现代浏览器，且运行于 localhost 或 HTTPS 安全上下文；Web Locks 不可用时明确报错。
5. refresh 返回 401 时清空内存并回到登录。网络失败可重试，不把断网误判为账号失效。
6. 退出等待服务端撤销成功才清理状态；网络失败提示重试，不显示虚假退出成功。账号切换和退出清理 React Query 缓存。

## 账号管理、审计与过期清理

- 修改自己密码需验证当前密码，成功后仅保留当前会话；管理员重置密码会撤销目标用户全部会话。两者都要求新密码 12-72 字节。
- 禁用账号立即撤销其全部会话并阻止登录；禁用状态只在密码正确时通过 403 account_disabled 透露，不会成为账号枚举信号。重新启用不恢复旧会话。
- 角色为全局角色，每次鉴权实时读取数据库，角色变更对既有会话即时生效，无需撤销会话。禁用或降级最后一个可用管理员被拒绝（400），防止系统失去管理入口。
- 持久化审计记录操作人（ID/用户名）、时间、动作、对象、结果、详情与请求 ID：登录成功、创建用户、修改密码（含当前密码错误的失败记录）、重置密码、禁用/启用、角色变更、会话撤销、项目成员增删改（project.member_add / project.member_update / project.member_remove）、构建触发（build.trigger）、取消（build.cancel）与重试（build.retry）、部署目标增删改与探测（deployment_target.create / deployment_target.update / deployment_target.remove / deployment_target.check）、部署创建/取消/回滚（deployment.create / deployment.cancel / deployment.rollback）。构建、成员、部署目标与部署任务审计都和状态变更同事务提交；重复取消幂等且不重复记账，detail 只写项目/成员与角色或目标名称与探测结果，不包含构建命令或凭据（重试写 `retry_of=<原构建号>`）。失败登录受 Redis 限流约束，只进结构化日志不进审计表；审计保留策略待业务审计里程碑（P4）设计。审计查询仅管理员。
- 过期会话及其 refresh 令牌历史由后台任务每小时清理（启动时先执行一次）。重放检测只需保留到会话绝对期限，到期即删；清理不影响活跃会话。

## 角色矩阵

下面是首版权限约定。除已提供的鉴权、账号管理、审计、项目、构建与部署目标接口，其他服务尚未实现；页面守卫用于交互，不能代替后端对象级授权。角色是**全局的**；项目可见性（public/private）与项目成员（maintainer/viewer）已实现 P1 与 P2（项目级权限设计文档本地维护，不入库）：成员身份既决定「能看哪个项目」，也决定「能改哪个项目」。

**可见性规则**：全局 admin 永远可见；其他角色可见 public 项目，以及自己创建或被加入的 private 项目。不可见一律返回 **404**（不是 403），避免用私有 id 枚举项目。构建、日志与 SSE 继承所属项目的可见性。

**写权限**：全局角色是上限——viewer 永不可写、admin 永远可写；private 项目另外要求调用者是该项目 maintainer，否则 **403**（可见但无权）。public 项目保持「任意 member 可写」。项目修改、归档、触发/取消/重试构建、成员增删改都走同一条判定（`projects.AuthorizeWrite`）。成员变更写审计：`project.member_add` / `project.member_update` / `project.member_remove`（objectType project）。

| 操作 | admin | member | viewer | 实现状态 |
| --- | --- | --- | --- | --- |
| 读取自己的资料、退出、修改自己密码、查看/撤销自己的会话 | 允许 | 允许 | 允许 | 已实现 |
| 创建/查询用户、重置密码、禁用/启用、变更角色、查询审计 | 允许 | 禁止 | 禁止 | 已实现 |
| 读取项目（public，或自己创建/已被加入的 private） | 允许 | 允许 | 允许 | 已实现（可见性过滤） |
| 读取其他 private 项目 | 允许 | 禁止（404） | 禁止（404） | 已实现（404 防枚举） |
| 创建/修改项目、归档与恢复、修改可见性 | 允许 | public 允许；private 需项目 maintainer | 禁止 | 已实现（可见性 + maintainer 叠加） |
| 读取项目成员、增删改成员 | 允许 | public 允许；private 需项目 maintainer | 只读列表 | 已实现；创建者不可移除；成员变更写审计 |
| 读取构建/部署/审查/扫描/知识信息 | 允许 | 允许 | 允许 | 构建与日志已实现（含可见性过滤）；其余待业务接口落实 |
| 发起/取消/重试构建、发起审查 | 允许 | public 允许；private 需项目 maintainer | 禁止 | 构建已实现（重试仅限终态，不可见 404）；发起审查待业务接口落实 |
| 发起部署、回滚及执行审批 | 允许 | 禁止 | 禁止 | 部署触发与回滚已实现（admin-only，P4.2）；审批流待 P4.3 |
| 管理部署目标、密钥、策略、设置、防御及业务审计 | 允许 | 禁止 | 禁止 | 部署目标已实现（admin-only，凭据加密存储，P4.1）；独立密钥管理、策略、设置、防御与业务审计待实现 |

后续对象涉及具体项目时，需在服务端查询对象所属项目并校验访问权限。不要根据客户端提交的 role 或隐藏按钮决定授权。

## 开发模式和验收

前端不再提供业务演示数据或演示开关：项目、构建、账号与审计始终调用真实接口，未接入模块恒定显示未实现声明，不用演示数据冒充业务结果。登录与鉴权没有 mock 回退。

后端验证：go test ./...、go vet ./...。前端验证：npm run build、npm run lint、npm test（session store 单元测试）。浏览器验证：先 npx playwright install chromium，再 npm run test:e2e；先设置 AEGIS_TEST_DATABASE_URL/AEGIS_TEST_REDIS_URL 运行后端真实集成测试；浏览器测试需另设置 AEGIS_E2E_DATABASE_URL/AEGIS_E2E_REDIS_URL 指向专用可丢弃服务，测试启动本地前后端，要求 8080、5173 端口空闲。E2E 后端显式设置 AEGIS_LOGIN_RATE_LIMIT=100，因为同一地址在一分钟内驱动多个账号登录；生产默认配额仍是 10。测试账号仅存在测试临时库；端到端配置使用随机 JWT 密钥，不使用生产数据。

验收覆盖：错误密码、正确登录、Cookie 属性、页面恢复、并发刷新、多个标签页、退出撤销、非管理员 403、刷新 Cookie 失效及管理员页面守卫；账号管理覆盖 UI 创建用户、角色变更、重置密码、禁用后 403 登录拒绝、审计查询、自助修改密码保留当前会话、双会话撤销其他全部并即时踢出；构建闭环覆盖 UI 创建带构建命令的项目、触发构建、列表与详情、SSE 推送日志中出现进程输出与 build succeeded、viewer 只读且触发 403；项目可见性与成员覆盖 UI 创建私有项目与徽章、成员页邀请查看者、非成员深链得到「未找到」、查看者成员无管理控件且触发构建 403、提升维护者后可触发。剩余能力（受控执行环境、仓库 clone、多 worker 并行吞吐）、生产备份及业务审计见 PR 记录与本地维护的开发文档（不入库）。
