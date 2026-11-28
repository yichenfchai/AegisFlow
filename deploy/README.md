# 多人、多实例部署基础

应用采用模块化单体、多个无本地会话状态的 Go/Gin 副本。入口负载均衡 → 应用副本 → 共享 PostgreSQL + Redis。用户/会话/刷新历史保存在 PostgreSQL；Redis 只负责共享登录限流。任何副本都可处理登录后请求，无需 sticky session。

## 本地双副本样例

根目录 compose.yaml 提供 PostgreSQL 17、Redis 7.4、两个应用副本和 Nginx 轮询入口，入口只绑定 127.0.0.1:8080。它是联调样例，不是生产高可用模板。

官方镜像 [backend/Dockerfile](../backend/Dockerfile) 的运行阶段包含 `git`（仓库 clone）与 `docker-cli`（container 构建沙箱），以非 root 用户 `aegis` 运行，默认监听容器内 `0.0.0.0:8080`（宿主暴露仍由 compose 网络与 gateway 端口决定），并内置 `wget /healthz` 健康检查。compose 给两个 api 副本配置了 healthcheck，gateway 等 `service_healthy` 后才启动。

PowerShell 设置 AEGIS_POSTGRES_PASSWORD、AEGIS_JWT_SECRET、AEGIS_BOOTSTRAP_USERNAME、AEGIS_BOOTSTRAP_PASSWORD 后，在仓库根目录运行：

```powershell
docker compose up --build -d
```

变量不要提交仓库。数据库密码在连接 URL 中需要 URL 编码；为简化样例，建议本地使用足够长的字母数字密码。首次初始化后去掉 bootstrap 配置并重建应用容器。前端仍通过 npm run dev 访问 localhost:5173，其 /api 代理到这个 8080 入口。

compose 还透传了部署目标凭据密钥（`AEGIS_SECRET_KEY`、`AEGIS_SECRET_KEY_PREVIOUS`，base64 恰好 32 字节，留空表示禁用凭据存储；生成用 `openssl rand -base64 32`）、审批环境（默认 production）、构建相关开关（`AEGIS_BUILD_*`，沙箱默认 none）与独立监听地址 `AEGIS_METRICS_ADDR`（留空关闭）。要在本地验收 P4 的凭据加密与 rekey，至少设置 `AEGIS_SECRET_KEY`；变量含义见 [后端文档](../backend/README.md)。container 构建沙箱需要额外挂载宿主 Docker socket，compose 末尾以注释给出可选覆盖片段，安全边界见下文「构建沙箱部署」，默认不启用。

Nginx 仅信任固定地址 172.30.88.10/32，对传入 X-Forwarded-For 进行覆盖；如网段冲突，需同时调整 compose 网络、gateway 地址与 AEGIS_TRUSTED_PROXIES。网关不自动重试鉴权写请求，避免 refresh 在上游已提交后被再次执行。不能把 0.0.0.0/0 当作可信代理。本地样例的 gateway 配置同样带构建日志 SSE 专用 location（关闭缓冲、11 分钟读超时），与 TLS 样例一致。

Docker 未安装时，可单独启动 PostgreSQL/Redis，按 backend/.env.example 设置共享 URL，再以不同 AEGIS_HTTP_ADDR 启动多个 Go 进程。

## TLS 终止与反向代理

生产必须让 SPA 与 `/api/v1` 共用同一个 HTTPS 入口（同源），否则 `SameSite=Strict` 的 refresh Cookie、Origin 白名单与 CORS 凭据约定都会失效。TLS 在入口代理终止，应用副本之间保持内网明文；见 [nginx-tls.example.conf](nginx-tls.example.conf)，它在本地样例 [nginx.conf](nginx.conf) 的基础上补了 HTTPS、HSTS、SSE 专用 location、`/healthz`/`/readyz` 代理与 SPA 静态托管。

- **Cookie 与来源**：入口启用 HTTPS 后设置 `AEGIS_COOKIE_SECURE=true`。该值是显式配置，不会从 `X-Forwarded-Proto` 推断，代理也无法用转发头改变它。
- **可信代理**：`AEGIS_TRUSTED_PROXIES` 只写入口代理的精确地址/CIDR（样例为 `172.30.88.10/32`）。应用只信任来自这些地址的 `X-Forwarded-For`，代理侧必须覆盖而不是追加该头（样例 `proxy_set_header X-Forwarded-For $remote_addr`）。把它写成 `0.0.0.0/0` 等于让任意客户端伪造登录限流来源。
- **SSE**：`/api/v1/builds/{id}/events` 需要关闭代理缓冲，并把上游读超时设到 15s 心跳之上、10 分钟服务端上限之内（样例用 11m）。普通路径保持 35s 读超时。两个样例都使用该专用 location，且必须排在通用 `/` 或 `/api/` 代理规则之前，避免被普通 location 吞掉。
- **不重试鉴权写**：`proxy_next_upstream off` 必须保留。refresh 在上游已提交后如果被代理重放到另一个副本，会被判定为重放并撤销会话。
- **请求体**：API 的 JSON 处理器限制 16 KiB，入口 `client_max_body_size 16k` 与之一致，超限在代理层直接拒绝。
- **证书**：可用 ACME webroot（样例保留了 `/.well-known/acme-challenge/`），续期后 `nginx -s reload`。证书私钥与 `AEGIS_JWT_SECRET` 一样属于需要备份的机密。
- **数据库与 Redis**：样例 compose 中 `sslmode=disable` 且 Redis 无密码，仅适用于隔离本地网络。生产必须启用 PostgreSQL TLS、Redis 认证/TLS 与网络隔离；应用侧只改连接 URL，不改代码。

