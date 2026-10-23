# 多人、多实例部署基础

应用采用模块化单体、多个无本地会话状态的 Go/Gin 副本。入口负载均衡 → 应用副本 → 共享 PostgreSQL + Redis。用户/会话/刷新历史保存在 PostgreSQL；Redis 只负责共享登录限流。任何副本都可处理登录后请求，无需 sticky session。

## 本地双副本样例

根目录 compose.yaml 提供 PostgreSQL 17、Redis 7.4、两个应用副本和 Nginx 轮询入口，入口只绑定 127.0.0.1:8080。它是联调样例，不是生产高可用模板。

PowerShell 设置 AEGIS_POSTGRES_PASSWORD、AEGIS_JWT_SECRET、AEGIS_BOOTSTRAP_USERNAME、AEGIS_BOOTSTRAP_PASSWORD 后运行：

```powershell
docker compose up --build -d
```

变量不要提交仓库。数据库密码在连接 URL 中需要 URL 编码；为简化样例，建议本地使用足够长的字母数字密码。首次初始化后去掉 bootstrap 配置并重建应用容器。前端仍通过 npm run dev 访问 localhost:5173，其 /api 代理到这个 8080 入口。

Nginx 仅信任固定地址 172.30.88.10/32，对传入 X-Forwarded-For 进行覆盖；如网段冲突，需同时调整 compose 网络、gateway 地址与 AEGIS_TRUSTED_PROXIES。网关不自动重试鉴权写请求，避免 refresh 在上游已提交后被再次执行。不能把 0.0.0.0/0 当作可信代理。

Docker 未安装时，可单独启动 PostgreSQL/Redis，按 backend/.env.example 设置共享 URL，再以不同 AEGIS_HTTP_ADDR 启动多个 Go 进程。

## 生产约束

- 前端和 /api 使用同一 HTTPS 入口；生产显式 AEGIS_COOKIE_SECURE=true，保留 Cookie/Origin/Set-Cookie，禁止缓存鉴权响应。
- 所有应用实例使用一致的 JWT 密钥、来源白名单、Cookie 与令牌期限配置、Redis namespace。JWT 密钥轮换需另行设计，不混用旧新密钥副本。
- PostgreSQL 与 Redis 均使用网络隔离、认证和 TLS。Compose 的 sslmode=disable、无 Redis 密码仅适用于隔离本地联调。
- PostgreSQL 是用户、密码哈希、会话的权威来源；需要独立备份、恢复演练与高可用方案。Redis 中断会阻止新登录并使 readyz 返回503，不退回各副本自己的限流。
- 连接预算 = 应用副本数 × AEGIS_DB_MAX_OPEN，须为迁移、管理和其他服务预留空间。应用不持有本地数据库文件。
- 迁移和 bootstrap 使用数据库事务 advisory 锁，版本迁移账本检查未知新版本。滚动升级仍要求 schema 变更遵循先扩展后收缩；当前机制不自动保证任意版本混跑安全。
- PostgreSQL/Redis/Nginx 在示例中各只有一个副本；扩容应用不等于消除依赖单点。生产需按可用性目标配置托管服务、主备或集群及告警。
- 日志、审计持久化、业务对象级权限、账号禁用、项目成员和任务 worker 仍按 TODO 推进；多人使用不等于已支持多租户隔离。

## 测试

后端真实集成：设置 AEGIS_TEST_DATABASE_URL 与 AEGIS_TEST_REDIS_URL 后运行 go test ./...，测试使用随机隔离 schema 和 Redis namespace。测试数据库角色需要 CREATE SCHEMA 与 DROP SCHEMA 权限，不使用生产数据库。

浏览器 E2E：设置 AEGIS_E2E_DATABASE_URL 指向专用可丢弃数据库、AEGIS_E2E_REDIS_URL 指向测试 Redis，再 npm run test:e2e。E2E 会初始化管理员和创建用户，不要指向生产；8080/5173 须空闲。多实例集成测试额外启动两个真实 httptest HTTP server、独立数据库池及 Redis clients，验证会话互通、刷新/退出撤销及共享限额。

SQLite 开发库不自动迁移。若有重要数据先备份，另行执行经审核的离线导入；上线不依赖保留开发会话。
