# 首版部署、权限与浏览器鉴权约定

## 范围和部署决定

产品目标已明确为多人使用、多实例部署。采用 PostgreSQL 共享用户与会话、Redis 共享登录限流，多个 Go/Gin 应用副本通过负载均衡提供同源接口，无需粘性会话。SQLite 不再作为运行存储，旧开发数据不自动导入。业务数据访问按模块逐步抽出 Repository，Gin Context 不进入业务服务。构建 worker 为进程内单并发；副本之间用 PostgreSQL 会话级 advisory lock 竞争同一 worker 租约，只有持锁副本恢复中断构建并领取任务，其余副本待命并在锁释放后接管；领取继续使用 SKIP LOCKED 兜底。该租约保证同一时刻只有一个 worker，不提供并行吞吐。多租户处于阶段 F3：租户表、认证数据租户归属、默认租户保证（F1）、**全量业务数据租户隔离 + 租户运维 CLI**（F2）与**登录页工作空间标识、用户响应租户元数据、顶栏租户展示与双租户 E2E**（F3）已交付；子域/路径路由、租户切换、每租户品牌、平台控制台、租户配额与计费仍未开始。项目可见性与成员（服务端读过滤、写权限叠加与前端成员管理）已交付。

数据库迁移与首次管理员创建使用 advisory 事务锁；刷新与退出锁定共享 session 行，跨实例并发消费旧令牌会触发一致撤销。所有实例须使用一致 JWT 密钥、会话配置与 Redis namespace。登录限流失败返回503，无进程内fallback。入口代理CIDR由 AEGIS_TRUSTED_PROXIES 明确指定，默认不信任转发头。部署样例和运维边界见 [部署说明](../deploy/README.md)。

推荐通过一个 HTTPS 域名提供前端，并把 /api 代理到 Go 服务。开发使用 http://localhost:5173，Vite 已把 /api 代理到 127.0.0.1:8080。AEGIS_ALLOWED_ORIGINS 必须包含浏览器实际 Origin（协议、主机、端口完全匹配）。生产设置 AEGIS_COOKIE_SECURE=true。不要混用 localhost 和 127.0.0.1。

运行指标：设置 `AEGIS_METRICS_ADDR`（默认关闭）时，服务在**独立监听地址**提供 `GET /metrics`，输出 Prometheus 文本格式（`text/plain; version=0.0.4`），含 HTTP 请求计数/延迟直方图与构建状态 gauge，标签使用路由模板而不是原始路径。该端点没有用户鉴权（scraper 无法持会话），只应绑定回环地址或仅经受保护的代理暴露，且不要与 API 端口共用。

## 真实接口契约

机器可读契约见 [OpenAPI](../backend/openapi.yaml)，启动参数见 [后端文档](../backend/README.md)。响应使用统一 data/error/requestId 包装；时间字段命名含义和单位必须在各业务 schema 明确。

| 接口 | 请求 | 响应及效果 |
| --- | --- | --- |
| POST /api/v1/auth/login | JSON username/password/remember + 可选 tenantSlug | data.user（含 tenant{id,slug,name}）/accessToken/expiresIn；设置 refresh Cookie；禁用账号密码正确时 403 account_disabled；未带 tenantSlug 且用户名跨租户重复时 400 tenant_required |
| POST /api/v1/auth/refresh | JSON {}，携带 Cookie | 轮换 Cookie，返回新 accessToken 和服务端用户（含 tenant 元数据） |
| POST /api/v1/auth/logout | JSON {}，携带 Cookie | 撤销整个会话并删除 Cookie；幂等 |
| GET /api/v1/auth/me | Bearer accessToken | 读取数据库会话与用户当前角色；user 含 tenant{id,slug,name} |
| POST /api/v1/auth/password | Bearer，JSON currentPassword/newPassword | 修改自己密码；撤销除当前会话外的全部会话；当前密码错误返回 401 并记入审计 |
| GET /api/v1/auth/sessions | Bearer | 自己的活跃会话列表，标记当前会话；不返回任何令牌 |
| DELETE /api/v1/auth/sessions | Bearer | 撤销除当前会话外的全部会话，返回撤销数量 |
| DELETE /api/v1/auth/sessions/{id} | Bearer | 撤销自己的指定会话；未知或他人会话 404，重复撤销幂等成功 |
| GET /api/v1/users | 管理员 Bearer，limit/offset | 用户分页 {items,total,limit,offset}，不返回密码哈希 |
| POST /api/v1/users | 管理员 Bearer，JSON 用户字段 | 创建用户，返回 201 |
| PATCH /api/v1/users/{id} | 管理员 Bearer，JSON role/disabled 至少一项 | 变更角色或禁用状态；禁用立即撤销该用户全部会话 |
| POST /api/v1/users/{id}/password-reset | 管理员 Bearer，JSON password | 重置密码并撤销该用户全部会话 |
| GET /api/v1/audit | 管理员 Bearer，limit/offset | 审计分页查询，最新在前 |
| GET /api/v1/audit/export.csv | 管理员 Bearer，与审计列表相同的 action/objectType/result/actor/from/to 筛选 | `text/csv; charset=utf-8`，RFC 4180 转义与 CRLF；表头固定 `id,createdAt,action,objectType,objectId,result,actor,detail`（createdAt 为 RFC3339 UTC）；最多 10000 行，超出时响应头 `X-Audit-Truncated: true`；无匹配仍导出表头；不含凭据/令牌/请求正文；非 admin 403 |
| GET /api/v1/projects | Bearer（所有角色），limit/offset/status? | 项目分页，按名称排序；只返回调用者可见的项目 |
| POST /api/v1/projects | admin/member Bearer，JSON 项目字段（含可选 buildCommand、visibility） | 创建项目（active 状态，默认 public），201；创建者为 maintainer |
| GET /api/v1/projects/{id} | Bearer（可见者） | 项目详情；private 且不可见时 404 |
| PATCH /api/v1/projects/{id} | admin/member Bearer，JSON 至少一字段（含 buildCommand、visibility） | 修改字段、归档/恢复或改可见性；归档项目只接受恢复；不可见 404 |
| GET /api/v1/projects/{id}/members | Bearer（可见者） | 成员列表（userId/username/role/createdAt），maintainer 在前 |
| POST /api/v1/projects/{id}/members | admin/member Bearer，JSON `{userId,role}` | 201；重复成员 409，账号不存在 404，不可见 404 |
| PATCH /api/v1/projects/{id}/members/{userId} | admin/member Bearer，JSON `{role}` | 改成员角色；无该成员 404 |
| DELETE /api/v1/projects/{id}/members/{userId} | admin/member Bearer | 移除成员；创建者不可移除（400） |
| GET /api/v1/projects/{id}/policy | Bearer（可见者） | ProjectPolicy；无策略行时返回默认值（requireReview=false、requiredScanVerdict=none、updatedBy 空、updatedAt=0）；不可见 404 |
| PUT /api/v1/projects/{id}/policy | 全局 admin 或项目 maintainer Bearer，严格 JSON `{requireReview,requiredScanVerdict}`（两者必填；requiredScanVerdict ∈ none\|passed） | 200 返回更新后策略（含 updatedBy/updatedAt）；不可见 404、可见但无权 403；upsert 与 `policy.update` 审计同事务，detail 只写 projectId 与两个门禁值 |
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
| GET /api/v1/projects/{id}/scans | Bearer（可见者），verdict/limit/offset | 扫描分页，最新在前；不可见项目 404 |
| POST /api/v1/projects/{id}/scans | **HMAC 签名（无用户会话）**：`X-AegisFlow-Timestamp`（±300s）+ `X-AegisFlow-Signature`（HMAC-SHA256）+ `X-AegisFlow-Delivery`（8–128 `[A-Za-z0-9._-]`），JSON `{source,tool?,version?,verdict,summary?,reportUrl?}` | 201；重复 `deliveryId` 幂等 200 返回既有记录；签名/时间戳/头缺失 401；未知项目 404；未配置 `AEGIS_INGEST_WEBHOOK_SECRET` 503 |
| GET /api/v1/scans | Bearer（所有角色），limit/offset/projectId?/verdict? | 全局扫描分页，最新在前；仅含可见项目的扫描，不可见 `projectId` 过滤返回空列表（不可枚举） |
| GET /api/v1/scans/{id} | Bearer（可见者） | 扫描详情（含 projectName）；不可见/不存在 404 |
| POST /api/v1/security-events | **HMAC 签名（无用户会话）**：同上三个头，JSON `{source,severity,eventType,summary,occurredAt?,detailsUrl?,tenantSlug?}` | 201；重复 `deliveryId` 幂等 200 返回既有记录（按租户幂等）；`occurredAt` 不得晚于 now+300s；`tenantSlug` 可选（签名覆盖，缺省为默认租户；未知/非法 404/400）；未配置 `AEGIS_INGEST_WEBHOOK_SECRET` 503 |
| GET /api/v1/security-events | 管理员 Bearer，severity/status/source/limit/offset | 本租户防御事件分页，按 occurredAt 倒序；非 admin 403；跨租户事件 404 |
| GET /api/v1/security-events/{id} | 管理员 Bearer | 本租户事件详情；不存在/跨租户 404；非 admin 403 |
| POST /api/v1/security-events/{id}/ack | 管理员 Bearer，JSON {} | 200；open→acknowledged（记录确认人与时间），重复确认幂等不重复审计；不存在/跨租户 404；非 admin 403 |
| GET /api/v1/knowledge | Bearer（所有已登录角色），q/limit/offset | 知识库分页 `{items,total,limit,offset}`，按 updatedAt 倒序；q 为标题子串（ILIKE，`%`/`_` 按字面转义），limit 1–100 默认 20；列表项不含正文 |
| POST /api/v1/knowledge | 管理员 Bearer，JSON `{title,body,tags?}` | 201；title 1–200B 单行、body ≤100KB（原样存储）、tags ≤200B 单行逗号分隔（清洗空白）；同事务写 `knowledge.create`（detail 只含 id/title） |
| GET /api/v1/knowledge/{id} | Bearer（所有已登录角色） | 文档详情（含 body，原样返回）；不存在 404 |
| PUT /api/v1/knowledge/{id} | 管理员 Bearer，JSON `{title,body,tags?}` | 200；字段规则同上；不存在 404；同事务写 `knowledge.update`（detail 只含 id/title） |
| DELETE /api/v1/knowledge/{id} | 管理员 Bearer | 200 `{deleted:true}`；不存在 404；同事务写 `knowledge.delete` |
| GET /api/v1/secrets | 管理员 Bearer，limit/offset | 密钥元数据分页 `{items,total,limit,offset}`，按 createdAt 倒序；**任何响应都不含值或密文**，非 admin 403 |
| POST /api/v1/secrets | 管理员 Bearer，JSON `{name,value}` | 201 返回元数据（值以 AES-256-GCM 加密后入库，永不返回）；name 1–128B 单行唯一（冲突 409），value 1–4096B；未配置 `AEGIS_SECRET_KEY` 时 503 `secrets are not configured`；同事务写 `secret.create`（detail 只含 id/name） |
| PUT /api/v1/secrets/{id} | 管理员 Bearer，JSON `{value}` | 200 返回元数据并递增 `key_version`；不存在 404；未配置密钥 503；同事务写 `secret.update`（detail 只含 id/name，绝不含值） |
| DELETE /api/v1/secrets/{id} | 管理员 Bearer | 200 `{deleted:true}`；不存在 404；同事务写 `secret.delete` |

