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

## TLS 终止与反向代理

生产必须让 SPA 与 `/api/v1` 共用同一个 HTTPS 入口（同源），否则 `SameSite=Strict` 的 refresh Cookie、Origin 白名单与 CORS 凭据约定都会失效。TLS 在入口代理终止，应用副本之间保持内网明文；见 [nginx-tls.example.conf](nginx-tls.example.conf)，它在本地样例 [nginx.conf](nginx.conf) 的基础上补了 HTTPS、HSTS 与 SSE 专用 location。

- **Cookie 与来源**：入口启用 HTTPS 后设置 `AEGIS_COOKIE_SECURE=true`。该值是显式配置，不会从 `X-Forwarded-Proto` 推断，代理也无法用转发头改变它。
- **可信代理**：`AEGIS_TRUSTED_PROXIES` 只写入口代理的精确地址/CIDR（样例为 `172.30.88.10/32`）。应用只信任来自这些地址的 `X-Forwarded-For`，代理侧必须覆盖而不是追加该头（样例 `proxy_set_header X-Forwarded-For $remote_addr`）。把它写成 `0.0.0.0/0` 等于让任意客户端伪造登录限流来源。
- **SSE**：`/api/v1/builds/{id}/events` 需要关闭代理缓冲，并把上游读超时设到 15s 心跳之上、10 分钟服务端上限之内（样例用 11m）。普通路径保持 35s 读超时。
- **不重试鉴权写**：`proxy_next_upstream off` 必须保留。refresh 在上游已提交后如果被代理重放到另一个副本，会被判定为重放并撤销会话。
- **请求体**：API 的 JSON 处理器限制 16 KiB，入口 `client_max_body_size 16k` 与之一致，超限在代理层直接拒绝。
- **证书**：可用 ACME webroot（样例保留了 `/.well-known/acme-challenge/`），续期后 `nginx -s reload`。证书私钥与 `AEGIS_JWT_SECRET` 一样属于需要备份的机密。
- **数据库与 Redis**：样例 compose 中 `sslmode=disable` 且 Redis 无密码，仅适用于隔离本地网络。生产必须启用 PostgreSQL TLS、Redis 认证/TLS 与网络隔离；应用侧只改连接 URL，不改代码。

## 生产约束

- 前端和 /api 使用同一 HTTPS 入口；生产显式 AEGIS_COOKIE_SECURE=true，保留 Cookie/Origin/Set-Cookie，禁止缓存鉴权响应。
- 所有应用实例使用一致的 JWT 密钥、来源白名单、Cookie 与令牌期限配置、Redis namespace。JWT 密钥轮换需另行设计，不混用旧新密钥副本；密钥丢失会使全部会话失效（用户重新登录即可），因此必须与数据库备份分开保存。
- PostgreSQL 与 Redis 均使用网络隔离、认证和 TLS。Compose 的 sslmode=disable、无 Redis 密码仅适用于隔离本地联调。
- PostgreSQL 是用户、密码哈希、会话的权威来源；需要独立备份、恢复演练与高可用方案，见下文「备份与恢复」。Redis 中断会阻止新登录并使 readyz 返回503，不退回各副本自己的限流。
- 连接预算 = 应用副本数 × AEGIS_DB_MAX_OPEN，须为迁移、管理和其他服务预留空间。应用不持有本地数据库文件。
- 迁移和 bootstrap 使用数据库事务 advisory 锁，版本迁移账本检查未知新版本。滚动升级要求 schema 变更遵循先扩展后收缩，见下文「滚动升级」。
- PostgreSQL/Redis/Nginx 在示例中各只有一个副本；扩容应用不等于消除依赖单点。生产需按可用性目标配置托管服务、主备或集群及告警。
- 构建 worker 由 PostgreSQL 会话锁保证每个部署只有一个副本执行与恢复（不提供并行吞吐）；停止持锁副本时，其正在运行的构建会被标记为 `interrupted`，由下一个持锁副本接管恢复。
- 多人使用不等于已支持多租户隔离；其余业务对象级权限按 TODO 推进。

## 备份与恢复

### 需要备份的内容

| 对象 | 说明 | 丢失的后果 |
| --- | --- | --- |
| PostgreSQL 数据 | 用户、密码哈希、会话/refresh 哈希、项目、构建与日志、审计 | 无法恢复账号与历史 |
| `AEGIS_JWT_SECRET` | 签发 access/refresh 的密钥 | 全部会话失效，用户需重新登录（数据不丢） |
| 入口 TLS 私钥 | 证书私钥 | 需重新签发证书 |
| 部署配置 | 环境变量清单（不含明文密码）、编排文件 | 灾难恢复时需要重建 |

Redis **不需要备份**：它只保存登录限流计数与 namespace，丢失只会重置配额；重启后 readyz 短暂返回 503、新登录被拒，连接恢复即恢复。不要把 Redis 当作会话或业务存储。

### PostgreSQL 逻辑备份