## 前端静态资源

前端是 Vite 构建的 SPA，生产由 TLS 入口同源托管（[nginx-tls.example.conf](nginx-tls.example.conf) 的 `root /usr/share/nginx/html` 与 `try_files ... /index.html`）；本地开发继续用 `npm run dev`（Vite 代理 `/api` 到 8080 入口），开发服务器不要对外提供服务。

在仓库根目录构建并把产物同步到入口机的静态目录：

```bash
npm ci
npm run build
# dist/ 是唯一需要发布的目录；按入口机实际路径调整目标目录：
rsync -a --delete dist/ /srv/aegisflow/www/
# 若 nginx 以容器运行，把该目录挂到 /usr/share/nginx/html，然后：
nginx -t && nginx -s reload
```

- 发布顺序：先发布后端再发布前端，或先同步静态文件再 reload；API 与前端各自可独立回滚。回滚前端只需把上一版 `dist/` 同步回去并 reload，不需要重启 API。
- 部署后把浏览器看到的入口源（如 `https://aegis.example.com`）加入 `AEGIS_ALLOWED_ORIGINS`，并设置 `AEGIS_COOKIE_SECURE=true`；入口源必须与 SPA/API 实际访问地址一致。
- 缓存策略：`index.html` 不缓存（样例 `expires -1`），`assets/` 下带内容 hash 的文件缓存一年（样例 `expires 1y`）；不要给 `/api/` 响应加缓存，refresh Cookie 与鉴权响应必须 `no-store`（应用自身已设置）。
- `nginx-tls.example.conf` 中 `/api/` 代理、SSE location 与 `/healthz`、`/readyz` 代理都排在 SPA fallback 之前；如果改动顺序，注意不要让 `try_files` 把 API 与健康检查请求返回成 `index.html`（监控会把 200 的 HTML 当成健康）。
- 入口静态根目录只放构建产物，不要把仓库源码、`.env` 或 source map 同步过去；确需保留 source map 时单独存放，不对外公开。
- 若静态资源改由 CDN/独立静态站点提供，仍必须与 API 同源或处于同一站点（见上文同源要求），否则 refresh Cookie 与 Origin 校验会失效。

## 构建沙箱部署

生产推荐 `AEGIS_BUILD_SANDBOX=container` + 宿主 Docker socket（单机或受控内网自托管）。官方镜像已包含 `git` 与 `docker-cli`，镜像内仍以非 root 用户 `aegis` 运行；compose 末尾的注释片段给出启用方式（挂载 `/var/run/docker.sock`、设置 `AEGIS_BUILD_SANDBOX=container` 和 `group_add`），默认不启用。

- **socket 的安全边界**：把 `/var/run/docker.sock` 挂进应用容器等价于把宿主 Docker daemon 的控制权交给应用容器——可以启动特权容器、挂载宿主目录，实际上接近宿主 root。只允许受信任管理员部署，不要用于多租户或不可信构建来源；构建命令来自项目配置，本身即可执行任意代码。可选的加固：用只读 Docker socket 代理限制可调用的 API、把构建镜像固定为内部仓库、对宿主 daemon 做审计。
- **socket 组权限**：socket 属主组在宿主上通常是 `docker`（Debian/Ubuntu）或 `root`，GID 因主机而异，不能写死在镜像里。用 `stat -c '%g' /var/run/docker.sock` 取得数字 GID，在 compose 里设置 `group_add: ['<GID>']`，或 `docker run --group-add "$(stat -c '%g' /var/run/docker.sock)"`。GID 变化（重装 Docker、换宿主）后要同步更新。
- **fail-closed**：配置 `AEGIS_BUILD_SANDBOX=container` 后，启动时会探测 docker CLI 与 daemon；缺失 CLI、socket 未挂载/权限不足或 daemon 不可达都会拒绝启动，不会静默回落到 `none`。构建镜像默认 `alpine:3.20`（`AEGIS_BUILD_IMAGE`），首次使用由宿主 daemon 拉取，离线环境要提前 `docker pull`。
- **镜像与资源上限**：每次构建是一个一次性容器：`--network none`、`--read-only`、`--cap-drop ALL`、`no-new-privileges`，只挂载构建目录，CPU/内存/进程数由 `AEGIS_BUILD_CPU`、`AEGIS_BUILD_MEMORY`、`AEGIS_BUILD_PIDS` 限制；超时或取消会显式 kill 容器。
- **clone**：`AEGIS_BUILD_CLONE=true` 时由 API 进程（运行在应用容器内）用 `git clone --depth 1` 浅克隆，构建命令在 clone 出的仓库目录执行；构建容器本身无网络。只支持无凭据的 https 地址，含内联凭据的 URL 直接失败。`git@` SSH 地址还需要 ssh 客户端与密钥/known_hosts，官方镜像未附带 SSH 客户端，也不提供私有仓库凭据；本批边界见 [后端文档](../backend/README.md)。
- **user 模式**：仅 Linux 裸机且服务以 root 启动时可用，通过 `setpriv` 把 clone 与构建命令降权到 `AEGIS_BUILD_USER` 指定的专用账号；官方镜像是非 root 运行，不满足该模式。需要时单独构建以 root 启动的镜像，并保证该账号存在、`setpriv` 可用。
- **none 仅开发**：以服务账号执行构建命令，启动时打印警告，不代表任何隔离。仅用于本地联调；生产不要使用默认值。
- **后续选项（未实现）**：DinD（在独立容器内运行 dockerd，把构建流量与宿主 daemon 隔离）与专用 runner（把构建执行拆到独立服务/主机，通过队列或 API 通信）作为后续演进方向，当前版本只提供上述宿主 socket 方案，不包含这两种形态的实现或部署样例。