accessToken 仅存页面内存，不放 localStorage/sessionStorage。refresh token 不返回 JSON，由 HttpOnly、SameSite=Strict、Path=/api/v1/auth 的 Cookie 保存，无 Domain。勾选保持登录时 Cookie 持久化到绝对会话期限，未勾选使用 session Cookie；浏览器恢复会话可能保留 session Cookie，因此不承诺关闭窗口立即退出。主动退出以服务端撤销为准。默认绝对期限 30 天，由后端配置决定，刷新不延长。

所有鉴权 POST 需要有效白名单 Origin，并拒绝 Sec-Fetch-Site: cross-site（唯一例外：OIDC provider 回跳的 `GET /api/v1/auth/oidc/callback` 是顶级跨站导航，允许 `Sec-Fetch-Site: cross-site`；该路径以一次性 state Cookie + 原子消费的 state 行作为 CSRF 防护，且在两者校验前不产生任何变更）。CORS 凭据只对精确白名单来源开放。即使调用者不是浏览器，也必须显式传 Origin；不提供 JSON refresh token 降级通道。跨站部署不在首版支持范围。

### 租户模型（阶段 F1–F3）

- 定位与边界：阶段 F1 交付租户基础——`tenants` 表（`id`/`slug` 唯一/`name`/`status active|suspended`/时间戳）、认证数据的租户归属（`auth_users`/`auth_sessions`/`auth_audit` 的 `tenant_id` NOT NULL 外键，`oidc_login_states.tenant_id` 可空外键）与默认租户保证；阶段 F2 交付**全量业务数据租户隔离**（schema v19：14 张业务表 `tenant_id` NOT NULL 外键）与**租户运维 CLI** `cmd/tenant`（唯一租户创建入口，无 HTTP API）。业务隔离与开通入口同一批次上线：能创建第二租户的那一刻，所有查询已按租户过滤。
- 默认租户：schema v18 seed 固定 id `tenant_default_0000000000000001`、slug `default`、name `Default`、status `active`。启动时按 `AEGIS_DEFAULT_TENANT_SLUG`（默认 `default`，1–32 个小写 ASCII 字母/数字/中间短横线，不可首尾短横线）保证该租户存在：若配置 slug 不同且当前只有 seed 租户，原子重命名该行（id/name 不变，已有行归属不变）；若仍不存在则启动失败并给出明确错误。bootstrap 管理员创建在默认租户。
- 用户单租户归属：每个 `auth_users` 行属于恰好一个租户；用户名与邮箱只在租户内唯一（`UNIQUE(tenant_id, lower(username))`、`UNIQUE(tenant_id, lower(email)) WHERE email <> ''`），同一用户名/邮箱可存在于不同租户。`auth.User.TenantID` 仅服务端使用；自阶段 F3 起所有用户 JSON 统一带公开的 `tenant:{id,slug,name}`（由会话/查询结果中的服务端租户派生，绝不读取请求体）。
- 登录解析：登录 body 可选 `tenantSlug`（严格 JSON：null、未知字段、非字符串拒绝；格式非法 400 validation_failed）。**有 slug**：按（租户, 用户名）查询；租户不存在、用户不存在、密码错误、租户 suspended 统一 401 unauthorized，不泄露差异。**无 slug**：跨租户按用户名查找，0 命中 401；唯一命中登录；多命中返回 **400 `tenant_required`**（消息 `tenantSlug is required: this email exists in multiple tenants`），客户端带 slug 重试。单租户部署永远唯一命中，行为与 F1 前一致。
- JWT 与会话：access token 增加 `tid`（租户 id）声明；解析必须同时满足签名/issuer/audience/有效期、`tid` 非空、会话行 `tenant_id`、用户行 `tenant_id` 与 `tid` 一致、租户 `active`、会话未撤销未过期、用户未禁用，否则视为未认证（伪造/缺失/篡改 `tid` 均为 401）。会话行保存 `tenant_id`。
- Refresh：每次刷新重校验用户存在、角色/禁用状态（与既有逻辑一致）与租户 active；会话租户与用户租户不一致或租户 suspended 时返回 401 并**撤销该会话**。租户恢复 active 后旧会话不会复活。业务数据隔离不改变该行为：suspended 租户登录、刷新与既有 access token 均被拒绝。
- 租户内 RBAC：admin/member/viewer 是租户内角色；所有管理员用户管理接口（列表/创建/重置密码/禁用启用/角色）只作用于 actor 的租户，跨租户用户一律 404；最后一个可用管理员保护按租户计算。业务接口的 admin 同样只在**本租户**内拥有全量权限：跨租户资源一律 404，管理员身份不放宽租户边界。
- 审计隔离：`auth_audit.tenant_id` NOT NULL。所有审计写入（业务动作、登录/用户动作、worker 系统动作）都带租户：审批过期的系统审计使用**所属审批/部署的租户**（不再回落到默认租户），bootstrap/管理员创建使用目标用户租户，租户开通等运维操作暂不入业务审计。审计列表与 CSV 导出按会话租户过滤，管理员看不到其他租户的审计。密码登录成功审计 detail 追加 `tenant=<slug>`；OIDC 登录 detail 仍为 `method=oidc`。
- 业务数据隔离（F2）：`projects`、`builds`、`deployment_targets`、`deployments`、`approvals`、`approval_notifications`、`reviews`、`review_external_results`、`review_notifications`、`scans`、`security_events`、`knowledge_docs`、`secrets`、`project_policies` 均带 `tenant_id` NOT NULL 外键。**租户谓词**：所有列表、详情、更新、删除、编号分配、唯一冲突判断、搜索、分页/筛选、审计查询与 CSV、通知 outbox 都按 `actor.TenantID` 或父资源租户过滤；项目可见性谓词同时校验租户与 public/private 可见性，其它模块复用该谓词或按已校验父资源取数。跨租户一律 **404**（不泄露存在性，管理员接口也一样）；不可见项目的 `projectId` 过滤在全局列表端点返回空页（不可枚举），项目作用域端点（如 `GET /projects/{id}/deployments`）对不可见/跨租户项目返回 404。
- 唯一约束租户化（F2）：项目名 `UNIQUE(tenant_id, lower(name))`、部署目标名 `UNIQUE(tenant_id, lower(name))`、`secrets.name` `UNIQUE(tenant_id, name)`、`review_external_results`/`scans`/`security_events` 的 `delivery_id` `UNIQUE(tenant_id, delivery_id)`；项目内编号（build/deployment/review number）不变（项目已属单一租户）。因此两个租户可各自拥有同名项目/目标/密钥，也可各自使用同一 `deliveryId`；同一租户内重复分别返回 409 或按幂等语义处理。
- 入站证据归属（F2）：无会话的 HMAC 入站按资源反查租户——扫描用 URL 中的项目 id、审查外部结果用 URL 中的审查 id（未知或跨租户一律 404）；防御事件不是项目对象，签名 JSON body 可选 `tenantSlug`（HMAC 覆盖原始 body，因此租户选择已认证；缺省为部署默认租户，保持单租户集成兼容），未知/非法 slug 返回 404/400，不写任何行。`deliveryId` 幂等键按租户独立。
- Worker 与 outbox（F2）：构建 worker、部署 worker、审批过期扫描与通知 notifier 领取的行都携带 `tenant_id`；系统审计（如审批过期）用所属租户，部署取数/目标读取/项目名按行租户过滤，审批与审查 Webhook payload 增加 `tenant:{id,slug}`（仅租户 id 与公开 slug，不含敏感信息）。单部署的 worker 会处理所有租户的任务（这是部署级系统进程），但每行的归属与后续读写严格按行租户；`build_log_events`/`deployment_events`/`project_members` 无独立租户列，读取总是先按租户校验父项目/构建/部署，写入只针对已领取/已授权的父行。指标只输出无租户标签的部署级构建状态计数，不含租户明细；日志不含其他租户数据。
- 租户运维 CLI（F2，`cmd/tenant`）：唯一租户开通入口，复用 `AEGIS_DATABASE_URL`（与 server 相同；不需要 Redis/JWT 配置；数据库须先由 server 迁移，schema 版本不符时拒绝执行）。命令：`create --slug <slug> --name <name> [--admin-email <email>] [--admin-password <password>]`（创建租户；给了 email 则创建该租户 admin；未给密码时生成 24 字节随机 base64url 口令，**仅在 stdout 打印一次**并明确提示，不写日志/审计明文；slug 冲突/非法非 0 退出）、`create-admin --tenant <slug> --email <email> [--password <password>]`（用户名由 email local part 派生，冲突自动加 `-2`/`-3` 后缀）、`list`（id/slug/name/status/用户数/创建时间，无凭据）、`disable/enable --tenant <slug>`（status suspended/active）。CLI 与 HTTP 用户接口共用同一套校验、bcrypt 哈希、租户内唯一约束与审计写入（审计 actor 记为 `tenant-cli` 系统标签，只写用户名）；`--password` 会出现在进程列表/历史中，应优先使用生成口令或由密钥注入工具传入。运维 CLI 自身不入业务审计。
- OIDC 过渡：`oidc_login_states.tenant_id` 记录发起租户（本批固定默认租户）；回调在该租户内按已验证 email 关联或自动开通账号；同邮箱跨租户的 SSO 歧义与租户选择留待后续批次。
- 其他契约不变：登录/刷新/退出 Cookie 属性、Origin/CSRF、限流、错误信封、`/users` 响应字段保持；`tenant_required` 是登录在跨租户同名时返回的 400 错误码。

