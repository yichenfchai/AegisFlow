# AegisFlow 开发 TODO

更新日期：2026-10-27（P1、P2 已完成；P3 第一批构建闭环已落地；项目可见性与成员 P1 已落地）。按依赖顺序推进，每项完成后更新勾选和验证结果。当前采用 React + Go/Gin 的模块化单体，真实浏览器鉴权、账号管理（P1）、项目注册表（P2 第一批）、最小构建闭环（P3 第一批）与项目可见性/成员读过滤已打通；其余业务页面恒定显示未实现声明。所有变更走 PR 并由所有者 review（见 AGENTS.md）。

## 已完成

- [x] 建立 Go 后端模块，接入 Gin 路由组及认证、管理员权限中间件。
- [x] 实现配置校验、结构化日志、请求 ID、统一 JSON 响应、异常恢复、CORS、登录限流及 HTTP 超时。
- [x] 实现 PostgreSQL 版本化迁移、健康检查、优雅退出；已替代早期 SQLite 实现。
- [x] 实现用户持久化、首次管理员初始化、bcrypt 密码哈希及 admin/member/viewer 角色。
- [x] 实现 JWT、refresh token 哈希存储和轮换、重放撤销及退出撤销会话。
- [x] 提供登录、刷新、退出、当前用户及管理员创建/查询用户接口。
- [x] 编写鉴权和 HTTP 集成测试；通过 go test ./...、go vet ./... 及编译。
- [x] 推送后端基础版本到 main，提交 350a9b2。
- [x] P1 账号管理（2026-10-25）：迁移 v2（disabled/时间戳/审计表）；自助改密（保留当前会话）、管理员重置密码、禁用/启用、角色变更、最后管理员保护；会话列表与撤销；审计持久化与分页查询；过期会话及 refresh 历史每小时清理；用户与审计分页。前端账号安全页（/account）与管理员用户管理/审计页（/settings）接入真实接口。
- [x] 演示页收敛：仅项目/构建主线保留 BusinessGate 演示，其余业务页面恒定显示未实现声明。
- [x] 收尾杂项：用户可见的 GoPulse 文案统一为 AegisFlow，README 移除 Vite 模板样板，引入 vitest 覆盖 session store 并接入 CI。
- [x] P1 运维验收（2026-10-25）：`cmd/server/ops_test.go` 真实进程生命周期测试——编译真实二进制、随机端口启动、readyz、登录并创建项目、Windows CTRL_BREAK/SIGTERM 优雅退出（退出码 0）、重启后 access token/refresh Cookie/项目数据全部存活；已接入 CI。
- [x] P2 项目管理第一批（2026-10-25）：迁移 v3 项目表（共享账本抽出 internal/dbmigrate）；internal/projects 注册表（校验/大小写唯一/归档只读/分页/事务内审计，全局角色授权，不依赖 Gin）；项目 CRUD + 归档端点与 OpenAPI；首页替换为真实项目列表、创建向导接真实接口。
- [x] P3 第一批最小构建闭环（2026-10-26）：迁移 v4（projects.build_command/build_count、builds 六状态表、build_log_events 游标表）；internal/builds 服务与进程内单并发 worker（SKIP LOCKED 领取、临时目录执行、超时与取消看门狗、批量日志与截断、重启恢复 interrupted）；httpapi 五个构建端点 + OpenAPI + RBAC；前端构建列表/详情接真实接口（`after` 游标累积轮询日志、取消按钮），项目行触发构建，创建向导支持构建命令；E2E 第六组与 E2E CI job；随后补多副本 worker 租约（PostgreSQL 会话级 advisory lock，只有持锁副本恢复与执行，待命副本自动接管）。
- [x] P3 构建操作审计（2026-10-26）：触发（build.trigger）与取消（build.cancel）在状态变更同一事务写入 auth_audit，objectType 为 build；detail 只记录项目与构建号，不写命令；幂等重复取消不重复记账；OpenAPI 审计枚举、后端 README/鉴权契约、前端审计页中文映射（含对象类型）同步；E2E 断言管理员审计页可见“触发构建”。
- [x] 前端演示层清理（2026-10-26）：删除全部未路由演示页（仪表盘、部署、审查、扫描、审批、知识库、攻击防御、策略、密钥、部署目标、演示审计）、mock 数据层（`src/api/mock`、`src/api/sse`）、假遥测（`stores/telemetry`）与只被演示页使用的 `BusinessGate`/`PlaceholderPage`/`StageTimeline`/`CountdownButton`；移除 `VITE_ENABLE_BUSINESS_MOCKS` 及其文档/测试配置；底部状态条改为陈述真实服务范围。未接入路由恒定显示 `UnimplementedPage`。
- [x] 构建命令环境隔离（2026-10-26，④a 安全子集）：`cmd.Env` 改为显式白名单（PATH/HOME/TMPDIR/TMP/TEMP/LANG；Windows 加 SYSTEMROOT 等），不再继承 `AEGIS_*` 服务凭据；`HOME`/临时目录指向构建目录；构建目录显式 0700；新增单元测试与真实执行 `env`/`set` 的集成测试。
- [x] P3 构建 SSE 推送（2026-10-26）：`GET /api/v1/builds/{id}/events` 流式推送 status/logs/done，`Last-Event-ID`/`after` 续传、15s 心跳、终态静默收尾、单连接 10 分钟上限后要求重连；为长连接单独豁免服务器写超时；前端用带 Bearer 的 fetch 流式读取（EventSource 无法带 Authorization，token 不进 URL），连接失败重试一次后回退现有游标轮询，并在日志工具条显示“实时推送”。
- [x] 项目可见性与成员（2026-10-27，⑤ P1）：迁移 v5（`projects.visibility` + `project_members`，存量项目按 `created_by` 补写 maintainer 行）；`internal/projects` 增加可见性谓词（唯一实现，构建查询复用）与成员增删改；项目与构建的列表、详情、日志、SSE、触发、取消全部按可见性过滤，不可见一律 404 防枚举；成员 API 四端点；创建者不可移除、重复成员 409、目标账号不存在 404；OpenAPI、鉴权契约、后端/根 README 同步。写权限叠加到成员 maintainer（P2）与前端成员页（P3）待做。