## 生产约束

- 前端和 /api 使用同一 HTTPS 入口；生产显式 AEGIS_COOKIE_SECURE=true，保留 Cookie/Origin/Set-Cookie，禁止缓存鉴权响应。
- 官方镜像以非 root 用户 `aegis` 运行，容器内监听 `0.0.0.0:8080` 以便网关访问；只通过受控内部网络和入口代理暴露 API 端口，不要把 8080 直接发布到公网，也不要因为容器内监听通配地址就放宽 `AEGIS_TRUSTED_PROXIES`。
- 所有应用实例使用一致的 JWT 密钥、来源白名单、Cookie 与令牌期限配置、Redis namespace。JWT 密钥轮换需另行设计，不混用旧新密钥副本；密钥丢失会使全部会话失效（用户重新登录即可），因此必须与数据库备份分开保存。
- PostgreSQL 与 Redis 均使用网络隔离、认证和 TLS。Compose 的 sslmode=disable、无 Redis 密码仅适用于隔离本地联调。
- PostgreSQL 是用户、密码哈希、会话的权威来源；需要独立备份、恢复演练与高可用方案，见下文「备份与恢复」。Redis 中断会阻止新登录并使 readyz 返回503，不退回各副本自己的限流。
- 连接预算 = 应用副本数 × AEGIS_DB_MAX_OPEN，须为迁移、管理和其他服务预留空间。应用不持有本地数据库文件。
- 迁移和 bootstrap 使用数据库事务 advisory 锁，版本迁移账本检查未知新版本。滚动升级要求 schema 变更遵循先扩展后收缩，见下文「滚动升级」。
- PostgreSQL/Redis/Nginx 在示例中各只有一个副本；扩容应用不等于消除依赖单点。生产需按可用性目标配置托管服务、主备或集群及告警。
- 构建 worker 由 PostgreSQL 会话锁保证每个部署只有一个副本执行与恢复（不提供并行吞吐）；停止持锁副本时，其正在运行的构建会被标记为 `interrupted`，由下一个持锁副本接管恢复。
- 审批 Webhook 通知（可选）使用 PostgreSQL outbox 与独立 notifier；生产必须使用 HTTPS（仅回环测试允许 HTTP），配置 `AEGIS_APPROVAL_WEBHOOK_SECRET` 时接收方应校验 `X-AegisFlow-Signature`，并按 `X-AegisFlow-Delivery` 幂等处理。投递失败不会改变审批状态，失败行保留在 `approval_notifications` 供排查。同一通道自 P5-A E4 起同时投递 `review.*` 审查事件（`review_notifications` outbox，签名/重试语义相同），接收方需同时处理 `approval.*` 与 `review.*`。
- 审查外部结果回写（可选，P5-A E4）：设置 `AEGIS_REVIEW_WEBHOOK_SECRET`（16–256 字节）后启用 `POST /api/v1/reviews/{id}/external-results`，第三方工具以 `X-AegisFlow-Timestamp`（±300s）、`X-AegisFlow-Signature`（HMAC-SHA256）与 `X-AegisFlow-Delivery`（幂等键）回写只读结果；未设置时该接口返回 503，读取接口仍可用。外部结果不改变审查状态或部署门禁。密钥未设置/非法不会影响出站审批与审查通知通道。
- 扫描结果与防御事件回写（可选，P5 剩余模块批次 A）：设置 `AEGIS_INGEST_WEBHOOK_SECRET`（16–256 字节，与审查回写密钥独立）后启用 `POST /api/v1/projects/{id}/scans` 与 `POST /api/v1/security-events`，外部扫描/检测工具使用相同的 `X-AegisFlow-Timestamp`/`X-AegisFlow-Signature`/`X-AegisFlow-Delivery` 约定回写（delivery 幂等，`Content-Type: application/json`）；未设置时两个入站接口返回 503，扫描读取接口与防御事件 admin 读取/确认接口仍可用。平台不自研扫描或检测引擎，扫描结果按项目可见性只读展示，防御事件仅管理员可确认（同事务写审计）。
- 部署审查门禁（可选，P5-A E2）：`AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS` 未设置或为空（默认）时不启用，部署创建/回滚行为与之前完全一致。用逗号分隔填写 `development`/`staging`/`production` 后，这些环境的部署创建与回滚必须引用**同一项目**已批准审查的 `reviewId`；缺失、跨项目或未批准统一返回 400 `validation_failed`，请求不会进入审批队列。审查门禁与 `AEGIS_DEPLOY_APPROVAL_ENVIRONMENTS` **叠加**：先校验审查，再走审批状态机。部署行会快照 `deployments.review_id`（schema v12，可空），审计 detail 追加 `review=<id>`。启用前先确认全部副本都升级到包含 v13 的版本：迁移账本不允许数据库版本高于代码支持范围，因此**升级后不能直接回退到旧二进制**，回退需同时恢复数据库备份（见「滚动升级」）。
- 项目自动化策略（P5 剩余模块批次 C，schema v16）：策略是**数据库配置**，不新增环境变量；在项目「策略」页由全局 admin 或项目 maintainer 写入 `requireReview`/`requiredScanVerdict`。策略叠加在 `AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS` 之上，只会加严：`requireReview=true` 时任何目标环境都要求同项目已批准审查，`requiredScanVerdict=passed` 时项目最新一条扫描必须存在且结论为 passed；无策略行 = 默认关闭，既有部署行为不变。创建与回滚走同一套门禁（审查 → 最新扫描 → 审批），命中策略时审计 detail 追加 `policy=<projectId>`。与既有门禁一样，迁移账本不允许数据库版本高于代码支持范围：升级到包含 v16 的版本后**不能直接回退到旧二进制**，回退需同时恢复数据库备份（见「滚动升级」）。审计 CSV 导出（admin-only）只读 `auth_audit`，不改变任何部署状态，也不存在额外的数据面依赖。
- 多租户（阶段 F1–F2）：同一部署内的租户共享 PostgreSQL/Redis 与 worker，隔离由 `tenant_id` 与查询谓词保证（业务数据、审计、凭据互不可见，跨租户一律 404）；租户创建/列出/禁用只通过 `backend/cmd/tenant`（复用 `AEGIS_DATABASE_URL`），没有 HTTP 开通入口；升级到 schema v19 后不能直接回退旧二进制，回退需同时恢复数据库备份。尚无子域/路径路由、租户切换、平台控制台与每租户配额。其余业务对象级权限按 TODO 推进。
- 部署目标 endpoint 由 admin 配置，admin 是可信配置者，目标 endpoint 属于受信配置面：允许私网/回环（自托管内网场景），拒绝 link-local 与云元数据地址（169.254.0.0/16、fe80::/10、100.100.100.200、fd00:ec2::254）；创建/更新返回 400，运行时在连接建立前按解析后的 IP 再校验一次（防 DNS rebinding）。