### 租户前端与双租户验收（阶段 F3）

- 用户响应租户元数据：`POST /auth/login`、`POST /auth/refresh` 的 `data.user`、`GET /auth/me`、`GET /users` 列表项与 `POST /users` 返回体都带同一对象 `tenant:{id,slug,name}`（id 为服务端租户 id，slug 为工作空间标识，name 为展示名）。它只由令牌/会话/查询结果中的租户派生：`GET /users` 列表只含调用者租户的用户，跨租户用户仍是 404，因此元数据不会泄露其他租户的存在性或名称。响应不新增令牌、Cookie 或权限语义，`auth.User.TenantID` 仍不出现在 JSON。
- 登录页工作空间标识（tenantSlug）：
  - 默认不显示工作空间标识输入，单租户部署的登录体验与 F3 前一致；用户可主动点击「填写工作空间标识」展开。
  - 登录返回 **400 `tenant_required`** 时，页面保留已填用户名/密码、自动展开「工作空间标识」输入并显示固定文案 **「该邮箱存在于多个工作空间，请输入工作空间标识」**（`errors.ts` 对 `tenant_required` 的中文映射，不渲染后端英文 message）；用户补填后重试，请求体带 `tenantSlug`（trim 后提交，空值则省略该字段）。
  - 支持 URL `?tenant=<slug>` 预填并展开输入（非法 slug 忽略）；slug 前端校验只允许 1–32 个小写字母、数字与中间短横线（不能以短横线开头/结尾），不合法时在本地给出中文提示、不发起请求；服务端 `400 validation_failed` 统一显示中文错误。
  - 登录成功后错误态清空并跳转 `returnTo`；退出登录清空内存用户对象（含租户元数据）。
- 界面展示：AppShell 顶栏用户信息附近显示当前工作空间名称，`title` 提示 slug；用户名菜单同时显示「工作空间：<name>（<slug>）」。窄屏只显示名称，不新增租户切换 UI（本阶段边界）。
- 双租户验收（Playwright 默认套件 + 真实 `cmd/tenant` CLI）：E2E 辅助 `tests/e2e/tenantCli.ts` 以 `cwd=backend` 用 Node `child_process.execFile` 执行 `go run ./cmd/tenant ...`（Windows/Linux 均不拼 shell 字符串），把 `AEGIS_E2E_DATABASE_URL` 映射为 CLI 所需的 `AEGIS_DATABASE_URL` 并继承 Go 缓存/代理环境，只从 stdout 捕获一次性初始口令（不写入日志）。用例：用唯一 slug 创建租户 B 与管理员并 `tenant list` 校验；经登录页 `?tenant=<slug>` 登录 B、顶栏显示 B 名称/slug、在 B 创建私有项目；退出后登录默认租户 A，断言项目列表不含 B 项目、深链 `/projects/<B项目id>/edit` 显示「未找到该项目」、`GET /api/v1/projects/<B项目id>` 返回 404、审计 API 按 B 管理员筛选为 0 且审计页面与列表不含 B 数据；另在 A 与 B 各建一个同用户名账号，验证不带 tenantSlug 的登录得到 `tenant_required` 提示、保留输入、非法 slug 本地拦截、补填 B slug 后登录成功且顶栏为 B。既有 26 例单租户 E2E 保持全绿（本批新增 2 例，共 28 例）。
- 本阶段边界（未做）：子域/路径路由、租户切换、每租户品牌、平台控制台、跨租户共享项目、租户配额与计费、跨租户 OIDC 歧义、LDAP/SAML、真实生产库升级演练。

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

