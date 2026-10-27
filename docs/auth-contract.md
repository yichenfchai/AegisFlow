# 首版部署、权限与浏览器鉴权约定

## 范围和部署决定

产品目标已明确为多人使用、多实例部署。采用 PostgreSQL 共享用户与会话、Redis 共享登录限流，多个 Go/Gin 应用副本通过负载均衡提供同源接口，无需粘性会话。SQLite 不再作为运行存储，旧开发数据不自动导入。业务数据访问按模块逐步抽出 Repository，Gin Context 不进入业务服务。构建 worker 为进程内单并发；副本之间用 PostgreSQL 会话级 advisory lock 竞争同一 worker 租约，只有持锁副本恢复中断构建并领取任务，其余副本待命并在锁释放后接管；领取继续使用 SKIP LOCKED 兜底。该租约保证同一时刻只有一个 worker，不提供并行吞吐。多租户和项目成员权限尚未实现。

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
| GET /api/v1/projects | Bearer（所有角色），limit/offset/status? | 项目分页，按名称排序 |
| POST /api/v1/projects | admin/member Bearer，JSON 项目字段（含可选 buildCommand） | 创建项目（active 状态），201 |
| GET /api/v1/projects/{id} | Bearer（所有角色） | 项目详情 |
| PATCH /api/v1/projects/{id} | admin/member Bearer，JSON 至少一字段（含 buildCommand） | 修改字段或归档/恢复；归档项目只接受恢复 |
| POST /api/v1/projects/{id}/builds | admin/member Bearer，JSON branch? | 入队构建，201（queued）；项目须活跃且配置 buildCommand |
| GET /api/v1/builds | Bearer（所有角色），limit/offset/projectId? | 构建分页，最新在前 |
| GET /api/v1/builds/{id} | Bearer（所有角色） | 构建详情（状态、命令快照、时间戳） |
| GET /api/v1/builds/{id}/logs | Bearer（所有角色），after/limit | 日志事件 {items}，按事件 id 升序；after 为游标 |
| POST /api/v1/builds/{id}/cancel | admin/member Bearer，JSON {} | 取消排队/运行中的构建；重复取消幂等，终态 409 |

accessToken 仅存页面内存，不放 localStorage/sessionStorage。refresh token 不返回 JSON，由 HttpOnly、SameSite=Strict、Path=/api/v1/auth 的 Cookie 保存，无 Domain。勾选保持登录时 Cookie 持久化到绝对会话期限，未勾选使用 session Cookie；浏览器恢复会话可能保留 session Cookie，因此不承诺关闭窗口立即退出。主动退出以服务端撤销为准。默认绝对期限 30 天，由后端配置决定，刷新不延长。

所有鉴权 POST 需要有效白名单 Origin，并拒绝 Sec-Fetch-Site: cross-site。CORS 凭据只对精确白名单来源开放。即使调用者不是浏览器，也必须显式传 Origin；不提供 JSON refresh token 降级通道。跨站部署不在首版支持范围。

### 构建执行边界（P3 第一批）

- 构建状态为 queued/running/succeeded/failed/cancelled/interrupted；interrupted 由持有 worker 租约的实例在启动恢复时产生，表示进程中断而非业务失败。
- 触发时固化命令、分支与触发人快照，构建号在项目内自增；仓储地址不参与本批执行（不 clone），命令在临时目录用系统 shell 执行，超时由 AEGIS_BUILD_TIMEOUT 控制。
- 构建不改变账号/权限数据，也不写审计表；触发、取消与日志读取沿用全局角色授权（admin/member 可写，viewer 只读），每次请求由服务端校验，不依赖前端隐藏按钮。
- 执行环境尚非完全受控：以服务进程身份执行，没有专用执行账号、资源上限或输出脱敏；但构建命令只获得最小环境变量白名单（`PATH`/`HOME`/`TMPDIR`/`TMP`/`TEMP`/`LANG` 等，Windows 另加系统必需变量），不继承 `AEGIS_*` 服务凭据，`HOME` 与临时目录都指向 0700 的构建目录。不要把不可信仓库命令接入本批执行器。

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
- 持久化审计记录操作人（ID/用户名）、时间、动作、对象、结果、详情与请求 ID：登录成功、创建用户、修改密码（含当前密码错误的失败记录）、重置密码、禁用/启用、角色变更、会话撤销、构建触发（build.trigger）与取消（build.cancel）。构建审计与状态变更同事务提交；重复取消幂等且不重复记账，detail 不包含构建命令。失败登录受 Redis 限流约束，只进结构化日志不进审计表；审计保留策略待业务审计里程碑（P4）设计。审计查询仅管理员。
- 过期会话及其 refresh 令牌历史由后台任务每小时清理（启动时先执行一次）。重放检测只需保留到会话绝对期限，到期即删；清理不影响活跃会话。