```bash
# 自定义格式：压缩且可按对象恢复；--no-owner/--no-privileges 便于恢复到不同角色。
pg_dump --format=custom --compress=9 --no-owner --no-privileges \
  --file="aegisflow-$(date -u +%Y%m%dT%H%M%SZ).dump" "$AEGIS_DATABASE_URL"
```

- 频率与保留按 RPO 决定：常见起点是每日全量 + 保留 30 天，重要操作前额外做一次。
- 备份要加密并异地存放；备份内含密码哈希、refresh 哈希与审计记录，泄漏等价于数据库泄漏。
- 建议用只读账号或独立连接做备份，避免与迁移争用 advisory 锁。

### 恢复与演练

```bash
createdb "$RESTORE_URL"
pg_restore --no-owner --no-privileges --dbname="$RESTORE_URL" aegisflow-20260101T000000Z.dump

# 1) 迁移账本必须完整，且不高于目标版本应用支持的版本。
psql "$RESTORE_URL" -c "SELECT max(version) FROM schema_migrations"
# 2) 关键表行数与最近审计时间。
psql "$RESTORE_URL" -c "SELECT count(*) FROM auth_users"
psql "$RESTORE_URL" -c "SELECT max(created_at) FROM auth_audit"
```

把恢复出的库指向一个**临时**应用实例（不同端口、独立 Redis namespace），等 `/readyz` 返回 200、能登录并读到项目后，再按变更窗口切换生产连接。不要在副本仍在运行时直接覆盖生产库：`pg_restore --clean` 会与在线连接和迁移锁冲突。

- 至少每季度做一次恢复演练并记录实测 RTO/RPO；演练在隔离环境进行，不接触生产凭据。
- 需要秒级 RPO 时使用托管服务的 PITR 或 PostgreSQL WAL 归档；应用本身不实现时间点恢复。
- 应用层审计保留只影响在线数据；备份中更早的审计记录会随备份保留周期存在，两者策略需要分别说明。

## 滚动升级

应用启动时自动执行版本化迁移，并用 advisory 事务锁保证多副本串行；发现数据库版本高于代码支持范围时**拒绝启动**。因此升级的基本约束是「schema 先扩展、代码后使用、旧列最后删除」。

1. **升级前**：完成一次备份并确认可恢复；记录当前 `SELECT max(version) FROM schema_migrations`；确认后端与前端来自同一发布版本。
2. **逐副本替换**：一次下线一个副本，等新副本 `/readyz` 返回 200 再处理下一个。入口应在此期间把该副本摘除；未持 worker 租约的副本可以安全替换。
3. **构建 worker**：停止持锁副本会把正在运行的构建标记为 `interrupted`（不是业务失败，可重试）。升级窗口尽量避开构建高峰，或提前清空排队构建。
4. **前端**：静态资源与 API 在同一入口后，先发布后端再发布前端；前端只调用同源 `/api/v1`。
5. **验证**：`/readyz` 200；管理员登录；项目列表与构建详情可读；触发一次构建并看到日志；审计页出现对应 `build.trigger` 记录。
6. **回滚**：仅当本次迁移是加法（只新增表/列/索引）时，允许把代码回退到上一版本。若迁移删改过列，必须先停掉全部副本，用升级前备份恢复到新库，再把副本指向该库——迁移账本没有 down migration，不要指望回退脚本。

**schema 变更约定**：新增列必须带默认值或允许 NULL；新代码兼容旧数据；删除/重命名放在后续版本，并至少跨一个发布周期。

## 监控与健康检查

- `/healthz` 是进程存活（不查依赖），供编排系统做 liveness；`/readyz` 检查 PostgreSQL 与 Redis（2s 超时），依赖不可用时返回 503，供 readiness 与入口摘流。
- 日志为结构化 JSON，带 `requestId`、method、path、status、duration；排查时用 `X-Request-ID` 贯穿代理与应用日志。日志与审计都不含密码、令牌或 Cookie 明文。
- 告警至少覆盖：`/readyz` 连续非 200、PostgreSQL 连接数接近上限（副本数 × `AEGIS_DB_MAX_OPEN`）、Redis 不可用、构建队列持续堆积。

## 测试

后端真实集成：设置 AEGIS_TEST_DATABASE_URL 与 AEGIS_TEST_REDIS_URL 后运行 go test ./...，测试使用随机隔离 schema 和 Redis namespace。测试数据库角色需要 CREATE SCHEMA 与 DROP SCHEMA 权限，不使用生产数据库。

浏览器 E2E：设置 AEGIS_E2E_DATABASE_URL 指向专用可丢弃数据库、AEGIS_E2E_REDIS_URL 指向测试 Redis，再 npm run test:e2e。E2E 会初始化管理员和创建用户，不要指向生产；8080/5173 须空闲。多实例集成测试额外启动两个真实 httptest HTTP server、独立数据库池及 Redis clients，验证会话互通、刷新/退出撤销及共享限额。

SQLite 开发库不自动迁移。若有重要数据先备份，另行执行经审核的离线导入；上线不依赖保留开发会话。
