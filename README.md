# AegisFlow

轻量交付治理平台。前端使用 React + TypeScript + Vite，Go 后端公共设施与本地账号鉴权已加入 `backend/`。

后端启动、环境变量、接口及验证命令见 [Go 后端文档](backend/README.md)。前端登录已接入真实鉴权：access token 仅存内存，refresh token 使用 HttpOnly Cookie，支持恢复、自动刷新及退出撤销。账号管理提供真实接口与页面（自助修改密码、会话管理、管理员用户/角色/禁用与审计查询）；项目管理（P2）与最小构建闭环（P3 第一批）已接入真实接口：首页为项目列表，支持创建、修改、归档与恢复，并在项目行触发构建；构建列表与详情展示状态、命令快照和轮询日志，可取消排队/运行中的构建，终态构建可一键重试（沿用原分支、使用项目当前命令）；构建触发、取消与重试写入审计表，管理员可在审计页按动作、对象、结果、操作人与日期范围筛选查询（筛选在服务端生效；`AEGIS_AUDIT_RETENTION` 可配置保留窗口，默认永久保留）。写操作按全局角色授权，并在其上叠加项目权限：项目可见性（public/private）与项目成员（maintainer/viewer）已实现服务端读过滤，私有项目对非成员返回 404；私有项目的写操作（改项目、触发/取消构建、管理成员）要求 maintainer，否则 403。前端项目列表显示公开/私有徽章并提供「成员」入口，创建向导可选可见性，成员管理页支持列表、添加、改角色与移除。部署目标（P4.1）已提供 admin-only 接口与页面：HTTP(S) 目标 CRUD、连通性探测、凭据以 AES-256-GCM 加密存储且从不回传，前端支持列表/新建/详情、检查与替换/清除凭据；部署任务（P4.2）已支持创建、单 worker 执行、事件游标、取消与回滚；部署审批（P4.3）已支持生产环境默认审批、批准/拒绝、超时过期与取消联动；部署与审批均为 admin-only。部署记录、部署详情与审批页面已接入：服务端分页列表与项目/状态筛选、新建部署、详情（事件轮询、取消、回滚与「回滚自 #n」）以及审批中心（状态筛选、批准/拒绝+理由）。

面向多人、多实例部署，采用 PostgreSQL 共享存储、Redis 共享限流与同源负载均衡；双实例联调样例见 [部署说明](deploy/README.md) 和 [Compose](compose.yaml)。角色矩阵与会话安全约定见 [鉴权契约](docs/auth-contract.md)，机器可读接口见 [OpenAPI](backend/openapi.yaml)。审批、审查等其余业务接口尚未实现（部署目标、部署任务与回滚已实现），页面恒定标记未实现，真实鉴权没有 mock 回退。

## 构建执行边界（P3 第一批）

构建命令在系统临时目录用 shell 执行，超时由 `AEGIS_BUILD_TIMEOUT` 控制，单次工作区大小受 `AEGIS_BUILD_DISK`（默认 2g）约束；状态为排队/运行/成功/失败/取消/中断。多副本通过 PostgreSQL 会话级租约保证同一时刻只有一个 worker：只有持锁副本在启动时把遗留的运行中构建标记为中断并领取任务，其余副本待命，持锁进程消失后自动接管。执行隔离由 `AEGIS_BUILD_SANDBOX` 选择：`container`（生产推荐）为每个构建启动一次性容器——无网络、只读根文件系统、只有构建目录可写，CPU/内存/进程数硬上限，超时或取消会杀掉容器；`user`（仅 Linux，服务需以 root 启动）通过 `setpriv` 把构建降权到 `AEGIS_BUILD_USER` 指定的专用账号；`none`（默认）以服务进程身份执行，仅供开发。三种模式都不继承 `AEGIS_*` 服务凭据，`HOME`/临时目录位于 0700 构建目录，输出在落库前脱敏（fail closed）。`AEGIS_BUILD_CLONE=true` 时先浅克隆项目仓库（`repoUrl`）再在仓库内执行命令，只支持无凭据的 https / `git@` 地址，本地路径需额外开启 `AEGIS_BUILD_ALLOW_LOCAL_REPO`，私有仓库凭据本批不支持。日志与状态优先经 SSE 推送（前端用带 Bearer 的 fetch 流式读取，token 不进 URL），连接失败时自动回退到 `after` 游标轮询。

## 本地联调

1. 启动 PostgreSQL、Redis，按后端文档设置连接 URL、共享密钥、首次管理员账号和密码，在 backend 目录启动 Go 服务（127.0.0.1:8080）。
2. 在项目根目录运行 `npm install`、`npm run dev`，访问 **http://localhost:5173**。Vite 将 /api 转发至 Go 服务。
3. 使用初始化的管理员账号登录。项目、构建、账号安全与用户管理均为真实功能；创建带「构建命令」的项目后可在项目行直接触发构建。未接入的模块恒定显示未实现声明，不提供演示数据开关。
4. 如修改访问域名或端口，同步配置后端 `AEGIS_ALLOWED_ORIGINS`。生产使用 HTTPS 并启用 `AEGIS_COOKIE_SECURE=true`。

构建：`npm run build`；规范检查：`npm run lint`；单元测试：`npm test`；浏览器测试：`npx playwright install chromium` 后运行 `npm run test:e2e`（需设置专用测试服务的 `AEGIS_E2E_DATABASE_URL`、`AEGIS_E2E_REDIS_URL`，并保持 5173 和 8080 端口空闲）。

开发进度、阶段验收与验证记录随 PR 维护（过程文档只在本地，不入库，见 [项目约束](AGENTS.md)）。贡献流程与协作约束见 [项目约束](AGENTS.md)：所有变更走 PR，且每个 PR 需仓库所有者 review 后合并。