## 角色矩阵

下面是首版权限约定。除已提供的鉴权、账号管理与审计接口，其他服务尚未实现；页面守卫用于交互，不能代替后端对象级授权。项目级成员与多租户暂不支持，首版角色为全局角色。

| 操作 | admin | member | viewer | 实现状态 |
| --- | --- | --- | --- | --- |
| 读取自己的资料、退出、修改自己密码、查看/撤销自己的会话 | 允许 | 允许 | 允许 | 已实现 |
| 创建/查询用户、重置密码、禁用/启用、变更角色、查询审计 | 允许 | 禁止 | 禁止 | 已实现 |
| 读取项目 | 允许 | 允许 | 允许 | 已实现（全局角色） |
| 创建/修改项目、归档与恢复 | 允许 | 允许 | 禁止 | 已实现（全局角色；项目级成员权限留待后续） |
| 读取构建/部署/审查/扫描/知识信息 | 允许 | 允许 | 允许 | 构建已实现（构建/日志读取）；其余待业务接口落实 |
| 发起/取消构建、发起审查 | 允许 | 允许 | 禁止 | 构建已实现；发起审查待业务接口落实 |
| 发起部署、回滚及执行审批 | 允许 | 禁止 | 禁止 | 后续业务接口落实 |
| 管理部署目标、密钥、策略、设置、防御及业务审计 | 允许 | 禁止 | 禁止 | 页面明确标记未实现，业务服务待实现 |

后续对象涉及具体项目时，需在服务端查询对象所属项目并校验访问权限。不要根据客户端提交的 role 或隐藏按钮决定授权。

## 开发模式和验收

前端不再提供业务演示数据或演示开关：项目、构建、账号与审计始终调用真实接口，未接入模块恒定显示未实现声明，不用演示数据冒充业务结果。登录与鉴权没有 mock 回退。

后端验证：go test ./...、go vet ./...。前端验证：npm run build、npm run lint、npm test（session store 单元测试）。浏览器验证：先 npx playwright install chromium，再 npm run test:e2e；先设置 AEGIS_TEST_DATABASE_URL/AEGIS_TEST_REDIS_URL 运行后端真实集成测试；浏览器测试需另设置 AEGIS_E2E_DATABASE_URL/AEGIS_E2E_REDIS_URL 指向专用可丢弃服务，测试启动本地前后端，要求 8080、5173 端口空闲。E2E 后端显式设置 AEGIS_LOGIN_RATE_LIMIT=100，因为同一地址在一分钟内驱动多个账号登录；生产默认配额仍是 10。测试账号仅存在测试临时库；端到端配置使用随机 JWT 密钥，不使用生产数据。

验收覆盖：错误密码、正确登录、Cookie 属性、页面恢复、并发刷新、多个标签页、退出撤销、非管理员 403、刷新 Cookie 失效及管理员页面守卫；账号管理覆盖 UI 创建用户、角色变更、重置密码、禁用后 403 登录拒绝、审计查询、自助修改密码保留当前会话、双会话撤销其他全部并即时踢出；构建闭环覆盖 UI 创建带构建命令的项目、触发构建、列表与详情、轮询日志中出现进程输出与 build succeeded、viewer 只读且触发 403。剩余能力（SSE 推送、受控执行环境、仓库 clone、多 worker 并行吞吐）、生产备份及业务审计见 [TODO](../TODO.md)。
