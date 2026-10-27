# AegisFlow

轻量交付治理平台。前端使用 React + TypeScript + Vite，Go 后端公共设施与本地账号鉴权已加入 `backend/`。

后端启动、环境变量、接口及验证命令见 [Go 后端文档](backend/README.md)。前端登录已接入真实鉴权：access token 仅存内存，refresh token 使用 HttpOnly Cookie，支持恢复、自动刷新及退出撤销。账号管理提供真实接口与页面（自助修改密码、会话管理、管理员用户/角色/禁用与审计查询）；项目管理（P2）与最小构建闭环（P3 第一批）已接入真实接口：首页为项目列表，支持创建、修改、归档与恢复，并在项目行触发构建；构建列表与详情展示状态、命令快照和轮询日志，可取消排队/运行中的构建；构建触发与取消写入审计表，管理员可在审计页查询。写操作按全局角色授权，并在其上叠加项目权限：项目可见性（public/private）与项目成员（maintainer/viewer）已实现服务端读过滤，私有项目对非成员返回 404；私有项目的写操作（改项目、触发/取消构建、管理成员）要求 maintainer，否则 403。前端项目列表显示公开/私有徽章并提供「成员」入口，创建向导可选可见性，成员管理页支持列表、添加、改角色与移除。

面向多人、多实例部署，采用 PostgreSQL 共享存储、Redis 共享限流与同源负载均衡；双实例联调样例见 [部署说明](deploy/README.md) 和 [Compose](compose.yaml)。角色矩阵与会话安全约定见 [鉴权契约](docs/auth-contract.md)，机器可读接口见 [OpenAPI](backend/openapi.yaml)。部署、审批、审查等其余业务接口尚未实现，页面恒定标记未实现，真实鉴权没有 mock 回退。

## 构建执行边界（P3 第一批）

构建命令在系统临时目录用 shell 执行，超时由 `AEGIS_BUILD_TIMEOUT` 控制；状态为排队/运行/成功/失败/取消/中断。多副本通过 PostgreSQL 会话级租约保证同一时刻只有一个 worker：只有持锁副本在启动时把遗留的运行中构建标记为中断并领取任务，其余副本待命，持锁进程消失后自动接管。构建命令只获得最小环境变量白名单（不继承 `AEGIS_*` 服务凭据），`HOME`/临时目录指向 0700 的构建目录。当前执行环境还不是受控执行环境：以服务进程身份运行，没有专用执行账号、资源上限或输出脱敏，也不会 clone 仓库，只执行项目配置的命令；不要把不可信仓库的命令接入本批执行器。日志与状态优先经 SSE 推送（前端用带 Bearer 的 fetch 流式读取，token 不进 URL），连接失败时自动回退到 `after` 游标轮询。

## 本地联调

1. 启动 PostgreSQL、Redis，按后端文档设置连接 URL、共享密钥、首次管理员账号和密码，在 backend 目录启动 Go 服务（127.0.0.1:8080）。
2. 在项目根目录运行 `npm install`、`npm run dev`，访问 **http://localhost:5173**。Vite 将 /api 转发至 Go 服务。
3. 使用初始化的管理员账号登录。项目、构建、账号安全与用户管理均为真实功能；创建带「构建命令」的项目后可在项目行直接触发构建。未接入的模块恒定显示未实现声明，不提供演示数据开关。
4. 如修改访问域名或端口，同步配置后端 `AEGIS_ALLOWED_ORIGINS`。生产使用 HTTPS 并启用 `AEGIS_COOKIE_SECURE=true`。

构建：`npm run build`；规范检查：`npm run lint`；单元测试：`npm test`；浏览器测试：`npx playwright install chromium` 后运行 `npm run test:e2e`（需设置专用测试服务的 `AEGIS_E2E_DATABASE_URL`、`AEGIS_E2E_REDIS_URL`，并保持 5173 和 8080 端口空闲）。

开发进度、优先级及各阶段验收标准见 [开发 TODO](TODO.md)。贡献流程与协作约束见 [项目约束](AGENTS.md)：所有变更走 PR，且每个 PR 需仓库所有者 review 后合并。