## 备份与恢复

### 需要备份的内容

| 对象 | 说明 | 丢失的后果 |
| --- | --- | --- |
| PostgreSQL 数据 | 用户、密码哈希、会话/refresh 哈希、项目、构建与构建日志、部署目标（含密文 `secret_cipher`）、部署任务与部署事件、审批、审批通知 outbox、审查记录与部署的审查引用（`deployments.review_id`）、审查外部结果与审查通知 outbox、项目自动化策略（`project_policies`）、审计 | 无法恢复账号、历史、审查记录与待处理审批；未投递的通知事件丢失（审批/审查状态本身在各自业务表）；项目策略丢失后该项目回到默认关闭门禁（部署不再被策略拦截） |
| `AEGIS_SECRET_KEY` | 部署目标凭据的 AES-256-GCM 当前密钥（base64 恰好 32 字节） | `deployment_targets.secret_cipher` 永久不可解密；受影响目标无法鉴权调用，只能重新录入凭据 |
| `AEGIS_SECRET_KEY_PREVIOUS` | 密钥轮换期间只用于解密旧密文的退役密钥 | 轮换未完成时旧密文同样不可解密，`rekey` 无法迁移；轮换完成并移除后可不备份 |
| `AEGIS_JWT_SECRET` | 签发 access/refresh 的密钥 | 全部会话失效，用户需重新登录（数据不丢） |
| 入口 TLS 私钥 | 证书私钥 | 需重新签发证书 |
| 部署配置 | 环境变量清单（只写密钥存放位置，不含明文）、编排文件、外部依赖连接信息 | 灾难恢复时需要重建 |

`AEGIS_SECRET_KEY`（及轮换期的 `AEGIS_SECRET_KEY_PREVIOUS`）必须与数据库备份**分开**保存：只有数据库备份而没有密钥，部署目标凭据永久不可解密；两者同时泄漏则等价于凭据泄漏。备份里同时包含 `deployment_targets`、`deployments`、`deployment_events`、`approvals`、`approval_notifications`、`reviews`、`review_external_results`、`review_notifications`、`project_policies` 等部署治理数据，恢复时整库恢复，不要单独重建这些表。

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
# 3) 部署治理数据恢复完整性。
psql "$RESTORE_URL" -c "SELECT count(*) FROM deployment_targets WHERE secret_cipher IS NOT NULL"
psql "$RESTORE_URL" -c "SELECT count(*) FROM approvals WHERE status = 'pending'"
psql "$RESTORE_URL" -c "SELECT count(*) FROM approval_notifications WHERE status <> 'delivered'"
psql "$RESTORE_URL" -c "SELECT count(*) FROM reviews"
psql "$RESTORE_URL" -c "SELECT count(*) FROM deployments WHERE review_id IS NOT NULL"
psql "$RESTORE_URL" -c "SELECT count(*) FROM review_external_results"
psql "$RESTORE_URL" -c "SELECT count(*) FROM review_notifications WHERE status <> 'delivered'"