## P0：确定接口约定并打通真实鉴权

- [x] 目标确定为多人、多实例服务：PostgreSQL 共享存储、Redis 共享限流和同源负载均衡，已移除 SQLite 运行依赖。
- [x] 跨实例安全的版本迁移、首管理员初始化与 session 行锁；独立数据库池及两个 HTTP 实例验证刷新、退出和重放撤销一致。
- [x] Redis Lua 原子限流、故障返回503、显式可信代理 CIDR；两个独立客户端共享配额并发验证。
- [x] 提供双应用副本 Compose、Nginx 样例和部署说明；本机未安装 Docker，容器样例尚待 Docker 环境实际启动验证。
- [x] 定义全局角色权限矩阵，覆盖用户、项目、构建、部署、审批、密钥与审计；项目级成员权限留待后续设计，业务授权尚未实现的部分已标注。
- [x] 补充 OpenAPI 契约，记录鉴权、用户和健康接口的请求、响应、错误码及安全约定。
- [x] 落实内存 access token + HttpOnly refresh Cookie；Strict SameSite、显式 Secure、Origin/Fetch Metadata 防护、凭据 CORS 及同源部署约定。
- [x] 建立真实 API 客户端，处理错误、网络超时和 Bearer；业务 mock 显式开启，鉴权始终真实且无降级。
- [x] 登录页接入真实登录，删除 mock JWT 与任意非空账号通过行为。
- [x] 实现 Cookie 恢复及 /auth/me、登录/管理员路由守卫、加载状态和网络错误重试。
- [x] 实现自动刷新、同页面并发合并、401 最多重试一次、Web Locks 多标签页串行化和 BroadcastChannel 账号/退出通知；refresh 401 清空会话，网络失败可重试。
- [x] 服务端退出成功后清理状态及缓存，失败可重试；保持登录对应持久/会话 Cookie，LDAP 明确禁用。
- [x] Chromium 真实前后端端到端测试通过：错误密码、登录、页面恢复、过期刷新、并发、多标签页、退出撤销、refresh失效、普通用户403、Cookie属性和退出网络失败重试。