### 安全扫描与攻击防御事件（P5 剩余模块批次 A）

- 定位：**集成优先，不自研扫描或检测引擎**。外部扫描/检测工具通过签名入站接口回写结果，AegisFlow 负责登记、可见性、审计与展示；扫描结果只读，防御事件由管理员确认。
- 入站密钥：`AEGIS_INGEST_WEBHOOK_SECRET`（16–256 字节）同时保护 `POST /api/v1/projects/{id}/scans` 与 `POST /api/v1/security-events`；未配置时两者返回 503 `dependencies_unavailable`（消息 `ingest is not configured`），设置不合法启动失败。它与审查外部结果沿用的 `AEGIS_REVIEW_WEBHOOK_SECRET` 相互独立：能回写扫描结果不等于能写审查证据。
- 签名与防重放（与 E4 完全一致）：`X-AegisFlow-Timestamp`（Unix 秒，与服务器时间差 ≤300s）、`X-AegisFlow-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + rawBody)>`（对原始请求体字节签名）、`X-AegisFlow-Delivery`（8–128 字符 `[A-Za-z0-9._-]`，全局幂等键）。缺头、签名错误、时间戳超出窗口或格式非法统一 401；严格 JSON（≤16 KiB、未知/`null` 字段拒绝，`Content-Type: application/json`）。
- 扫描（迁移 v14 `scans`）：`source` 必填 1–64 字节单行；`tool`/`version` 可选、≤64 字节单行；`verdict ∈ passed|failed|warning`；`summary` ≤1000 字节（保留换行、去除其他控制字符）；`reportUrl` 可选、仅 http(s)、无 userinfo/fragment、≤2048 字节，只展示不抓取。`delivery_id` 全局唯一，重复回写返回 200 + 既有记录。读取：项目内列表与全局列表（均按项目可见性过滤，全局列表的不可见 `projectId` 过滤返回空列表，不可枚举）、详情；不可见/不存在一律 404；响应含 `projectName`，不含签名头或密钥。
- 防御事件（迁移 v14 `security_events`）：`source` 必填 1–64 字节单行；`severity ∈ low|medium|high|critical`；`eventType` 必填 1–64 字节单行；`summary` 必填 1–1000 字节；`occurredAt` 可选 Unix 秒（默认 now，不得晚于 now+300s，按 occurredAt 倒序）；`detailsUrl` 同 reportUrl 规则。事件为全局对象，**读取与确认仅全局 admin**（member/viewer 403）。确认：open→acknowledged，记录 `acknowledgedBy/acknowledgedAt`，与 `security_event.ack` 审计同事务（objectType `security_event`，detail 只写 id/severity/eventType，不含 summary 原文）；重复确认幂等 200 且不重复审计；不存在 404。
- 前端：`/scans` 全局列表（项目/结论筛选、中文徽章、分页）与 `/scans/:id` 详情（来源、工具/版本、保留换行的摘要、报告外链 `target=_blank rel="noopener noreferrer"`）；`/defense` 仅管理员可见（导航隐藏 + 路由守卫 + 服务端 403），支持严重度/状态筛选、分页与开放事件二次确认。E2E 用 Node `crypto` 计算 HMAC 回写扫描与事件，验证筛选、详情、确认与幂等，并覆盖非 admin 的 403/无入口。

### 知识库与独立密钥管理（P5 剩余模块批次 B）

- 知识库定位：团队文档以**纯文本 / Markdown 原文**存储，服务端只做校验与持久化，**不自研富文本编辑器、不做服务端渲染**；安全渲染是客户端责任（禁用原始 HTML、链接协议白名单）。读取对所有已登录角色开放，写入仅全局 admin。
- 知识库字段：`title` 必填 1–200 UTF-8 字节、单行（trim）；`body` ≤102400 字节（100 KiB）、合法 UTF-8、不含 NUL，原样保存（换行与缩进对 Markdown 有意义）；`tags` 可选 ≤200 字节单行逗号分隔，服务端清洗为规范形式（逐项 trim、内部空白折叠、丢弃空项、`a,b` 无空格连接）。列表 `q` 为标题 ILIKE 子串（`%`、`_`、`\` 按字面转义，`ESCAPE '\'`），按 `updated_at DESC, id DESC` 倒序并服务端分页（limit 1–100 默认 20，offset ≥0）；列表投影不含 body，详情返回 body。
- 知识库迁移与审计：schema v15 新增 `knowledge_docs`（`(updated_at DESC, id)` 索引；`created_by`/`updated_by` 为操作者快照，非外键）。`knowledge.create` / `knowledge.update` / `knowledge.delete` 与数据变更**同事务**写入 `auth_audit`（objectType `knowledge_doc`），detail 只写 `id` 与 `title`，**正文永不进入审计**；删除为硬删除，无审计历史回放。
- 独立密钥管理定位：与部署目标凭据共用 `AEGIS_SECRET_KEY`，但**值只写不读**——服务端没有返回明文或密文的读取路径，页面与 API 只暴露元数据。全部接口 admin-only，member/viewer 403。
- 密钥加密方案：AES-256-GCM，密文布局 `[12 字节随机 nonce][ciphertext+tag]`，AAD 为 `aegisflow:secrets:<id>`（密文不能跨行复用），每次写入使用新随机 nonce（相同值不产生相同密文）。`secrets` 表保存 `ciphertext BYTEA`、`key_version INTEGER`（每次轮换 +1，仅记录轮换代次；本批不做多密钥迁移逻辑，更换 `AEGIS_SECRET_KEY` 后旧密文不可解——因为值本就不读回，只影响历史行的可解析性）与创建人/时间；`name` 唯一（区分大小写）。
- 密钥字段与权限：`name` 1–128 UTF-8 字节单行（trim，重复 409）；`value` 1–4096 字节，仅出现在请求体中，不落日志/错误/审计。未配置 `AEGIS_SECRET_KEY`（base64 恰好 32 字节）时，创建与轮换返回 **503 `secrets are not configured`**（`error.code=secrets_not_configured`），列表与删除仍可用；配置非法时启动失败。
- 密钥审计：`secret.create` / `secret.update` / `secret.delete`（objectType `secret`）与写入/轮换/删除同事务提交，detail 只写 `id` 与 `name`，**绝不含值或密文**。
- 前端：`/knowledge` 列表（标题搜索、服务端分页、标签徽章、空态/加载/中文错误）与 `/knowledge/:id` 详情（安全 Markdown 渲染、标签、创建/更新时间），管理员可见新建/编辑/删除（复用 Dialog/ConfirmDialog，标题/正文/标签前端校验）；`/keys` 密钥管理仅管理员可见（导航隐藏 + 路由守卫 + 服务端 403），列表只显示名称/创建人/创建与轮换时间，新建为 name + 密码框值 + 二次确认，轮换值、删除均二次确认，保存后提示值不可再查看；未配置密钥时写入失败显示中文提示。Markdown 渲染使用 `marked` + `DOMPurify`：原始 HTML token 一律转义为文本（XSS payload 按文本显示）、链接仅允许绝对 http(s) 并强制 `target=_blank rel="noopener noreferrer"`、图片降级为 alt 文本，DOMPurify 按标签/属性白名单二次清洗。