# 4) 凭据可解密性：rekey 默认只读 dry run，不会改写恢复库。
#    必须在 backend 模块目录执行（仓库根目录没有 go.mod）。
cd backend
AEGIS_DATABASE_URL="$RESTORE_URL" \
AEGIS_SECRET_KEY=<备份记录中的当前钥> \
AEGIS_SECRET_KEY_PREVIOUS=<轮换期旧钥> \
go run ./cmd/rekey
```

第 4 步输出 `dry run: scanned=N already_current=... reencrypted=... failed=0` 表示所有 `deployment_targets.secret_cipher` 都能用备份的密钥解开；`failed>0` 说明恢复出的密文与提供的密钥不匹配，先核对备份记录，**不要删除旧钥**。恢复演练不需要 `-apply`（那会改变恢复库的校验基线）。`rekey` 要求同时提供当前钥与退役钥；从未轮换过的环境没有退役钥，此时改为在临时实例登录管理员，对带凭据的部署目标执行一次「检查」——探测会在内存中解密凭据并发请求，成功即证明密文可解。

把恢复出的库指向一个**临时**应用实例（不同端口、独立 Redis namespace），等 `/readyz` 返回 200、能登录并读到项目后，再按变更窗口切换生产连接。不要在副本仍在运行时直接覆盖生产库：`pg_restore --clean` 会与在线连接和迁移锁冲突。

- 至少每季度做一次恢复演练并记录实测 RTO/RPO；演练在隔离环境进行，不接触生产凭据。
- 需要秒级 RPO 时使用托管服务的 PITR 或 PostgreSQL WAL 归档；应用本身不实现时间点恢复。
- 应用层审计保留只影响在线数据；备份中更早的审计记录会随备份保留周期存在，两者策略需要分别说明。

## 部署凭据密钥轮换

目标凭据用 `AEGIS_SECRET_KEY` 密封，轮换分两步：先让新钥接管写入、旧钥保留读取，再用维护命令把存量密文改写为新钥。

1. 生成新钥（`openssl rand -base64 32`），把新钥写入 `AEGIS_SECRET_KEY`，把原来的当前钥移到 `AEGIS_SECRET_KEY_PREVIOUS`，滚动重启所有副本。此阶段读路径先试新钥、失败回退旧钥，写入只用新钥，服务不中断。
2. dry run 估算（默认不写库）。命令必须在 `backend` 模块目录执行，仓库根目录没有 `go.mod`：

   ```bash
   cd backend
   AEGIS_DATABASE_URL=<连接 URL> \
   AEGIS_SECRET_KEY=<新钥> \
   AEGIS_SECRET_KEY_PREVIOUS=<旧钥> \
   go run ./cmd/rekey
   ```

   确认 `reencrypted` 与预期一致且 `failed=0`。
3. 加 `-apply` 重跑，逐行重加密：`go run ./cmd/rekey -apply`。
4. 再跑一次 dry run：`reencrypted=0` 且 `already_current` 等于存储凭据的目标数后，删除 `AEGIS_SECRET_KEY_PREVIOUS`，再滚动重启一次。

以上为 bash 语法；PowerShell 依次设置 `$env:AEGIS_DATABASE_URL`、`$env:AEGIS_SECRET_KEY`、`$env:AEGIS_SECRET_KEY_PREVIOUS` 后执行同一 `go run` 命令即可。

`failed>0` 表示某些行的密文两把钥都打不开：命令不会改动这些行，先核对配置与历史数据，**不要删除旧钥**。重加密只改 `deployment_targets.secret_cipher`，不改 `updated_at`、不写审计；命令可以在服务运行期间执行（此时服务已同时持有两把钥）。

## 滚动升级

应用启动时自动执行版本化迁移，并用 advisory 事务锁保证多副本串行；发现数据库版本高于代码支持范围时**拒绝启动**。因此升级的基本约束是「schema 先扩展、代码后使用、旧列最后删除」。

1. **升级前**：完成一次备份并确认可恢复；记录当前 `SELECT max(version) FROM schema_migrations`；确认后端与前端来自同一发布版本。
2. **逐副本替换**：一次下线一个副本，等新副本 `/readyz` 返回 200 再处理下一个。入口应在此期间把该副本摘除；未持 worker 租约的副本可以安全替换。
3. **构建 worker**：停止持锁副本会把正在运行的构建标记为 `interrupted`（不是业务失败，可重试）。升级窗口尽量避开构建高峰，或提前清空排队构建。
4. **前端**：静态资源与 API 在同一入口后，先发布后端再发布前端；前端只调用同源 `/api/v1`。同步 `dist/` 后 `nginx -s reload`，见「前端静态资源」。
5. **验证**：`/readyz` 200；管理员登录；项目列表与构建详情可读；触发一次构建并看到日志；审计页出现对应 `build.trigger` 记录。
6. **回滚**：仅当本次迁移是加法（只新增表/列/索引）时，允许把代码回退到上一版本。若迁移删改过列，必须先停掉全部副本，用升级前备份恢复到新库，再把副本指向该库——迁移账本没有 down migration，不要指望回退脚本。

**网关 upstream 与静态地址**：样例 Nginx 的 `upstream` 是写死副本地址的静态列表，启动时只解析一次；副本重建后容器 IP 变化时，旧地址会继续被使用并返回 502/超时。因此每次替换副本后执行 `nginx -t && nginx -s reload`（reload 平滑），或改用 DNS `resolver` + 动态 upstream、服务发现或主动健康检查。compose 的 gateway `depends_on: service_healthy` 只覆盖首次启动，不覆盖运行期重建；滚动发布需要编排系统的健康检查与网关重载配合。健康判断用 `/readyz`（依赖就绪，503 表示不可用），`/healthz` 只表示进程存活。`proxy_next_upstream off` 是有意的安全取舍：login/refresh/logout 等鉴权写请求不会自动重放到另一个副本，上游故障时宁可返回 5xx 让客户端重试，也不冒 refresh 已提交后被再次执行的风险。

**schema 变更约定**：新增列必须带默认值或允许 NULL；新代码兼容旧数据；删除/重命名放在后续版本，并至少跨一个发布周期。

## 生产化运维（升级/回滚/备份/租户级迁移）

本节把升级、回滚、备份恢复与租户迁移串成可执行的运维流程；脚本用法、参数与故障排查见 [scripts/ops/README.md](../scripts/ops/README.md)，脚本本身（`scripts/ops/backup-postgres.ps1`、`restore-postgres.ps1`、`export-tenant.ps1`、`import-tenant.ps1`、`verify-schema.ps1`）面向 Windows PowerShell 5.1 与 `pwsh`，且**不在 CI 中运行**（需要真实数据库与凭据）。多租户阶段的 schema 版本是 v19：从 v19 起迁移只向前、没有 down migration，回滚必须走"恢复升级前备份 + 旧二进制"。

### 升级前检查清单

- [ ] 已用当前版本完成一次全库备份，`.dump` 与 `.sha256` 均落盘、**离线/加密保存**，并记录 SHA-256 到变更单。
- [ ] 已记录当前 `SELECT max(version) FROM schema_migrations`（例如 19），并确认目标版本所需迁移清单来自同一发布线。
- [ ] 已确认磁盘空间：备份大小、恢复演练库、PostgreSQL 数据目录与 WAL 均有足够余量（恢复需要一份额外全库空间）。
- [ ] `AEGIS_SECRET_KEY`（轮换期还有 `AEGIS_SECRET_KEY_PREVIOUS`）、`AEGIS_JWT_SECRET`、TLS 私钥等机密已单独备份，且与数据库备份**分开存放**；轮换中的密钥不要提前删除。
- [ ] 已确认所有副本/worker 都能在窗口内停止与重启，构建/部署队列已排空或允许 `interrupted`，网关摘流与恢复步骤明确。
- [ ] 已安排变更窗口与回滚决策人，通知 tenant 管理员登录会短暂中断（滚动替换期间按副本摘流，正常无需全停）。
- [ ] 已确认部署的 `AEGIS_DEFAULT_TENANT_SLUG` 在数据库中仍存在（v18/v19 升级与租户导入后尤其重要），否则 server 会拒绝启动。

### 全库备份（升级前必须）

```powershell
$env:PGPASSWORD = Get-Content C:\secrets\pg.txt
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/backup-postgres.ps1 `
  -Database aegisflow -Host db.internal -Port 5432 -User aegis_backup `
  -OutputDir D:\backups -SslMode verify-full
