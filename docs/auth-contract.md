# 首版部署、权限与浏览器鉴权约定

## 范围和部署决定

产品目标已明确为多人使用、多实例部署。采用 PostgreSQL 共享用户与会话、Redis 共享登录限流，多个 Go/Gin 应用副本通过负载均衡提供同源接口，无需粘性会话。SQLite 不再作为运行存储，旧开发数据不自动导入。业务数据访问按模块逐步抽出 Repository，Gin Context 不进入业务服务。任务 worker 的协调在构建模块实现时设计，多租户和项目成员权限尚未实现。

数据库迁移与首次管理员创建使用 advisory 事务锁；刷新与退出锁定共享 session 行，跨实例并发消费旧令牌会触发一致撤销。所有实例须使用一致 JWT 密钥、会话配置与 Redis namespace。登录限流失败返回503，无进程内fallback。入口代理CIDR由 AEGIS_TRUSTED_PROXIES 明确指定，默认不信任转发头。部署样例和运维边界见 [部署说明](../deploy/README.md)。

推荐通过一个 HTTPS 域名提供前端，并把 /api 代理到 Go 服务。开发使用 http://localhost:5173，Vite 已把 /api 代理到 127.0.0.1:8080。AEGIS_ALLOWED_ORIGINS 必须包含浏览器实际 Origin（协议、主机、端口完全匹配）。生产设置 AEGIS_COOKIE_SECURE=true。不要混用 localhost 和 127.0.0.1。

## 真实接口契约

机器可读契约见 [OpenAPI](../backend/openapi.yaml)，启动参数见 [后端文档](../backend/README.md)。响应使用统一 data/error/requestId 包装；时间字段命名含义和单位必须在各业务 schema 明确。

| 接口 | 请求 | 响应及效果 |
| --- | --- | --- |
| POST /api/v1/auth/login | JSON username/password/remember | data.user/accessToken/expiresIn；设置 refresh Cookie |
| POST /api/v1/auth/refresh | JSON {}，携带 Cookie | 轮换 Cookie，返回新 accessToken 和服务端用户 |
| POST /api/v1/auth/logout | JSON {}，携带 Cookie | 撤销整个会话并删除 Cookie；幂等 |
| GET /api/v1/auth/me | Bearer accessToken | 读取数据库会话与用户当前角色 |
| GET /api/v1/users | 管理员 Bearer | 用户列表，不返回密码哈希 |
| POST /api/v1/users | 管理员 Bearer，JSON 用户字段 | 创建用户，返回 201 |

accessToken 仅存页面内存，不放 localStorage/sessionStorage。refresh token 不返回 JSON，由 HttpOnly、SameSite=Strict、Path=/api/v1/auth 的 Cookie 保存，无 Domain。勾选保持登录时 Cookie 持久化到绝对会话期限，未勾选使用 session Cookie；浏览器恢复会话可能保留 session Cookie，因此不承诺关闭窗口立即退出。主动退出以服务端撤销为准。默认绝对期限 30 天，由后端配置决定，刷新不延长。

所有鉴权 POST 需要有效白名单 Origin，并拒绝 Sec-Fetch-Site: cross-site。CORS 凭据只对精确白名单来源开放。即使调用者不是浏览器，也必须显式传 Origin；不提供 JSON refresh token 降级通道。跨站部署不在首版支持范围。

## 客户端会话生命周期

1. 启动通过 Cookie 刷新，随后 /auth/me 获取真实用户；等待恢复完成才进入业务布局。
2. 登录只调用真实后端，不接受 mock JWT；旧 gp_session 本地缓存会被清理。
3. access 即将过期时自动刷新；API 客户端对 401 最多刷新并重试一次，403 不刷新。
4. 同页面共享一次刷新，Web Locks 串行化同源多标签页的 Cookie 轮换，BroadcastChannel 通知账号变更和退出。首版要求支持这些 API 的现代浏览器，且运行于 localhost 或 HTTPS 安全上下文；Web Locks 不可用时明确报错。
5. refresh 返回 401 时清空内存并回到登录。网络失败可重试，不把断网误判为账号失效。
6. 退出等待服务端撤销成功才清理状态；网络失败提示重试，不显示虚假退出成功。账号切换和退出清理 React Query 缓存。

## 角色矩阵

下面是首版权限约定。除已提供的鉴权与用户接口，其他服务尚未实现；页面守卫用于交互，不能代替后端对象级授权。项目级成员与多租户暂不支持，首版角色为全局角色。

| 操作 | admin | member | viewer | 实现状态 |
| --- | --- | --- | --- | --- |
| 读取自己的资料、退出 | 允许 | 允许 | 允许 | 已实现 |
| 创建/查询用户 | 允许 | 禁止 | 禁止 | 已实现 |
| 读取项目/构建/部署/审查/扫描/知识信息 | 允许 | 允许 | 允许 | 后续业务接口落实 |
| 创建/修改项目、发起/取消构建、发起审查 | 允许 | 允许 | 禁止 | 后续业务接口落实 |
| 发起部署、回滚及执行审批 | 允许 | 禁止 | 禁止 | 后续业务接口落实 |
| 管理部署目标、密钥、策略、设置、防御及查询审计 | 允许 | 禁止 | 禁止 | 当前页面守卫，业务服务待实现 |

后续对象涉及具体项目时，需在服务端查询对象所属项目并校验访问权限。不要根据客户端提交的 role 或隐藏按钮决定授权。

## 开发模式和验收

VITE_ENABLE_BUSINESS_MOCKS=false 是默认：业务服务未接入时明确显示状态；只有显式设 true 才显示业务演示数据。两种模式的登录都是真实鉴权，mock 从来不是认证回退。

后端验证：go test ./...、go vet ./...。前端验证：npm run build、npm run lint。浏览器验证：先 npx playwright install chromium，再 npm run test:e2e；先设置 AEGIS_TEST_DATABASE_URL/AEGIS_TEST_REDIS_URL 运行后端真实集成测试；浏览器测试需另设置 AEGIS_E2E_DATABASE_URL/AEGIS_E2E_REDIS_URL 指向专用可丢弃服务，测试启动本地前后端，要求 8080、5173 端口空闲。测试账号仅存在测试临时库；端到端配置使用随机 JWT 密钥，不使用生产数据。

验收覆盖：错误密码、正确登录、Cookie 属性、页面恢复、并发刷新、多个标签页、退出撤销、非管理员 403、刷新 Cookie 失效及管理员页面守卫。剩余构建执行、生产备份、持久审计及账户管理见 [TODO](../TODO.md)。