验收标准：关闭 mock 后，可在浏览器完成登录 → 获取真实用户 → 自动刷新 → 退出；退出后旧令牌访问返回 401，非管理员访问用户管理返回 403。

## P1：补齐账号管理及必要运维能力

- [x] 实现修改密码、管理员重置密码、禁用/启用用户和修改角色；密码变更与禁用应撤销相关会话。
- [x] 增加最后一个可用管理员保护，避免误禁用或降权导致无法管理系统。
- [x] 实现当前用户会话列表与撤销指定会话/全部会话，接口避免泄露令牌。
- [x] 增加过期会话及 refresh 历史清理策略：会话有效期内保留重放检测记录，到期后由每小时后台任务清理。
- [x] 增加用户列表分页；接入前端用户管理页面并验证角色限制。
- [x] 持久化必要鉴权与管理审计：操作人、时间、对象、结果、请求 ID；不记录密码、令牌和密钥明文。失败登录受限流约束，仅进结构化日志。
- [x] 建立 PostgreSQL 版本化迁移和并发升级/未知版本拒绝验证。
- [x] 按模块需要逐步抽出 Repository，保持业务逻辑不依赖 Gin。（以 internal/projects 起步：业务包自带校验与事务内审计、不依赖 Gin；迁移账本抽出 internal/dbmigrate 共享。auth 包暂保持内聚。）
- [x] 建立 CI：后端测试、静态检查与构建，前端类型检查、单元测试、lint 与构建；后端在真实服务下执行 race 检查。
- [x] 完成真实进程启动、就绪检查、优雅退出及重启持久化验证（cmd/server/ops_test.go，Windows CTRL_BREAK/Unix SIGTERM，已接入 CI）。本机便携 PostgreSQL 环境下未再出现安全软件拦截生成的二进制。
- [x] 真实 PostgreSQL + Redis 下全后端 go test -race ./... 通过；2026-10-25 以本机便携 PostgreSQL 17 + Redis 8.8 复验全量通过。

验收标准：管理员能够管理账号，用户可更改密码并撤销会话；禁用账号立即失效，重要操作有可查询审计，CI 提供基础验证。（2026-10-25 全部达成，P1 关闭。）

## P2：项目管理完整链路

- [x] 设计项目表、项目状态与成员关系；约定本地路径/仓库地址、分支等字段的适用部署场景及校验规则。（迁移 v3；repoUrl 支持 https/git@/file:/// 与绝对本地路径，分支做轻量 git 引用校验；项目级成员明确留待后续。）
- [x] 实现项目创建、详情、分页列表、修改、归档接口及服务端权限校验。（全局角色：admin/member 写、viewer 只读；归档项目只读直到恢复。）
- [x] 接入项目页面与创建向导，逐步替换对应 mock。（首页为真实项目列表，创建向导接真实接口；构建页已在 P3 第一批接真实接口。）
- [x] 验证跨项目访问权限、重复创建、非法路径/地址及归档项目限制。（服务层/HTTP 集成测试 + E2E；数据重启后保留由 ops_test 验证。）

验收标准：页面操作真实 API，数据重启后保留，不同角色与项目成员权限符合矩阵。（全局角色部分已达成；项目级成员权限按计划留待后续设计。）

## P3：最小构建闭环