```

备份完成后确认输出中的 schema/归档库名、表数、TOC 条目数、文件大小与 SHA-256，并把哈希写入演练记录。`pg_restore --list` 校验失败或退出码非 0 时**不要继续升级**。Linux 环境等价的 `pg_dump` 命令见上文「备份与恢复」；恢复演练与租户导出同样可在隔离主机执行。

### 升级步骤

1. **停写/摘流**：把副本从网关摘除并停止旧版本进程（滚动替换时逐副本进行；本仓库迁移采用"先扩展后收缩"，但本批不改变 schema，升级窗口内仍建议避免新写入的部署/审批操作）。确认没有持锁的构建 worker 正在写库。
2. **备份**：按上一节执行全库备份并校验归档；记录 `schema_migrations` 当前最大版本。
3. **起新版本**：用新二进制启动（应用启动时在事务 advisory 锁下自动执行迁移，遇到未知更高版本会拒绝启动）。等待 `/readyz` 返回 200；观察启动日志确认迁移版本已推进到目标版本。
4. **校验 schema**：

   ```powershell
   powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/verify-schema.ps1 `
     -Database aegisflow -Host db.internal -Port 5432 -User aegis_backup -ExpectedVersion 19
   ```

   要求全部 `[PASS]`：`schema_migrations` 连续、24 张表齐全、14 张业务表 `tenant_id` NOT NULL 且外键/唯一索引存在。