### 项目自动化策略（P5 剩余模块批次 C）

- 定位：策略是**按项目的数据库配置**，叠加在环境变量门禁之上，只会加严不会放松；不新增任何环境变量。`requireReview=true` 时无论目标环境是否命中 `AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS`，创建与回滚部署都必须关联同项目 `approved` 审查；`requiredScanVerdict=passed` 时项目**最新一条扫描**（`received_at DESC, id DESC`，与扫描列表同排序）必须存在且 verdict 为 passed。两者默认关闭，无策略行等价于默认策略，升级后既有部署行为不变。
- 门禁顺序：审查门禁（环境变量集合 ∪ 项目 requireReview）→ 最新扫描门禁 → 既有审批状态机；失败统一 400 `validation_failed`（扫描门禁消息含 `scan gate` 提示），创建与回滚同样生效。命中项目策略时 `deployment.create` / `deployment.rollback` 审计 detail 追加 ` policy=<projectId>`。
- 读写权限：读取对所有项目可见者开放；写入复用审查决策权限（`projects.AuthorizeDecide`：全局 admin 或该项目 maintainer；public 项目普通 member 不能写），可见但无权 403、不可见 404（先做可见性再判权限，不可用作枚举）。严格 JSON 拒绝未知/null/缺字段，枚举非法 400 `validation_failed`。
- 审计与迁移：upsert 与 `policy.update` 同事务（objectType `policy`，detail 只写 `projectId`、`requireReview`、`requiredScanVerdict`，不含其他内容）；schema v16 新增 `project_policies`（`project_id` 主键、外键级联删除，默认 false/`none`）。实现 `internal/policies` 不依赖 Gin，部署服务通过接口注入并在自己的事务内读取门禁输入。
- 前端：项目列表「策略」入口打开 `/projects/:id/policy`（admin/maintainer 可编辑，其他角色只读并说明），部署表单选择项目后加载策略并显示中文提示；全局 `/policies` 入口仍为未实现占位。

### 审计 CSV 导出（P5 剩余模块批次 C）

- 接口：`GET /api/v1/audit/export.csv`（admin-only；非 admin 403、未登录 401）与审计列表共用筛选语义：`action`/`objectType`/`result`/`actor`/`from`/`to` 按 AND 组合，`from`/`to` 接受 RFC3339 或 `YYYY-MM-DD`（只给日期时 `to` 覆盖该 UTC 全天），非法值 400。
- 响应：`text/csv; charset=utf-8`，`Content-Disposition: attachment; filename="audit-YYYYMMDD-HHMMSS.csv"`；按 RFC 4180 转义（含逗号/引号/换行的字段加引号、内部引号翻倍），记录以 CRLF 结束；表头固定 `id,createdAt,action,objectType,objectId,result,actor,detail`，createdAt 为 RFC3339 UTC，actor 优先用户名、为空时回退用户 id。最多 10000 行，命中更多时响应头 `X-Audit-Truncated: true`；没有匹配行也返回表头行。
- 边界：不导出 `requestId` 列、请求正文、请求头、凭据、令牌或 Cookie；detail 沿用审计表中既有的脱敏内容。导出为一次性有界查询（未做逐行流式），大结果集通过缩小筛选范围获得完整报告。
- 前端：设置页审计面板工具栏「导出 CSV」带上当前筛选，通过带 Bearer 的 fetch 下载 Blob（文件名含日期时间），导出中禁用按钮、失败中文提示、截断时提示缩小筛选范围。

### OIDC 单点登录（P5 剩余模块批次 D）

