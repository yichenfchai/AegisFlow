# AegisFlow Go 后端

Go 1.23+，Gin 1.11 + slog，SQLite（modernc，无需 CGO），bcrypt，HS256 JWT。此阶段提供公共服务基础和本地账号鉴权；构建、发布等业务接口及 LDAP 尚未实现，现有前端仍是 mock，没有切换为真实鉴权。

## 本地启动（PowerShell）

在项目 backend 目录执行：

```powershell
go mod download
$keyBytes = New-Object byte[] 48
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($keyBytes)
$rng.Dispose()
$env:AEGIS_JWT_SECRET = [Convert]::ToBase64String($keyBytes)
$env:AEGIS_BOOTSTRAP_USERNAME = 'admin'
# 密码至少 12 字节、至多 72 字节，避免写进脚本或提交 Git。
$credential = Get-Credential -UserName 'admin' -Message '设置首次管理员密码'
$env:AEGIS_BOOTSTRAP_PASSWORD = $credential.GetNetworkCredential().Password
go run ./cmd/server
```

默认地址 http://127.0.0.1:8080。启动自动建库、迁移，数据库默认位于当前目录的 data/aegisflow.db。首次空库创建管理员；已有用户时不会覆盖管理员。初始化后移除 bootstrap 环境变量。JWT 密钥应通过部署密钥管理保存并在重启时保持一致；更换它会让已有 access token 失效。不要为正式环境每次重新生成。

环境变量清单见 `.env.example`。服务只读取进程环境，不自动加载 .env 文件；没有内置密码或 JWT 密钥。数据库与 WAL/SHM 文件包含敏感信息，应限制宿主机访问权限、备份并保护备份。Windows 上 Go 的权限位不能替代 Windows 文件权限配置。

## HTTP 接口

成功：`{"data": ..., "requestId": "..."}`；失败：`{"error":{"code":"unauthorized","message":"..."},"requestId":"..."}`。所有写请求使用 `Content-Type: application/json`，只接受单个 JSON 对象，拒绝未知字段，最大 16 KiB。受保护接口使用 `Authorization: Bearer <accessToken>`。

| 方法 | 路径 | 权限 / 请求体 |
| --- | --- | --- |
| GET | /healthz | 存活检查，无鉴权 |
| GET | /readyz | 数据库就绪检查，无鉴权 |
| POST | /api/v1/auth/login | `{ "username":"admin", "password":"..." }` |
| POST | /api/v1/auth/refresh | `{ "refreshToken":"..." }` |
| POST | /api/v1/auth/logout | `{ "refreshToken":"..." }`，撤销整个会话 |
| GET | /api/v1/auth/me | 已登录 |
| GET | /api/v1/users | admin |
| POST | /api/v1/users | admin；`{ "username":"reader", "password":"...", "displayName":"Reader", "role":"viewer" }` |

登录和刷新返回 `data: {user, accessToken, refreshToken, expiresIn}`，expiresIn 单位为秒。User 包含 id、username、displayName、role，无密码哈希。用户名为 3–64 个 ASCII 字母、数字、下划线、点或短横线，首字符须为字母或数字，匹配不区分大小写；密码 12–72 字节，displayName 最多 256 字节。角色为 admin / member / viewer；本阶段只有管理员用户管理接口，两种非管理员角色都可读取自己的资料；后续业务接口应在服务端独立配置权限，不能只依赖前端隐藏按钮。

调用示例：

```powershell
$body = @{ username = 'admin'; password = $credential.GetNetworkCredential().Password } | ConvertTo-Json
$session = (Invoke-RestMethod 'http://127.0.0.1:8080/api/v1/auth/login' -Method Post -ContentType 'application/json' -Body $body).data
Invoke-RestMethod 'http://127.0.0.1:8080/api/v1/auth/me' -Headers @{ Authorization = "Bearer $($session.accessToken)" }
```

## 会话与公共设施

- Access JWT 默认 15 分钟，验证 HS256、issuer、audience、过期时间和数据库会话状态；退出后当前会话签发的 access token 立即失效。
- Refresh token 使用密码学随机值，只存 SHA-256 哈希，每次刷新事务轮换。旧值再次使用会撤销整个会话（包括新 access token）。客户端必须串行刷新，避免多个请求同时使用旧 refresh token。会话绝对期限默认 30 天，刷新不延长。
- 用户和会话持久化；没有公共注册接口，只允许管理员创建用户。JWT 密钥和数据库可用性都是鉴权的前提。
- 登录按直连 IP 每分钟最多 10 次，单进程内存限流，最多记录 10000 个 IP；忽略不可信 X-Forwarded-For。代理部署需另配可信代理及集中限流；当前多实例不共享计数。
- 严格 CORS 来源白名单，默认 http://localhost:5173，无通配符、无 Cookie 鉴权。前端接入时需设计 refresh token 安全存储；建议 BFF 或 HttpOnly Cookie 方案，若改 Cookie 需同时实现 CSRF 防护。
- 请求 ID、JSON 结构化访问日志、鉴权操作日志、panic 恢复、安全响应头、请求大小上限、HTTP 超时、信号触发优雅退出。日志不记录令牌、密码和请求体；当前审计输出日志，不是持久化审计查询服务。
- SQLite 单连接、WAL、foreign_keys、busy_timeout。适合单实例起步；后续按业务需求引入 PostgreSQL、Redis、集中式迁移及完整权限模型。

对公网提供服务时在 TLS 反向代理后运行，保持后端绑定回环地址；请勿用未加密 HTTP 传送账号令牌。

## 验证

```powershell
go test ./...
go vet ./...
go build ./cmd/server
# 支持 race detector 且已安装 C 编译器的环境可额外执行：
go test -race ./...
```

## 架构与 HTTP 框架

采用前后端分离的模块化单体架构，HTTP 层使用 Gin。cmd/server 通过标准库 http.Server 承载 Gin Engine，保留超时和优雅退出；internal/config 负责配置校验；internal/auth 负责用户、密码、会话、迁移及令牌；internal/httpapi 使用 Gin 路由组、Handler 和中间件组织 HTTP 接口。

/api/v1/auth 提供登录、刷新、退出和当前用户接口；/api/v1/users 路由组统一挂载身份认证及管理员角色中间件。Gin Context 只在 HTTP 层使用，鉴权业务继续接收标准 context.Context。JSON 解码按请求执行严格校验，不修改 Gin 全局 binding 设置；使用自定义 slog 日志及恢复中间件，通过统一 JSON 返回错误。未知路径、错误方法和额外尾斜杠均返回统一 404，不自动重定向。

Gin 关闭可信代理，不接受任意 X-Forwarded-For / X-Real-IP 作为登录限流依据。Gin 1.11 与现有 Go 1.23 要求兼容，升级到 Gin 1.12 需要同时升级 Go 最低版本至 1.25。