5. **冒烟**：`/healthz` 与 `/readyz` 200；管理员与一个普通租户用户（带 `tenantSlug`）登录成功；项目列表、构建详情、部署目标与审查/审批列表可读；触发一次构建并看到日志；设置页审计出现对应记录。多租户环境再抽查另一个租户数据不可见（跨租户 404）。
6. **恢复流量**：把副本按依赖顺序重新接入网关并恢复写入，观察错误率、`/readyz`、构建/部署 worker 日志。
7. **收尾**：确认构建/部署 worker 已领取租约、队列恢复；把备份哈希、迁移版本、冒烟结果、负责人写入演练记录。

### 回滚策略（v19 起不可 down migrate）

迁移账本没有 down migration，且 v13/v16/v19 之后旧二进制无法读取更高版本 schema（启动时拒绝）。因此：

- **回滚 = 恢复升级前备份 + 回退旧二进制**，不是执行反向 SQL。升级前备份是唯一可信回滚点，没有可用备份就不应在生产执行不可逆迁移。
- 回滚步骤：全部副本停写 → 新库恢复升级前 dump（`restore-postgres.ps1` 默认拒绝覆盖，建议恢复到新库）→ `verify-schema.ps1` 校验 → 用旧版本二进制指向恢复库启动 → `/readyz`+登录+关键列表冒烟 → 切流量。不要用 `pg_restore --clean` 直接覆盖在线库（会与在线连接/迁移锁冲突）。
- 恢复出的库同样需要 `AEGIS_SECRET_KEY`（及轮换期旧钥），先跑 `cmd/rekey` dry run 确认密文可解（见「备份与恢复」第 4 步），再切生产。
- 前端静态资源可独立回滚到上一版 `dist/` 并 `nginx -s reload`；API 与前端允许分开回退，但 schema 只能通过恢复备份回退。
- 若只是应用版本问题而 schema 未变（本批只加脚本/文档，不改 schema），可以只回退二进制；一旦迁移已应用，回退二进制前必须确认它兼容当前 schema 版本。

### 租户级备份/恢复（tenant 迁移）

单个租户迁移不搬整库、不迁移会话：用 `export-tenant.ps1` 导出该租户全部业务数据（含 `secrets` 密文与 `project_members`），在目标库用 `import-tenant.ps1` 单事务导入。

1. **准备**：目标库必须已由同发布线 server 启动并迁移到 **>= 导出包 schemaVersion**；记录源/目标 `max(version)` 与 `AEGIS_DEFAULT_TENANT_SLUG`。
2. **停写窗口**：让该租户暂停触发构建/部署/审批/审查等写入（或整体摘流），避免导出快照之后仍有新写入未被迁移；导出本身是单快照一致的，但切换时应以停写窗口保证"导出后无新增"。
3. **导出**：

   ```powershell
   powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/export-tenant.ps1 `
     -TenantSlug acme -Database aegisflow -Host db.internal -Port 5432 -User aegis_backup `
     -OutputDir D:\tenant-moves
   ```

   核对目录内 `manifest.json`：`schemaVersion`、租户 id/slug、逐表行数、总行数；导出包 README 说明密钥绑定，目录按机密数据管理（加密、访问控制、不入库）。
4. **导入**：

   ```powershell
   powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/import-tenant.ps1 `
     -TenantDir D:\tenant-moves\tenant-acme-20261128-231500 -Database aegisflow_new `
     -Host db-new.internal -Port 5432 -User aegis_backup
   ```

   导入前会校验 manifest、每个 CSV 的 SHA-256 与表头、目标 schema 版本；目标库已有同 slug/同 id 租户时默认拒绝，确认替换才用 `-Force`（同一事务内删除并重建该租户数据，失败整体回滚）。目标 schema 更高时需显式 `-AllowNewerSchema`，并先确认新增迁移是加法变更。
5. **校验**：`verify-schema.ps1` 全 PASS；逐表行数/关键数据与源库比对；用带 `tenantSlug` 的登录、项目/构建/部署/审查/审批列表与跨租户 404 做冒烟。**密钥要求**：目标部署必须配置与源部署相同的 `AEGIS_SECRET_KEY`（轮换期还需 `AEGIS_SECRET_KEY_PREVIOUS`），否则 `secrets` 与 `deployment_targets` 密文不可解密，只能重新录入；可用 `cmd/rekey` dry run 验证。
6. **切换与回退**：确认目标部署 `AEGIS_DEFAULT_TENANT_SLUG` 指向仍存在的租户后切流量；保留源库数据与导出包直到租户验收完成。若导入出错，事务已回滚，修复后重试；若目标租户已被部分使用，用 `-Force` 从同一导出包重建（先确认目标库数据可丢弃）。

### 密钥轮换注意