- 定位：标准 OIDC **授权码 + PKCE（S256）** 单点登录，接入外部身份提供方（IdP）；未配置 `AEGIS_OIDC_ISSUER` 时功能完全关闭，登录页与密码登录行为不变，`/auth/oidc/config` 返回 `enabled=false` 且不泄露 issuer，`start`/`callback` 返回 503（不是 404：便于运维区分「未配置」与「路径错误」）。
- 接口：`GET /api/v1/auth/oidc/config`（`{enabled,label}`，公开探针）、`GET /api/v1/auth/oidc/start?redirectTo=`、`GET /api/v1/auth/oidc/callback?code=&state=`。三条都是公开 GET 浏览器导航，不要求 Origin（GET 不适用 CSRF 头规则）；`start` 与 `callback` 与密码登录共用同一个 Redis 登录限流配额（fail-closed，Redis 故障 503，无进程内 fallback）。
- start：只接受站内相对路径（必须以 `/` 开头、非 `//`、无 scheme、无反斜杠/控制字符，编码后的 `//` 也拒绝），否则回退 `/`；生成 32 字节 state、32 字节 nonce 与 43 字节 PKCE verifier（S256 challenge），把 `SHA-256(state)`、nonce、verifier、redirectTo、10 分钟过期时间写入 `oidc_login_states`，同时设置一次性 `aegis_oidc_state` Cookie（HttpOnly、SameSite=Lax、Path=/api/v1/auth/oidc、Max-Age=600、Secure 由 `AEGIS_COOKIE_SECURE` 显式控制），302 到授权端点（`response_type=code`、`client_id`、`redirect_uri`、`scope`、`state`、`nonce`、`code_challenge`、`code_challenge_method=S256`）。
- callback（顶级跨站 GET 导航是 OIDC 的正常回跳：全局 `Sec-Fetch-Site: cross-site` 拒绝仅对这一条路径豁免，CSRF 由 state Cookie + 一次性 state 行承担）：先常量时间比较 query `state` 与 state Cookie；再用单条 `UPDATE ... WHERE consumed_at IS NULL AND expires_at > now RETURNING ...` 原子消费 state 行（不存在/过期/已用一律 `state_expired`，重放不能第二次建会话）；用 verifier 在 token 端点换 id_token（配置 client secret 时用 `client_secret_post`，未配置时是 public client PKCE；请求/响应有超时与 256 KiB 大小上限、拒绝跟随 3xx）；校验 RS256 签名（JWKS 按 kid 取钥，未知 kid 刷新一次缓存以支持轮换；RSA 2048–8192 位）、`iss` 精确匹配、`aud` 含 client_id、`exp`/`iat`/`nbf`（±60s 时钟容差，exp 必填）、nonce 常量时间匹配、`azp`（若存在）等于 client_id、`email_verified=true` 且 email 非空合法。失败统一 302 `/login?sso_error=<固定错误码>`，不泄露 IdP 错误原文、令牌或 claims。
- 配置（启动校验，失败拒绝启动）：`AEGIS_OIDC_ISSUER`（必填才启用；https，测试允许 loopback http；无 userinfo/query/fragment，尾部斜杠归一化）；`AEGIS_OIDC_CLIENT_ID`（启用时必填）；`AEGIS_OIDC_CLIENT_SECRET`（可选，配置即 `client_secret_post` 保密客户端）；`AEGIS_OIDC_REDIRECT_URL`（启用时必填，与 issuer 同一 URL 安全规则，必须是本服务 callback 的绝对地址）；`AEGIS_OIDC_SCOPES`（默认 `openid profile email`，必须含 `openid`）；`AEGIS_OIDC_AUTO_PROVISION`（默认 false）；`AEGIS_OIDC_LABEL`（登录按钮文案，默认 `企业单点登录`）；`AEGIS_OIDC_DISCOVERY_TIMEOUT`（默认 5s，上限 30s）。只设置 issuer 之外的任一 `AEGIS_OIDC_*` 会启动失败（fail closed）；启用时启动必须先成功拉取 `{issuer}/.well-known/openid-configuration` 并校验 `issuer` 与实际一致、三者端点均为 https（测试 loopback 允许 http）、无 userinfo/query/fragment，否则拒绝启动；discovery/JWKS/token 响应大小上限 256 KiB，SSRF 规则与 issuer 一致。
- 账号关联：按 id_token 中**已验证 email** 在登录 state 记录的租户内大小写不敏感查找本地账号（`auth_users.email`，schema v17 新增；空 email 不参与匹配，因此未显式设置 email 的存量账号不会被 IdP 同名邮箱自动接管）。已存在账号：禁用则拒绝（`account_disabled`，不透露密码/角色信息），否则复用密码登录同一条 session 签发与审计路径（同一 session/refresh 表、同样的 Cookie 属性、同样的事务内审计），会话为**非持久会话 Cookie**（SSO start 不带 remember 选择）。自动开通同样发生在该租户内；跨租户同邮箱的 SSO 歧义留待后续批次。
- 自动开通边界：`AEGIS_OIDC_AUTO_PROVISION=true` 且 email 无匹配账号时，创建**全局 member**（永不 admin、永不 viewer）；用户名取 `preferred_username`（回退 email local part，非法字符清洗，冲突加随机后缀，最长 64 字节）；密码是随机不可用值（bcrypt 哈希后丢弃明文），因此该账号在管理员重置密码前只能经 SSO 登录；displayName 取 `name`（清洗控制字符、≤256 字节）。并发首登同一 email 时，唯一 email 索引保证只创建一个账号，另一请求复用已创建账号。
- 错误码（`?sso_error=`，前端映射中文）：`state_expired`（state Cookie 缺失/不一致/过期/已用）、`token_exchange_failed`（code 缺失、token 端点失败/无 id_token）、`invalid_token`（签名/iss/aud/exp/iat/nbf/nonce/azp/JWKS kid 失败）、`email_unverified`（email 缺失或未验证）、`account_not_found`（未匹配且未开启自动开通）、`account_disabled`、`server_error`（本地存储/会话等内部错误）。响应与日志都不含 IdP 错误原文、令牌、Cookie 或 claims。
- 审计：成功登录沿用 `auth.login`（objectType `user`），detail 固定为 `method=oidc`；自动开通额外在**同一事务**写 `user.create`（detail 同样只含 `method=oidc`，不写 email）。callback 失败也写 `auth.login` 失败审计，detail 仅 `method=oidc code=<固定错误码>`；该路径在写审计前已通过共享登录限流，因此失败行数量受限，不构成无界写入面。密码登录失败仍只进结构化日志（不写审计表），两者不改变既有密码登录行为。
- 迁移与测试：schema v17 新增 `oidc_login_states`（`state_hash` 主键、`nonce`/`code_verifier`/`redirect_to`/`expires_at`/`consumed_at`/`created_at`，`expires_at` 索引，start 时顺带清理过期行）与 `auth_users.email`（`lower(email)` 部分唯一索引，`email <> ''`）。Go 测试使用 httptest 假 IdP（crypto/rsa 生成密钥、discovery/JWKS/authorize/token，token 端点校验 PKCE）覆盖 config 未启用/启用、state Cookie 缺失/不一致/重放/过期、nonce/aud/iss/exp/iat/nbf 校验与 ±60s 时钟容差、email 未验证、已存在账号登录成功（Cookie 属性 + 审计）、禁用账号拒绝、自动开通开关、admin 不被自动创建、开放重定向拒绝、JWKS kid 轮换与未知 kid、token 端点失败与失败审计脱敏；配置校验有独立单测。可选 E2E（`AEGIS_E2E_OIDC=1`）另起本地假 provider，覆盖浏览器内完整授权码 + PKCE 登录并自动开通 member。
- 本批边界（未做）：真实 IdP 联调与各厂商兼容矩阵；LDAP；多租户/按 IdP 分组；RP-Initiated Logout / 单点登出与本地会话联动；使用 IdP access token 或 userinfo（本实现只信任 id_token）；管理员维护 `email` 的界面（本批 email 仅由自动开通写入，存量账号需要后续批次或直接 SQL 关联）；`email_verified` 为字符串而非布尔 JSON 的 IdP 不支持（严格按布尔校验）。
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
- 角色为租户内角色，每次鉴权实时读取数据库，角色变更对既有会话即时生效，无需撤销会话。禁用或降级最后一个可用管理员被拒绝（400，按租户计算），防止系统失去管理入口。
- 持久化审计记录操作人（ID/用户名）、时间、动作、对象、结果、详情与请求 ID：登录成功、创建用户、修改密码（含当前密码错误的失败记录）、重置密码、禁用/启用、角色变更、会话撤销、项目成员增删改（project.member_add / project.member_update / project.member_remove）、构建触发（build.trigger）、取消（build.cancel）与重试（build.retry）、部署目标增删改与探测（deployment_target.create / deployment_target.update / deployment_target.remove / deployment_target.check）、部署创建/取消/回滚（deployment.create / deployment.cancel / deployment.rollback）、审批请求/批准/拒绝/过期（approval.request / approval.approve / approval.reject / approval.expire）、审查请求与批准/拒绝/取消（review.request / review.approve / review.reject / review.cancel，objectType `review`）、防御事件确认（security_event.ack，objectType `security_event`）、知识库文档新建/更新/删除（knowledge.create / knowledge.update / knowledge.delete，objectType `knowledge_doc`）、密钥新建/轮换/删除（secret.create / secret.update / secret.delete，objectType `secret`）、项目自动化策略更新（policy.update，objectType `policy`）、OIDC 登录（复用 `auth.login`，detail `method=oidc`；自动开通在同一事务追加 `user.create`）。构建、成员、部署目标、部署任务、审批、审查、防御事件确认、知识库文档、密钥操作与项目策略审计都和状态变更同事务提交；重复取消/重复审批/重复审查决策/重复确认防御事件幂等且不重复记账，detail 只写项目/成员与角色或目标名称与探测结果，不包含构建命令或凭据（重试写 `retry_of=<原构建号>`，拒绝写 `reason=<≤512>`，审查写 `number=<n>` 与 reason，不写审查正文；部署创建/回滚命中审查门禁时追加 `review=<id>`，只写审查 id；命中项目策略时追加 `policy=<projectId>`；防御事件确认只写 `id/severity/eventType`，不写 summary 原文；知识库 detail 只写 `id/title`，不写正文；密钥 detail 只写 `id/name`，绝不写值或密文；策略 detail 只写 `projectId` 与两个门禁值）。签名扫描回写是外部证据登记，不新增业务审计动作。密码登录失败受 Redis 限流约束，只进结构化日志不进审计表；OIDC callback 失败在共享限流之后写入 `auth.login` 失败审计，detail 只含 `method=oidc` 与固定错误码（不含 IdP 原文/令牌/claims）；审计查询仅管理员且只返回调用者租户的行；密码登录成功 detail 追加 `tenant=<slug>`，OIDC 登录仍只写 `method=oidc`。
- 审计查询支持服务端筛选：`action`、`objectType`、`result`（success/failure）、`actor`（id 或用户名，用户名大小写不敏感）、`from`/`to`（RFC3339 或 `YYYY-MM-DD`，只给日期时 `to` 覆盖该 UTC 全天）。筛选同时作用于分页结果与 total，多个条件按 AND 组合；非法时间或未知 result 返回 400。审计页提供动作/对象/结果/操作人/日期范围筛选与重置。查询始终限定在调用者租户内，租户本身不是可筛选参数。
- 审计 CSV 导出：`GET /api/v1/audit/export.csv`（admin-only）复用上述筛选语义，按 RFC 4180 导出最多 10000 行（`id,createdAt,action,objectType,objectId,result,actor,detail`，createdAt 为 RFC3339 UTC），超出时响应头 `X-Audit-Truncated: true`，无匹配仍返回表头；不导出 `requestId`、请求正文、凭据、令牌或 Cookie。审计页工具栏「导出 CSV」带上当前筛选并通过带 Bearer 的 fetch 下载 Blob（文件名含日期时间），导出中禁用按钮、失败中文提示、截断时提示缩小筛选范围；导出同样只包含调用者租户的审计行。
- 审计保留策略：`AEGIS_AUDIT_RETENTION` 可选，未设置或为 `0` 时**永久保留**（默认，审计不应默认丢证据）；设置为 24h–8760h 之间的时长时，后台任务每小时删除早于该窗口的审计行（启动时先执行一次），与过期会话清理共用同一个循环。删除是运维显式选择，而不是按业务对象做归档。
- 过期会话及其 refresh 令牌历史由后台任务每小时清理（启动时先执行一次）。重放检测只需保留到会话绝对期限，到期即删；清理不影响活跃会话。