- [x] 定义构建任务状态机：排队、执行、成功、失败、取消；明确合法转换及重启恢复规则。（六态含 `interrupted`：由持有 worker 租约的副本在启动时把遗留 `running` 统一转为中断；租约为 PostgreSQL 会话级 advisory lock，保证同一部署只有一个副本执行与恢复。）
- [ ] 设计任务、阶段、日志及产物数据模型，实现触发、详情、列表、取消和重试接口。（第一批已落地 builds / build_log_events 模型与触发、详情、列表、取消、游标日志接口；阶段、产物与重试未做。）
- [ ] 建立受控执行环境：工作目录、执行身份、超时、取消传播、资源上限及敏感信息脱敏；隔离不可信仓库代码。（已实现：临时工作目录、超时/取消传播、0700 目录、环境变量白名单（不继承 `AEGIS_*`）。仍待做：专用执行身份、CPU/内存/进程数上限、输出脱敏、仓库 clone 与隔离；设计见 [受控执行环境与 clone 设计](docs/build-execution-design.md)，待所有者确认 D1-D6 后按 ④a/④b/④c 拆 PR。勿把不可信仓库命令接入。）
- [x] 实现最小任务调度与 worker，限制并发；根据实际需求决定是否引入外部队列。（进程内单并发 worker，PostgreSQL 会话锁保证多副本单 worker，`FOR UPDATE SKIP LOCKED` 领取兜底；暂不引入外部队列，也不做多 worker 并行吞吐。）
- [x] 实现 SSE 日志与状态推送，支持断线恢复、事件序号及权限校验。（`GET /builds/{id}/events`：status/logs/done 帧、事件 id 即续传序号、`Last-Event-ID`/`after` 断点续传、心跳与终态收尾、沿用 Bearer + 全局角色校验；前端失败自动回退轮询。）
- [x] 接入构建列表和详情，验证一次真实构建从触发到结束，以及失败、取消和服务重启场景。（前端真实列表/详情/取消；E2E 覆盖触发→成功→日志；失败、超时、取消、重启中断由 internal/builds 集成测试覆盖。）

## P4：部署、审批与审计

- [ ] 实现部署目标管理、连通性和健康检查；密钥加密存储并限制读取权限。
- [ ] 实现部署任务、版本记录、失败处理与回滚，记录操作审计。
- [ ] 实现审批状态机、操作权限、超时规则与幂等处理，将审批结果接入部署等实际操作。
- [ ] 实现审计分页查询、筛选和保留策略，接入审计页面。
- [ ] 为项目、构建、部署、审批逐项补齐对象级授权和端到端测试。（项目级成员与可见性的 P1 后端已落地：可见性读过滤、成员 API、404 防枚举；剩余 P2 写权限叠加/成员审计、P3 前端成员页与 E2E 见 [项目级成员权限设计](docs/project-permissions-design.md) §10。）

## 按实际需求推进

- [ ] 扩展代码审查、扫描、知识库、防御策略等业务；分别定义最小接口和验收标准。
- [ ] 需要统一身份源时接入 LDAP/OIDC，并定义外部身份到本地角色的映射。
- [x] 多实例共享存储、集中登录限流及可信代理配置。
- [ ] 构建模块增加跨实例任务领取、幂等执行及 worker 恢复协调。（单 worker 租约、恢复归属与待命接管已实现；多 worker 并行领取与幂等执行未做。）
- [ ] 补齐部署说明、TLS 反向代理配置、数据库备份恢复及升级流程。
- [ ] 增加运行指标、告警和按需求引入链路追踪。

## 参考

P0 验证记录：`npm run build` 通过；`npm run lint` 为 0 错误，保留既有业务演示代码的 6 条警告；`npm run test:e2e` 三组真实浏览器测试通过。后端 `go test ./...`、`go vet ./...` 通过；npm 兼容安全补丁更新后 audit 为 0 漏洞。浏览器验收已成功启动真实后端。2026-10-23 迁移后真实 PostgreSQL/Redis 下全量 test/vet/build 和全后端 race 通过，Chromium 三组 E2E 回归通过；并发鉴权测试重复十次通过。优雅退出运维验收仍保留在 P1，Docker Compose 未在本机启动验证。

