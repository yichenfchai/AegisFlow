# AegisFlow

轻量交付治理平台。前端使用 React + TypeScript + Vite，Go 后端公共设施与本地账号鉴权已加入 `backend/`。

后端启动、环境变量、接口及验证命令见 [Go 后端文档](backend/README.md)。前端登录已接入真实鉴权：access token 仅存内存，refresh token 使用 HttpOnly Cookie，支持恢复、自动刷新及退出撤销。账号管理提供真实接口与页面（自助修改密码、会话管理、管理员用户/角色/禁用与审计查询）；项目管理（P2）已接入真实接口：首页为项目列表，支持创建、修改、归档与恢复，按全局角色授权。

面向多人、多实例部署，采用 PostgreSQL 共享存储、Redis 共享限流与同源负载均衡；双实例联调样例见 [部署说明](deploy/README.md) 和 [Compose](compose.yaml)。角色矩阵与会话安全约定见 [鉴权契约](docs/auth-contract.md)，机器可读接口见 [OpenAPI](backend/openapi.yaml)。构建等其余业务接口尚未实现：构建链路演示仅在显式启用时开放，其余页面明确标记未实现，真实鉴权没有 mock 回退。

## 本地联调

1. 启动 PostgreSQL、Redis，按后端文档设置连接 URL、共享密钥、首次管理员账号和密码，在 backend 目录启动 Go 服务（127.0.0.1:8080）。
2. 在项目根目录运行 `npm install`、`npm run dev`，访问 **http://localhost:5173**。Vite 将 /api 转发至 Go 服务。
3. 使用初始化的管理员账号登录。账号安全与用户管理为真实功能；如需预览项目/构建业务 UI，在本机 `.env.local` 设置 `VITE_ENABLE_BUSINESS_MOCKS=true` 并重启 Vite，登录仍需真实账号。
4. 如修改访问域名或端口，同步配置后端 `AEGIS_ALLOWED_ORIGINS`。生产使用 HTTPS 并启用 `AEGIS_COOKIE_SECURE=true`。

构建：`npm run build`；规范检查：`npm run lint`；单元测试：`npm test`；浏览器测试：`npx playwright install chromium` 后运行 `npm run test:e2e`（需设置专用测试服务的 `AEGIS_E2E_DATABASE_URL`、`AEGIS_E2E_REDIS_URL`，并保持 5173 和 8080 端口空闲）。

开发进度、优先级及各阶段验收标准见 [开发 TODO](TODO.md)。贡献流程与协作约束见 [项目约束](AGENTS.md)：所有变更走 PR，且每个 PR 需仓库所有者 review 后合并。