## 角色矩阵

下面是首版权限约定。除已提供的鉴权（含 OIDC 单点登录）、账号管理、审计（含 CSV 导出）、项目、构建、部署目标、部署任务、审批、审查、安全扫描与攻击防御事件、知识库与独立密钥管理、项目自动化策略接口，其他服务尚未实现；页面守卫用于交互，不能代替后端对象级授权。角色是**租户内的**（阶段 F1 每个部署只有默认租户，因此与 F1 前的全局角色语义等价；租户内 admin/member/viewer 不变）；项目可见性（public/private）与项目成员（maintainer/viewer）已实现（P2；项目级权限设计文档本地维护，不入库）：成员身份既决定「能看哪个项目」，也决定「能改哪个项目」。

**可见性规则**：全局 admin 永远可见；其他角色可见 public 项目，以及自己创建或被加入的 private 项目。不可见一律返回 **404**（不是 403），避免用私有 id 枚举项目。构建、部署、日志与 SSE 继承所属项目的可见性。

**写权限**：全局角色是上限——viewer 永不可写、admin 永远可写；private 项目另外要求调用者是该项目 maintainer，否则 **403**（可见但无权）。public 项目保持「任意 member 可写」。项目修改、归档、触发/取消/重试构建、成员增删改都走同一条判定（`projects.AuthorizeWrite`）。成员变更写审计：`project.member_add` / `project.member_update` / `project.member_remove`（objectType project）。

| 操作 | admin | member | viewer | 实现状态 |
| --- | --- | --- | --- | --- |
| 读取自己的资料、退出、修改自己密码、查看/撤销自己的会话 | 允许 | 允许 | 允许 | 已实现 |
| 创建/查询用户、重置密码、禁用/启用、变更角色、查询与导出审计 | 允许 | 禁止 | 禁止 | 已实现（审计查询与 CSV 导出均 admin-only） |
| 读取项目（public，或自己创建/已被加入的 private） | 允许 | 允许 | 允许 | 已实现（可见性过滤） |
| 读取其他 private 项目 | 允许 | 禁止（404） | 禁止（404） | 已实现（404 防枚举） |
| 创建/修改项目、归档与恢复、修改可见性 | 允许 | public 允许；private 需项目 maintainer | 禁止 | 已实现（可见性 + maintainer 叠加） |
| 读取项目成员、增删改成员 | 允许 | public 允许；private 需项目 maintainer | 只读列表 | 已实现；创建者不可移除；成员变更写审计 |
| 读取构建/部署/审查/扫描/知识/项目策略信息 | 允许 | 允许 | 允许 | 构建、部署、审查（含外部结果读取）、安全扫描、知识库与项目自动化策略已实现（构建/部署/审查/扫描按项目可见性过滤；知识库对所有已登录角色开放；策略对项目可见者只读）；审批与防御事件仍 admin-only |
| 新建/编辑/删除知识库文档 | 允许 | 禁止 | 禁止 | 已实现（P5 批次 B，admin-only）：title/body/tags 校验，审计与写入同事务且 detail 不含正文 |
| 管理独立密钥（新建/轮换/删除/元数据） | 允许 | 禁止 | 禁止 | 已实现（P5 批次 B，admin-only）：值 AES-256-GCM 加密且只写不读，列表只返回元数据；未配置 `AEGIS_SECRET_KEY` 时写入 503 |
| 发起/取消/重试构建、发起审查 | 允许 | public 允许；private 需项目 maintainer | 禁止 | 构建与审查发起已实现（重试仅限终态，不可见 404；审查发起走同一写权限叠加） |
| 批准/拒绝审查、取消他人审查 | 允许 | 仅该项目的 maintainer；发起人可取消本人的 pending 审查 | 禁止 | 已实现（`projects.AuthorizeDecide`，比写权限更严；终态 409，重复决策/取消幂等 200） |
| 读取/修改项目自动化策略 | 允许 | 读取允许；修改仅该项目 maintainer（public 项目的普通 member 禁止） | 读取允许；修改禁止 | 已实现（P5 批次 C）：读取对项目可见者开放（无行返回默认值）；写入复用 `projects.AuthorizeDecide`，与 `policy.update` 审计同事务；详情见「项目自动化策略」 |
| 外部系统回写审查结果、接收审查通知 | —（HMAC，无用户会话） | — | — | 已实现（P5-A E4）：`AEGIS_REVIEW_WEBHOOK_SECRET` HMAC-SHA256 + ≤300s 时间窗 + delivery 幂等；只追加只读证据，不改审查状态；审查事件复用审批 Webhook 通道出站 |
| 读取/确认攻击防御事件 | 允许 | 禁止 | 禁止 | 已实现（P5 批次 A，admin-only）：列表/详情/确认；确认记录确认人与时间并写 `security_event.ack` 审计，重复确认幂等 |
| 外部系统回写扫描结果、防御事件 | —（HMAC，无用户会话） | — | — | 已实现（P5 批次 A）：`AEGIS_INGEST_WEBHOOK_SECRET` HMAC-SHA256 + ≤300s 时间窗 + delivery 幂等；扫描结果按可见性只读，防御事件仅管理员确认；平台不自研扫描/检测引擎 |
| 发起部署、回滚及执行审批 | 允许 | 允许（项目可写时）；审批仅 admin | 禁止 | 部署触发与回滚已实现（项目写权限叠加，P4.2）；P5-A E2 增加可配置审查门禁：目标环境命中 `AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS` 时要求已批准审查，默认关闭；P5 批次 C 再叠加项目策略：`requireReview=true` 时任何环境都要求已批准审查，`requiredScanVerdict=passed` 时最新扫描必须通过；审批仍 admin-only（P4.3） |
| 管理部署目标、密钥、设置及业务审计 | 允许 | 禁止 | 禁止 | 部署目标（P4.1，凭据加密存储）与独立密钥管理（P5 批次 B，只写不读）已实现，均为 admin-only；防御事件确认（P5 批次 A）与审计 CSV 导出（P5 批次 C）同样 admin-only；项目策略按项目由 admin/maintainer 写；设置页与其余 P5 业务模块待实现 |

| 通过 OIDC 单点登录（已开通账号） | 允许（按既有账号角色） | 允许（按既有账号角色） | 允许（按既有账号角色） | 已实现（P5 批次 D）：真实授权码 + PKCE（S256）、一次性 state、nonce/JWKS 验签、`email_verified` 必填；按已验证 email 大小写不敏感关联既有账号，禁用账号拒绝；会话 Cookie 与密码登录一致 |
| OIDC 自动开通新账号（`AEGIS_OIDC_AUTO_PROVISION=true`） | —（不自动创建 admin） | 仅 member，永不 admin/viewer | — | 已实现（P5 批次 D）：首次登录创建 member、随机不可用密码、用户名冲突加随机后缀；已有账号可被关联但角色不会被 SSO 修改 |
后续对象涉及具体项目时，需在服务端查询对象所属项目并校验访问权限。不要根据客户端提交的 role 或隐藏按钮决定授权。

## 开发模式和验收

前端不再提供业务演示数据或演示开关：项目、构建、部署、审查、安全扫描、防御事件、知识库、密钥管理、项目自动化策略、账号与审计、OIDC 单点登录始终调用真实接口，未接入模块恒定显示未实现声明，不用演示数据冒充业务结果。登录与鉴权没有 mock 回退。