P1 验证记录（2026-10-25）：`npm run build`、`npm run lint`（0 错误、6 条既有警告）、`npm test`（session store 单元测试 10 用例）通过；`npm run test:e2e` 四组真实浏览器测试通过，新增账号管理全流程（UI 创建、角色变更、重置密码、禁用后 403 登录、审计查询、自助改密保留当前会话、双会话撤销其他全部）。后端 `go build`、`go vet`、`gofmt` 通过；以本机便携 PostgreSQL 17（5433）+ Redis 8.8 在真实服务下 `go test -race ./...` 全量通过。CI 已加入前端单元测试步骤。

P1 运维验收 + P2 第一批验证记录（2026-10-25）：`cmd/server/ops_test.go` 真实进程生命周期测试通过（随机端口启动、readyz、登录并创建项目、优雅退出退出码 0、重启后 access token/refresh Cookie/项目数据存活，约 7 秒），已接入 CI。projects 服务 4 组集成测试、httpapi 项目路由测试通过；E2E 扩至五组，新增项目管理（UI 创建、归档/恢复、审计记录、viewer 无入口且 API 403）。`go test -race ./...` 全量、`gofmt`/`go vet`、前端 build/lint/vitest 复验通过。

P3 第一批验证记录（2026-10-26）：后端 `go build ./...`、`go vet ./...`、`gofmt -l ./cmd ./internal` 为空；本机便携 PostgreSQL 17（5433）+ Redis 8.8 下 `go test -race ./... -count=1` 全量通过（新增 internal/builds 集成测试：触发校验与编号、成功、失败退出码、超时、取消与幂等、重启恢复 interrupted、游标日志；httpapi 构建路由 2 组 + ratelimit 配置配额用例）。前端 `npm run build`、`npm run lint`（0 错误、5 条既有演示警告）、`npm test`（session store 10 用例）通过。E2E 六组通过：auth.spec 五组 + builds.spec 构建闭环（UI 创建带构建命令的项目 → 触发 → 列表/详情 → 日志出现输出与 `build succeeded` → viewer 只读且触发 403）。E2E job 已加入 CI（PG17/Redis7.4 services、一次性 `aegis_e2e` 库）；测试环境以 `AEGIS_LOGIN_RATE_LIMIT=100` 启动后端，避免同地址多账号用例触发生产默认的 10 次/分钟登录限流。

多副本 worker 租约验证记录（2026-10-26）：`go build ./...`、`go vet ./...`、gofmt（LF 规范化副本）通过；真实 PostgreSQL 17 + Redis 8.8 下 `go test -race ./... -count=1` 全绿（internal/builds 8 组，含新增 `TestWorkerLeaseIsExclusiveAcrossInstances` 与 `TestStandbyDoesNotRecoverAnotherInstancesBuild`：两个独立连接池竞争同一 schema 租约；持锁期间待命副本不改动 running 构建；释放后待命副本接管并恢复为 interrupted 且输出 `interrupted builds recovered`）。E2E 六组在重置 `aegis_e2e` 后通过（1.9m）。

CI 暴露的既有缺陷修复（2026-10-26，随 worker 租约 PR 一并提交）：① 输出丢失——`cmd.Wait` 先于管道读取关闭读端，快命令的 stdout 可能被丢弃；改为先 drain 再 Wait，并在取消/超时后关闭父端读口，避免 Windows 上 `cmd /c ping` 的孙进程占住管道；新增 900 行输出回归用例（internal/builds 9 组）。② 短 token 下预刷新风暴——固定的 5s 提前刷新阈值在 `AEGIS_ACCESS_TTL=3s` 的 E2E 中使每个请求都轮换 Cookie，导航中断的轮换会让新页面重放已消费的 Cookie 而被判重放并登出；改为按 token 生命周期缩放提前量，新增 session store 单测（11 例）。本地 `npm run build`、`npm run lint`（0 错误、5 警告）、`npm test`（11 例）与 E2E 六组通过；CI 侧 backend/frontend 通过，e2e 结果见 PR。