- 部署目标凭据（`deployment_targets.secret_cipher`）轮换分两步：先设 `AEGIS_SECRET_KEY`=新钥、`AEGIS_SECRET_KEY_PREVIOUS`=旧钥并滚动重启（读先新后旧、写入只用新钥），再 `cmd/rekey` dry run 确认后 `-apply` 重加密，最后确认 `reencrypted=0` 再移除旧钥。详见「部署凭据密钥轮换」。
- 独立密钥库（`secrets` 表，P5 批次 B）**没有多钥迁移**：轮换 `AEGIS_SECRET_KEY` 后旧密文不可读，且读取路径不存在，只能按运维流程重建。备份/恢复与租户迁移时，`secrets` 密文与**备份时点的当前密钥**绑定：恢复演练必须使用该时点的 `AEGIS_SECRET_KEY`，否则整库恢复后这些密文无法解密。
- 轮换窗口内不要删除 `AEGIS_SECRET_KEY_PREVIOUS`；备份与导出包应按"需要哪把钥"记录密钥标识（不要记录密钥明文）。密钥与数据库备份必须分开保存。

### 演练记录模板

| 字段 | 内容 |
| --- | --- |
| 演练时间 | YYYY-MM-DD HH:MM–HH:MM（时区） |
| 版本 | 后端/前端发布版本、期望 schema 版本、实际 `schema_migrations` 最大版本 |
| 备份 | dump 文件名、大小、SHA-256、存储位置（加密/异地） |
| 恢复 | 目标库、`restore-postgres.ps1` 结果、恢复后行数摘要、`verify-schema` 结果 |
| 租户迁移 | 租户 slug/id、导出目录、manifest 总行数、导入目标库、导入后总行数、逐表比对结果 |
| 密钥校验 | `cmd/rekey` dry run 输出（scanned/already_current/reencrypted/failed）、使用的密钥标识 |
| 冒烟 | `/readyz`、管理员登录、项目/构建/部署/审查列表、跨租户 404 |
| 实测 RTO/RPO | 从开始恢复到可登录的耗时、最近可恢复数据时间点 |
| 负责人/结论 | 执行人、复核人、是否通过、遗留问题与后续动作 |

演练至少每季度一次，在隔离环境进行，不接触生产凭据；演练记录按内部合规要求留存，不写入仓库。

## 监控与健康检查

- `/healthz` 是进程存活（不查依赖），供编排系统做 liveness；`/readyz` 检查 PostgreSQL 与 Redis（2s 超时），依赖不可用时返回 503，供 readiness 与入口摘流。
- 官方镜像内置 Docker `HEALTHCHECK`：每 30s 用 wget 请求 `/healthz`，3s 超时，启动宽限 10s；compose 样例给两个 api 副本配置了 10s 间隔的 healthcheck，并让 gateway 等 `service_healthy` 后才启动。容器 healthy 不代表依赖可用，按依赖摘流仍应使用 `/readyz`。
- `AEGIS_METRICS_ADDR` 在 compose 中已透传，默认空表示关闭；启用后监听在独立地址，提供无鉴权的 Prometheus 文本端点，因此只能绑定回环，或仅经内网/受保护代理抓取，不要与 API 端口共用。在容器内抓取需要显式监听 `0.0.0.0:<port>`，并只把该端口发布到受控主机地址（compose 末尾有注释示例）。
- 日志为结构化 JSON，带 `requestId`、method、path、status、duration；排查时用 `X-Request-ID` 贯穿代理与应用日志。日志与审计都不含密码、令牌或 Cookie 明文。
- 告警至少覆盖：`/readyz` 连续非 200、PostgreSQL 连接数接近上限（副本数 × `AEGIS_DB_MAX_OPEN`）、Redis 不可用、构建队列持续堆积。

## 测试

后端真实集成：在 `backend` 目录设置 AEGIS_TEST_DATABASE_URL 与 AEGIS_TEST_REDIS_URL 后运行 `go test ./...`（仓库根目录没有 `go.mod`），测试使用随机隔离 schema 和 Redis namespace。测试数据库角色需要 CREATE SCHEMA 与 DROP SCHEMA 权限，不使用生产数据库。

浏览器 E2E：设置 AEGIS_E2E_DATABASE_URL 指向专用可丢弃数据库、AEGIS_E2E_REDIS_URL 指向测试 Redis，再 npm run test:e2e。E2E 会初始化管理员和创建用户，不要指向生产；8080/5173 须空闲。本地重复运行需要空库时，先执行 `pwsh scripts/reset-e2e-db.ps1`（默认读取 `AEGIS_E2E_DATABASE_URL`，删除并重建该一次性库；`-WhatIf` 只打印将执行的 SQL）；CI 的 e2e 任务每次使用全新的 PostgreSQL 服务容器，无需重置。多实例集成测试额外启动两个真实 httptest HTTP server、独立数据库池及 Redis clients，验证会话互通、刷新/退出撤销及共享限额。

部署配置的 CI 检查：`infra` 任务执行 `docker build -t aegisflow-api:ci ./backend`，在占位 `AEGIS_POSTGRES_PASSWORD`/`AEGIS_JWT_SECRET`（只用于 compose 插值，不会启动服务）下运行 `docker compose config -q`，并用本样例同款 `nginx:1.28-alpine` 镜像对网关配置运行 `nginx -t`。nginx 在加载配置时解析 upstream 主机名，因此该检查用 `--add-host api1:127.0.0.1 --add-host api2:127.0.0.1` 满足解析、只校验语法与指令，不启动整套双副本样例；本机没有 Docker 守护进程时以 CI 结果为准。

SQLite 开发库不自动迁移。若有重要数据先备份，另行执行经审核的离线导入；上线不依赖保留开发会话。