后端验证：go test ./...、go vet ./...。前端验证：npm run build、npm run lint、npm test（session store 单元测试）。浏览器验证：先 npx playwright install chromium，再 npm run test:e2e；先设置 AEGIS_TEST_DATABASE_URL/AEGIS_TEST_REDIS_URL 运行后端真实集成测试；浏览器测试需另设置 AEGIS_E2E_DATABASE_URL/AEGIS_E2E_REDIS_URL 指向专用可丢弃服务，测试启动本地前后端，要求 8080、5173 端口空闲。E2E 后端显式设置 AEGIS_LOGIN_RATE_LIMIT=100，因为同一地址在一分钟内驱动多个账号登录；生产默认配额仍是 10。测试账号仅存在测试临时库；端到端配置使用随机 JWT 密钥，不使用生产数据。E2E 的 webServer 还会注入固定的一次性测试密钥 `AEGIS_REVIEW_WEBHOOK_SECRET`（外部结果用例）与 `AEGIS_INGEST_WEBHOOK_SECRET`（扫描/防御事件用例），仅供用例计算 HMAC 签名回写；同时每次运行随机生成 `AEGIS_SECRET_KEY`（base64 恰好 32 字节），供部署目标凭据与独立密钥管理的加密写入使用，随一次性测试库一起丢弃，不进入生产配置。项目自动化策略与审计 CSV 导出用例复用同一一次性库与签名密钥（策略是数据库配置，不新增环境变量）。设置 `AEGIS_E2E_OIDC=1` 时 E2E 额外启动本地假 OIDC provider（Node 生成一次性 RSA 密钥，提供 discovery/JWKS/authorize/token 并校验 PKCE S256），给后端注入一次性 `AEGIS_OIDC_*`（issuer/客户端/回调均为测试占位值、`AUTO_PROVISION=true`），`tests/e2e/oidc.spec.ts` 才被纳入运行；不设置时 provider 不启动、SSO 用例被配置排除，默认 E2E 仍是未配置 OIDC 的纯密码场景（登录页无 SSO 按钮）。阶段 F3 的双租户用例会用真实 `go run ./cmd/tenant`（cwd=backend）开通第二租户，数据库复用 `AEGIS_E2E_DATABASE_URL`（映射为 CLI 的 `AEGIS_DATABASE_URL`），因此跑 E2E 的机器需要 Go 工具链且一次性库已由后端 webServer 完成迁移；CLI 生成的一次性初始口令只从 stdout 捕获，不写日志、不进入页面或审计。

验收覆盖：错误密码、正确登录、Cookie 属性、页面恢复、并发刷新、多个标签页、退出撤销、非管理员 403、刷新 Cookie 失效及管理员页面守卫；账号管理覆盖 UI 创建用户、角色变更、重置密码、禁用后 403 登录拒绝、审计查询、自助修改密码保留当前会话、双会话撤销其他全部并即时踢出；构建闭环覆盖 UI 创建带构建命令的项目、触发构建、列表与详情、SSE 推送日志中出现进程输出与 build succeeded、viewer 只读且触发 403；项目可见性与成员覆盖 UI 创建私有项目与徽章、成员页邀请查看者、非成员深链得到「未找到」、查看者成员无管理控件且触发构建 403、提升维护者后可触发；部署与审批覆盖部署目标、发起部署、审批、取消与回滚主链路；审查中心覆盖 UI 发起审查（列表对话框）、批准（带理由）与状态/项目筛选、拒绝理由必填与详情展示、viewer 只读（无发起入口且发起 API 403）、非项目成员详情 404，以及部署表单关联已批准审查并在部署详情展示审查链接。审查详情页回显外部工具签名回写的只读结果（来源、中文「通过/失败/警告」徽章、保留换行的摘要、报告外链），E2E 覆盖签名回写后回显与重复 delivery 幂等。安全扫描与攻击防御事件（P5 批次 A）覆盖：外部工具签名回写扫描（passed/failed）后在 `/scans` 列表按项目/结论筛选命中、详情展示来源/工具/保留换行的摘要与报告外链、重复 delivery 幂等 200；签名回写高危防御事件后在 `/defense` 出现、admin 二次确认后状态变为已确认且重复确认幂等，非 admin 无导航入口、访问 `/defense` 被守卫拒绝且接口 403，而扫描读取仍按项目可见性开放。知识库与独立密钥管理（P5 批次 B）覆盖：管理员新建含 Markdown 粗体/行内代码与 `<script>` XSS payload 的文档后列表搜索命中、详情中粗体/代码生效且脚本按文本显示（不弹窗、无 script 元素）、编辑标题与正文后详情更新、删除后列表消失；密钥管理新建（值二次确认）后列表只显示名称/创建人/时间，页面与列表/创建/轮换/删除接口响应均不含该值，「最近轮换」随轮换更新，删除后行消失；非 admin 无密钥管理导航入口、访问 `/keys` 被守卫拒绝、`GET /api/v1/secrets` 403，而知识库列表/详情可读、无新建/编辑/删除按钮且写接口 403。项目自动化策略与审计导出（P5 批次 C）覆盖：UI 创建项目后在项目「策略」页开启 `requireReview` 并保存，部署表单出现中文策略提示，未关联审查创建部署得到 400 中文错误，发起并批准同项目审查后选择关联审查创建部署成功；策略改为 `requiredScanVerdict=passed` 后无扫描创建部署被拒，经既有签名回写通道登记 `passed` 扫描后创建成功；设置页审计面板按动作筛选后点击「导出 CSV」触发浏览器下载，文件名含日期时间、CSV 表头与筛选一致。OIDC 单点登录（P5 批次 D）覆盖：未配置时登录页无 SSO 按钮、无行为变化（默认 E2E 套件全部通过）；`AEGIS_E2E_OIDC=1` 时登录页出现「企业单点登录」按钮，点击经本地假 provider 完成真实授权码 + PKCE（S256 由 provider 校验）回调后进入站内 returnTo、refresh Cookie 可恢复会话、自动开通账号角色为 member 且调用管理员接口 403。多租户基础（阶段 F1）覆盖：迁移 v18 的默认租户 seed、回填 NOT NULL 与租户内唯一索引，默认 slug 重命名启动路径与缺失时拒绝启动，bootstrap 管理员落在默认租户；登录不带 tenantSlug 的单租户成功、带 slug 成功、错 slug/错密码统一 401、同用户名跨两租户时 400 tenant_required 与带 slug 重试成功；JWT `tid` 存在且缺失/篡改时被拒，refresh 在租户 suspended 或跨租户时撤销会话；管理员用户接口跨租户 404、审计列表与导出按租户过滤；OIDC state 记录默认租户并在该租户内关联/自动开通。阶段 F3 覆盖：登录/刷新/`/auth/me`/用户列表与创建响应都带 `tenant:{id,slug,name}` 且跨租户不串（auth/httpapi 测试）；前端 Vitest 覆盖 tenant_required 展开提示、提交带 tenantSlug、slug 格式、错误码中文映射与 session 保存/清除 tenant；Playwright 新增 2 例双租户验收（用真实 `go run ./cmd/tenant` CLI 创建唯一租户 B 与管理员：登录页 `?tenant=<slug>` 预填并登录 B、顶栏显示 B 名称与 slug title、B 内私有项目在 A 侧项目列表/深链/API/审计均不可见；A 与 B 同标识账号登录触发 `tenant_required`、保留邮箱密码、非法 slug 本地拦截、补填 B slug 后登录成功且错误态清空）。单租户（登录请求不带 tenantSlug）既有全部 E2E 用例保持通过。剩余能力（多 worker 并行吞吐、构建阶段与产物、构建缓存、私有仓库凭据、部署多级会签与通知管理 API、跨租户 OIDC 歧义与租户切换、子域/路径路由、每租户品牌、平台控制台、租户配额与计费、LDAP/SAML 与真实 IdP 联调、单点登出）与生产备份演练见 PR 记录与本地维护的开发文档（不入库）。