P3 构建操作审计验证记录（2026-10-26）：`go build ./...`、`go vet ./...`、gofmt（LF 规范化副本）通过；真实 PostgreSQL 17 + Redis 8.8 下 `go test -race ./... -count=1` 全绿（internal/builds 12 组，新增 3 个审计用例：触发写一条 build.trigger、取消与幂等只写一条 build.cancel、被拒绝的触发不写审计；httpapi 新增 1 个用例：构建触发/取消后管理员审计接口可见 build.* 与 objectType build，detail 不泄露命令，viewer 仍 403）。前端 `npm run build`、`npm run lint`（0 错误、5 条既有警告）、`npm test`（11 例）通过；E2E 六组通过（重置 `aegis_e2e`），构建用例新增管理员审计页断言（“触发构建”与“构建:对象”）。

前端演示层清理验证记录（2026-10-26）：`npm run build` 通过（Vite 模块数 103→96，产物体积降低）；`npm run lint` 0 错误、2 条警告（清理前 5 条，均为演示代码警告）；`npm test` 11/11；重置 `aegis_e2e` 后 E2E 六组通过（2.1m）。`AGENTS.md` 的「演示页边界」条目仍保留，待所有者在下一次约束更新时删除（本次未改约束文档）。

构建命令环境隔离验证记录（2026-10-26）：`go build ./...`、`go vet ./...`、gofmt（LF 规范化副本）通过；真实 PostgreSQL 17 + Redis 8.8 下 `go test -race ./... -count=1` 全绿（internal/builds 15 组，新增 3 个用例：白名单不含 `AEGIS_*`、构建目录 0700、真实执行 `env`/`set` 断言日志无服务凭据且 HOME 位于 `aegis-build-*`）。E2E 六组通过（重置 `aegis_e2e`）。同 UID 下仍可读取服务进程的 `/proc/<pid>/environ` 或直连数据库，这属于 ④b 专用执行身份的范围，本 PR 只关闭环境变量继承。

P3 构建 SSE 验证记录（2026-10-26）：`go build ./...`、`go vet ./...`、gofmt（LF 规范化副本）通过；真实 PostgreSQL 17 + Redis 8.8 下 `go test -race ./... -count=1` 全绿（httpapi 新增 2 组 SSE 用例：真实 httptest 服务器配置 200ms 写超时，仍能在延迟命令后收到输出与终态 done；负游标/非数字游标/未知构建/未鉴权分别 400/400/404/401）。前端 `npm run build`、`npm run lint`（0 错误、2 条既有警告）、`npm test`（14 例，含 SSE 分帧解析 3 例）通过；E2E 六组通过（重置 `aegis_e2e`），构建用例断言 `/events` 返回 200 与 `text/event-stream` 且日志经推送到达。

项目可见性与成员 P1 验证记录（2026-10-27）：`go build ./...`、`go vet ./...`、gofmt（LF 规范化副本）通过；真实 PostgreSQL 17（5433）+ Redis 8.8 下 `go test -race ./... -count=1` 全绿。新增用例：`internal/projects` 2 组（可见性过滤：admin 全见、非成员只见 public、非成员 Get/Update 均 404、成员 viewer 读、切回 public 恢复；成员生命周期：创建者为 maintainer、添加/重复 409/账号不存在 404/非法角色 400/改角色/未知成员 404/创建者不可移除 400/移除后失去可见性/非成员列成员 404）；`internal/builds` 1 组（private 项目下 outsider 的 List 为空、Get/Logs/Trigger/Cancel 全 404，创建者与 admin 不受影响）；`httpapi` 1 组（私有项目对非成员四条路径全 404、成员列表、重复 409、未知账号 404、非法角色 400、成员可读、全局 viewer 仍 403 写、提升/移除成员、创建者不可移除、public 项目对所有角色行为不变）。OpenAPI 契约用例新增 4 条成员路由。前端未改动；E2E 六组在重置 `aegis_e2e` 后通过，验证 public 项目行为未变。

- [鉴权、部署与权限约定](docs/auth-contract.md)
- [OpenAPI](backend/openapi.yaml)
- [项目说明](README.md)
- [后端启动、接口与架构](backend/README.md)
