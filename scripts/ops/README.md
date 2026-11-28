# AegisFlow 生产化运维脚本

本目录提供备份/恢复、租户级导出/导入与 schema 校验五个 PowerShell 脚本，用于真实生产与演练环境。它们**不进入 CI**：脚本需要真实 PostgreSQL 客户端与真实数据库（甚至生产只读账号），CI 无生产凭据，因此手工或运维流水线执行。

与业务代码无关：脚本只调用 `pg_dump` / `pg_restore` / `psql` 和服务已有的运维命令（`cmd/tenant`、`cmd/rekey`），不修改 schema、不写业务表（导入除外，且只写目标租户数据并在单事务内校验）。

## 脚本清单

| 脚本 | 用途 | 关键参数 | 退出码 |
| --- | --- | --- | --- |
| `backup-postgres.ps1` | 全库逻辑备份（custom 格式）+ SHA-256 sidecar + `pg_restore --list` 校验 | `-Database`（必填）、`-Host`、`-Port`、`-User`、`-OutputDir`、`-SkipVerify` | 0 成功；非 0 失败 |
| `restore-postgres.ps1` | 恢复整库备份到新/重建的数据库并校验 schema 与行数 | `-DumpFile`（必填）、`-TargetDatabase`（必填）、`-Host`、`-Port`、`-User`、`-Force` | 0 成功；非 0 失败 |
| `export-tenant.ps1` | 导出一个租户到 CSV + manifest + README | `-TenantSlug`（必填）、`-Database`（必填）、`-Host`、`-Port`、`-User`、`-OutputDir` | 0 成功；非 0 失败 |
| `import-tenant.ps1` | 单事务导入一个租户导出包 | `-TenantDir`（必填）、`-Database`（必填）、`-Host`、`-Port`、`-User`、`-Force`、`-AllowNewerSchema` | 0 成功；非 0 失败 |
| `verify-schema.ps1` | 只读校验 schema 版本（**仅 v19+**）、租户隔离列/外键/索引与行数摘要 | `-Database`（必填）、`-Host`、`-Port`、`-User`、`-ExpectedVersion` | 0 全部通过；1 有失败项；2 跳过（schema < v19，不是失败） |

`ops-common.ps1` 为以上脚本共享的辅助函数（工具解析、psql 调用、租户表清单/清理语句、序列修复 SQL），不单独执行。`tenant-export-readme.md` 是导出包内 `README.md` 的模板（导出时复制，中文说明密钥绑定与恢复方式）。

## 前置条件

- **PowerShell**：Windows PowerShell 5.1 与 PowerShell 7（`pwsh`）均可；脚本只用 5.1 兼容语法。示例统一用 `powershell.exe -NoProfile -ExecutionPolicy Bypass -File`。
- **PostgreSQL 客户端**：需要 `pg_dump`、`pg_restore`、`psql`（版本与服务器主版本一致或更高）。解析顺序：
  1. 显式参数 `-PgDumpPath` / `-PgRestorePath` / `-PsqlPath`；
  2. 环境变量 `AEGIS_PG_DUMP_PATH` / `AEGIS_PG_RESTORE_PATH` / `AEGIS_PSQL_PATH`；
  3. 本机工具目录 `D:\AegisFlow-tools\pgsql\pgsql\bin\`；
  4. `PATH`。

  例如：`$env:AEGIS_PSQL_PATH = 'C:\Program Files\PostgreSQL\17\bin\psql.exe'`。
- **口令**：脚本**没有密码参数**，也从不把口令放到命令行或打印。认证完全交给 libpq：
  - 进程内 `$env:PGPASSWORD`（只影响当前 PowerShell 进程）；
  - `$env:PGPASSFILE` 或 `%APPDATA%\postgresql\pgpass.conf`；
  - peer/trust 连接（本地演练常见）。

  生产建议由密钥注入工具临时设置 `PGPASSWORD`，不要写进脚本、命令历史或工单。
- **主机/端口/用户**：默认 `-Host 127.0.0.1 -Port 5433 -User postgres`（本仓库本地演练环境）。生产显式传 `-Host`/`-Port`/`-User`。可用 `-SslMode require|verify-ca|verify-full` 开启 TLS（不传时沿用环境里的 `PGSSLMODE`）。
- **数据库权限**：备份账号至少对所有表有 `SELECT`（建议独立只读账号）；恢复/创建库需要 `CREATEDB`；租户导入需要目标库写权限与建临时表权限。`cmd/tenant`/`cmd/rekey` 复用 `AEGIS_DATABASE_URL`。
- **schema 版本**：租户导出/导入与多租户校验要求 schema v19（阶段 F2）；`verify-schema.ps1` 的 48 项检查只描述 v19+ 结构，v18 基线会打印 `[SKIP]` 并以退出码 2 结束（见第 5 节）。目标库必须**先由 server 启动完成迁移**；导入脚本不会执行迁移。
- **密钥**：`secrets.ciphertext` 与 `deployment_targets.secret_cipher` 由 `AEGIS_SECRET_KEY` 密封（轮换期还可能需要 `AEGIS_SECRET_KEY_PREVIOUS`）。备份/导出前确认密钥已单独备份，恢复/导入后使用同一密钥。

## 用法

所有示例都假定在仓库根目录执行，且已按上一节设置 `$env:PGPASSWORD`（或使用 trust/pgpass）。

### 1. 全库备份

```powershell
$env:PGPASSWORD = Get-Content C:\secrets\pg.txt
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/backup-postgres.ps1 `
  -Database aegisflow -Host db.internal -Port 5432 -User aegis_backup -OutputDir D:\backups
```

- 产物：`<db>-<yyyyMMdd-HHmmss>.dump` 与同名 `.dump.sha256`（`<sha256>  <文件名>`，与 `sha256sum` 兼容）。
- 默认执行 `pg_restore --list` 校验：打印归档内表数、TOC 条目数、归档库名、dump 版本、大小、SHA-256 与耗时；校验失败退出码非 0。
- `-SkipVerify` 跳过读取校验（哈希仍会写），仅在已另有校验流程时使用。
- 归档包含用户密码哈希、会话/refresh 哈希、审计、密封凭据密文：**必须加密存储、异地保存、限制访问**，并按 RPO 设置保留策略。

### 2. 恢复整库

```powershell
# 恢复到全新库（目标库必须不存在）
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/restore-postgres.ps1 `
  -DumpFile D:\backups\aegisflow-20261128-231500.dump -TargetDatabase aegisflow_restore_drill

# 仅用于可丢弃的演练库：-Force 会 DROP 并重建目标库
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/restore-postgres.ps1 `
  -DumpFile D:\backups\aegisflow-20261128-231500.dump -TargetDatabase aegisflow_restore_drill -Force
```

- 目标库已存在时**默认拒绝**；`-Force` 会先 `DROP DATABASE ... WITH (FORCE)` 再重建，等价于销毁目标库内容，绝不要指向在线生产库。
- dump 旁存在 `.sha256` 时先校验；不匹配或 sidecar 格式非法则**在触碰目标库之前**失败。
- `pg_restore` 使用 `--exit-on-error --single-transaction`：失败时不会留下半恢复结构（库可能已创建但为空）。
- 恢复后自动校验：`schema_migrations` 存在且从 1 连续；脚本先读取恢复库的 `schema_migrations` 最大版本，再打印 `schema version X`、所需表 N/N 齐全与关键表/治理数据行数摘要（含 `tenants`、`auth_users`、`projects`、`builds`、`deployment_targets`、`deployments`、`approvals`、`secrets`）。**恢复库的 schema 版本由 dump 决定**：恢复 v18 备份（回滚）是合法场景，版本低于 v19 时脚本打印 WARNING 提示（不阻塞、不按 v19 列检查），结构校验失败才退出码非 0。
- **恢复演练的密钥一致性校验不要用 `cmd/rekey` 单钥运行。** `cmd/rekey` 是部署凭据**轮换**工具，强制要求 `AEGIS_SECRET_KEY` 与 `AEGIS_SECRET_KEY_PREVIOUS` 同时存在且不同（单钥运行 `exit=1`，报 `rekey: AEGIS_SECRET_KEY_PREVIOUS is required`），它不是“备份恢复校验”命令。

**rekey：轮换用法与恢复库可解性冒烟（两者不同）**

1. **正式轮换（唯一支持的正式用法）**：新钥写入 `AEGIS_SECRET_KEY`、原当前钥移到 `AEGIS_SECRET_KEY_PREVIOUS` → 在 `backend` 目录跑默认 dry run 看 `scanned`/`reencrypted`/`failed` 统计 → `-apply` 重加密 → 再跑一次 dry run 确认 `reencrypted=0` → 移除 `AEGIS_SECRET_KEY_PREVIOUS`。

   ```powershell
   cd backend
   $env:AEGIS_DATABASE_URL = '<连接 URL>'
   $env:AEGIS_SECRET_KEY = '<新钥 base64 32B>'
   $env:AEGIS_SECRET_KEY_PREVIOUS = '<旧钥 base64 32B>'
   go run ./cmd/rekey          # dry run：确认 reencrypted 与预期一致且 failed=0
   go run ./cmd/rekey -apply   # 按行重加密（只改 deployment_targets.secret_cipher）
   go run ./cmd/rekey          # 再跑：reencrypted=0 表示轮换完成
   ```
2. **恢复/导入库的可解性冒烟（只读）**：只能做“轮换模拟”——`AEGIS_SECRET_KEY` 用**新随机钥**、`AEGIS_SECRET_KEY_PREVIOUS` 用**备份/导出时点的钥**，默认 dry run 要求 `failed=0`；`reencrypted>0` 说明备份期密钥能解开这些密文。**不要**对要保留基线的恢复库加 `-apply`（那会改变恢复库状态）。

   ```powershell
   # 只读冒烟：不要加 -apply
   $env:AEGIS_DATABASE_URL = '<恢复库 URL>'
   $env:AEGIS_SECRET_KEY = '<本次演练新生成的随机钥>'
   $env:AEGIS_SECRET_KEY_PREVIOUS = '<备份时点的当前钥>'
   go run ./cmd/rekey
   ```
3. **备份恢复后的密钥一致性**优先走应用路径：用恢复库启动临时实例，管理员登录并读取/探测依赖凭据的业务路径（例如部署目标「检查」会在内存中解密凭据并发起请求）；`secrets` 表按设计没有读取路径，只能通过密钥管理流程确认。不要为了“单钥校验”修改 `cmd/rekey`；预发演练与生产恢复都要保留备份时点的密钥（及轮换期旧钥），详见 [deploy/README.md](../../deploy/README.md) 的备份与恢复、密钥轮换章节。

### 3. 租户级导出

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/export-tenant.ps1 `
  -TenantSlug acme -Database aegisflow -Host db.internal -Port 5432 -User aegis_backup -OutputDir D:\tenant-moves
```

- 按 `slug` 解析租户，在**单个 `REPEATABLE READ READ ONLY` 快照**中导出 20 张表：`tenants`（仅该行）、`auth_users`、`auth_audit`、14 张业务表、`build_log_events` / `deployment_events`（按父表 join 租户）与 `project_members`（无 `tenant_id`，通过项目归属；不导出会丢失私有项目成员）。
- 刻意排除短期状态：`auth_sessions`、`auth_refresh_tokens`、`oidc_login_states`（迁移后用户重新登录）。
- 产物目录：

  ```text
  tenant-<slug>-<yyyyMMdd-HHmmss>/
    manifest.json          # 格式版本、schema 版本、租户信息、逐表行数/列顺序/SHA-256、生成时间、工具版本
    tenants.csv … project_members.csv
    README.md              # 导出包内说明：密钥绑定与恢复步骤
  ```

- 导出目录已存在时拒绝覆盖；中途失败会删除半成品目录。
- `secrets.ciphertext`、`deployment_targets.secret_cipher` 是**密文**，不是明文；`auth_users.password_hash` 是 bcrypt 哈希，不含明文口令、令牌或 Cookie。导出包泄漏等价于数据库泄漏。
- 如果需要在迁移切换时得到"安静"快照（避免导出瞬间仍有部署/审批写入），请安排停写窗口；普通导出本身是快照一致的。

### 4. 租户级导入

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/import-tenant.ps1 `
  -TenantDir D:\tenant-moves\tenant-acme-20261128-231500 -Database aegisflow_drill `
  -Host db.internal -Port 5432 -User aegis_backup

# 目标库已有同 slug/同 id 租户、确认要替换时（只替换该租户，其他租户不动）：
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/import-tenant.ps1 `
  -TenantDir D:\tenant-moves\tenant-acme-20261128-231500 -Database aegisflow_drill -Force

# 目标 schema 比导出包新（确认新增迁移是加法）时：
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/import-tenant.ps1 `
  -TenantDir D:\tenant-moves\tenant-acme-20261128-231500 -Database aegisflow_drill -AllowNewerSchema

# 全新迁移库 + 默认租户导出包：首次导入默认租户必须 -Force（只替换该租户）
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/import-tenant.ps1 `
  -TenantDir D:\tenant-moves\tenant-default-20261128-231500 -Database aegisflow_drill -Force
```

> **全新迁移库首次导入默认租户必须 `-Force`。** 目标库由 server 启动完成 v19 迁移后会 seed 同 id/slug 的默认租户（`tenant_default_0000000000000001` / `default`）和 bootstrap 管理员；默认租户导出包携带同一 id/slug，脚本按设计默认拒绝覆盖。因此把**默认租户**导回全新库时，首次导入必须显式 `-Force`：
>
> - **语义**：在同一个导入事务内删除并重建**该租户**的数据（子表优先，最后删 `tenants` 行；外键 RESTRICT 兜底），其他租户完全不动；失败整体回滚。
> - **后果**：目标库该租户迁移后临时创建的数据（包括 seed 的 bootstrap 管理员、其会话/审计）会被导出包内容替换；导入后以导出包中的管理员/凭据为准。导入前确认目标库可丢弃该租户的现有数据。
> - **不需要 `-Force` 的情况**：导入其他（非默认）租户，且目标库没有同 slug/同 id 冲突；或目标库本就没有该租户。
>
> 常见错误信息：`target database '...' already contains tenant 'default' (id tenant_default_0000000000000001); refusing to import without -Force`（见「故障排查」）。

校验与安全语义：

1. 读取 `manifest.json`：格式/格式版本、租户 id/slug、表清单、列顺序、逐文件 SHA-256 与表头均必须在写入前通过。
2. 目标库 `schema_migrations` 必须 **>= 导出包 schemaVersion**；更高时要求显式 `-AllowNewerSchema`（先确认新迁移是加法）。
3. 目标库已有同 slug（或同 tenant id）租户时默认拒绝；`-Force` 在**同一事务**内先删除该租户的全部数据（子表优先，最后删 tenants 行，外键 RESTRICT 兜底）再导入，绝不隐式覆盖其他租户。全新迁移库导入默认租户即属此类（seed 默认租户已存在），首次必须 `-Force`，见上文说明。
4. 全部 `\copy`、序列修复与行数校验在**一个事务**内：任一行数/表头/schema 不匹配即整体回滚，目标库保持原样。
5. 导入完成后逐表打印行数并与 manifest 比对；失败则数据库无变化。
6. 目标库必须已经迁移且 `AEGIS_DEFAULT_TENANT_SLUG` 指向的租户仍存在（服务器启动会校验默认租户，导入不会自动创建）。

### 5. schema 校验

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/verify-schema.ps1 `
  -Database aegisflow_restore_drill -Host db.internal -Port 5432 -User aegis_backup -ExpectedVersion 19
```

只读检查并以 `[PASS]`/`[FAIL]` 输出：

- `schema_migrations` 存在、版本从 1 连续、可选 `-ExpectedVersion` 匹配；
- `tenants` 存在且至少一行（并打印各租户 slug/status/用户数）；
- 24 张 schema-v19 表齐全；
- 14 张业务表的 `tenant_id` 为 `NOT NULL text` 且有指向 `tenants(id)` 的外键；`auth_users`/`auth_sessions`/`auth_audit` 同样 NOT NULL 外键，`oidc_login_states` 为可空外键；
- 5 个代表性唯一索引存在且唯一（如 `projects_tenant_name_lower_idx`、`secrets_tenant_name_key`）；
- 全部表行数摘要。

任何 `[FAIL]` 都会汇总并以非 0 退出。适合在迁移后、恢复后、导入后以及日常巡检中运行。

**适用范围（v19+）**：上述 48 项检查描述的是 v19 结构。脚本会先读取 `schema_migrations` 最大版本：若数据库版本（或显式传入的 `-ExpectedVersion`）低于 19，脚本打印 `[SKIP] verify-schema supports schema >= 19; for a v18 baseline use the psql checks documented in scripts/ops/README.md` 并以**退出码 2** 结束——这不是数据库故障，而是 v18 基线不适用 v19 结构检查。升级前记录 v18 基线请用下面的 psql 检查，升级到 v19 后再用本脚本验证（`-ExpectedVersion 19` 应为 48/48 PASS）。

**v18 基线检查（升级前记录，使用 psql）**：

```powershell
$env:PGPASSWORD = Get-Content C:\secrets\pg.txt
$psql = 'D:\AegisFlow-tools\pgsql\pgsql\bin\psql.exe'   # 或 PATH/安装位置中的 psql
& $psql -X -w -h db.internal -p 5432 -U aegis_backup -d aegisflow `
  -c "SELECT COALESCE(MAX(version),0) AS schema_max, COUNT(*) AS migrations FROM schema_migrations" `
  -c "SELECT id, slug, name, status FROM tenants ORDER BY slug" `
  -c "SELECT table_name FROM information_schema.columns WHERE table_schema = 'public' AND column_name = 'tenant_id' AND table_name IN ('projects','builds','deployment_targets','deployments','approvals','approval_notifications','reviews','review_external_results','review_notifications','scans','security_events','knowledge_docs','secrets','project_policies') ORDER BY table_name" `
  -c "SELECT 'tenants' AS table_name, count(*) FROM tenants UNION ALL SELECT 'auth_users', count(*) FROM auth_users UNION ALL SELECT 'projects', count(*) FROM projects UNION ALL SELECT 'builds', count(*) FROM builds UNION ALL SELECT 'deployments', count(*) FROM deployments"
```

预期（v18 = 多租户 F1 基线）：

- `schema_max=18, migrations=18`（连续 1..18）；
- `tenants` 至少一行，默认租户为 `tenant_default_0000000000000001 / default / active`；
- 第三条查询**返回 0 行**：14 张业务表还没有 `tenant_id`（v18 只有 `auth_users`/`auth_sessions`/`auth_audit`/`oidc_login_states` 4 张 auth 表有该列）；
- 最后一条行数摘要是升级前基线，升级到 v19 后用同一命令对比（业务表行数应不变）。

升级到 v19 后，第三条查询应返回 14 行，且可用 psql 复核 14 张表的 `tenant_id` 全部非空并回填到默认租户（例如 `SELECT count(*) FROM projects WHERE tenant_id IS NULL` 应为 0）。恢复 v18 备份（回滚）后同样用本节 psql 基线检查，`verify-schema` 会 `[SKIP]`/exit 2，属预期。

## 演练执行顺序（建议，与 v18→v19 全停窗口流程一致）

> **v18→v19 必须是全停窗口，不能滚动升级。** v19 迁移给 14 张业务表新增 `tenant_id NOT NULL` 并回填收紧：旧 v18 代码的 INSERT 不写该列，迁移完成后仍运行的旧副本写入会失败；旧二进制看到 schema 19 会拒绝启动（`unsupported schema migration version 19`，exit 1）。因此升级与回滚演练都按“全停”设计，不要演练“先起新副本/逐副本滚动”。

1. 记录版本与配置：`cmd/tenant list`、`SELECT max(version) FROM schema_migrations`、`AEGIS_SECRET_KEY` 指纹（只记录哈希前的标识，不要记录密钥本身）；v18 基线另用第 5 节 psql 检查记录（14 张业务表 `tenant_id` 应返回 0 行）。
2. 全库备份并保留 `.sha256`；`backup-postgres.ps1` 输出中的 SHA-256 写入演练记录，`pg_restore --list` 校验失败不得继续。
3. **升级演练（隔离库/预发，全停）**：网关摘流 → 停掉**全部**旧副本与 worker（确认无旧进程在写库）→ 起**单个** v19 实例完成迁移并等 `/readyz` 200 → `verify-schema.ps1 -ExpectedVersion 19`（48/48 PASS）→ 冒烟（`/healthz`、管理员/租户登录、列表、跨租户 404）→ 再起其余副本恢复流量。**不要**先起新副本或逐副本滚动。
4. **回滚演练（同样全停）**：停全部副本 → `restore-postgres.ps1` 把升级前 dump 恢复到新库（脚本会打印恢复库的 `schema version X / 表数 N/N / 行数摘要`；恢复 v18 dump 时会提示版本低于 v19 并继续，属合法回滚路径）→ 用旧 v18 二进制指向恢复库完成 `/readyz` + 登录 + 项目列表冒烟。恢复库是 v18 时 `verify-schema` 会 `[SKIP]`/exit 2，用第 5 节 psql 基线检查代替，不要当成失败。
5. 密文可解性冒烟：按第 2 节“rekey：轮换用法与恢复库可解性冒烟”做**轮换模拟**（新随机钥当 `AEGIS_SECRET_KEY`、备份时点钥当 `AEGIS_SECRET_KEY_PREVIOUS`），默认 dry run 要求 `failed=0`；不要单钥跑 `cmd/rekey`，也不要对恢复库加 `-apply`。
6. 租户迁移演练：`export-tenant.ps1` → 在另一个已迁移到 v19 的临时库 `import-tenant.ps1`（**默认租户首次导入必须 `-Force`**，见第 4 节）→ `verify-schema.ps1 -ExpectedVersion 19` → 逐表行数/关键数据与源库比对 → 临时实例按 `tenantSlug` 登录该租户验证。
7. 记录：时间、版本、备份哈希、恢复/导入行数、校验结果、RTO/RPO 实测、负责人；模板见 [deploy/README.md](../../deploy/README.md) 的「生产化运维」章节。
8. 清理临时库/实例与本地导出目录，确认未改动生产库。

## 安全注意事项

- **备份/导出即敏感数据**：dump 与租户导出包包含用户 bcrypt 密码哈希、审计、会话/refresh 哈希（整库备份）、以及 `AEGIS_SECRET_KEY` 密封的凭据密文。必须加密存储、最小权限访问、按保留策略销毁；不要把文件放进仓库、CI artifact、工单或聊天。
- **不要在日志中打印秘密**：脚本只打印库名/host/port、行数、文件大小、dump 文件哈希；不打印 `PGPASSWORD`、密钥、密文或用户哈希。排障时也不要手工 `SELECT ciphertext` 贴到工单。
- **恢复目标隔离**：`restore-postgres.ps1` 默认拒绝覆盖已存在库；`-Force` 仅用于可丢弃演练库。恢复出的库在验证前不要接入生产流量；恢复到生产必须按变更窗口停写、切连接串并保留回滚备份。
- **租户导入的破坏性**：`import-tenant.ps1 -Force` 会删除目标库中同 slug/同 id 租户的数据（同一事务内，失败回滚）。对生产执行前先确认目标库、备份目标库、并确认没有其他租户数据被误删。
- **密钥绑定**：备份/导出包只有在持有对应 `AEGIS_SECRET_KEY` 时才完整；密钥与备份必须分开保存，二者同时泄漏等价于凭据泄漏。轮换期间保留 `AEGIS_SECRET_KEY_PREVIOUS`，直到 `cmd/rekey` 轮换完成（再次 dry run 为 `reencrypted=0`）才可移除；不要用单钥 `cmd/rekey` 校验恢复库（见第 2 节）。
- **连接安全**：生产显式 `-SslMode verify-full` 并配置 `PGSSLROOTCERT`；不要把 `sslmode=disable` 用于生产。脚本使用 `-w`（从不交互式询问口令），口令缺失会直接失败而不是挂起。
- **文件路径**：脚本用 psql `\copy` 写文件，导出/备份目录不要放在 Web 根目录、共享盘或同步目录；Windows 上路径中的单引号会被正确转义，但请保持路径简洁。

## 产物与 .gitignore

仓库根 `.gitignore` 已忽略运维产物，避免误提交：

```text
*.dump
*.dump.sha256
tenant-*-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]-[0-9][0-9][0-9][0-9][0-9][0-9]/
```

即便如此，也请把 `-OutputDir` 指向仓库外（例如 `D:\backups`、`D:\tenant-moves`）。备份/导出产物永不入库。

## 故障排查

| 现象 | 处理 |
| --- | --- |
| `pg_dump/psql/pg_restore not found` | 安装 PostgreSQL 客户端，或设置 `AEGIS_*_PATH`，或传 `-PgDumpPath`/`-PsqlPath`/`-PgRestorePath`。 |
| `psql exited with code 2` 且提示 `fe_sendauth: no password supplied` | 当前进程没有 `PGPASSWORD`/pgpass 且服务器要求口令；设置后重试（脚本不会提示输入）。 |
| `database schema is at version X but export needs v19` | 先用新版本 server 启动目标库完成迁移；不要用脚本手工改 `schema_migrations`。 |
| `target database ... already exists; refusing ... without -Force` | 换一个新库名做演练；只有确认目标可丢弃时才用 `-Force`。 |
| `target database ... already contains tenant ...` | 目标库已有同 slug/同 id 租户；确认要替换时用 `-Force`，否则先清理目标或换库。**全新迁移库的 seed 默认租户是常见情况**：首次导入默认租户必须 `-Force`（只替换该租户），见第 4 节。 |
| `[SKIP] verify-schema supports schema >= 19; ...`（退出码 2） | 数据库或 `-ExpectedVersion` 低于 v19（例如升级前 v18 基线）；这不是失败。用第 5 节 psql 基线检查；升级到 v19 后再运行 `verify-schema -ExpectedVersion 19`。 |
| `SHA-256 mismatch` | 备份/导出包损坏或被修改；不要继续恢复/导入，回到备份源重新获取。 |
| `tenant import row count mismatch` | 导出包与 manifest 不一致或列映射不兼容；事务已回滚，检查 psql 报错与 schema 版本。 |
| `restored database ... failed structural verification` | 归档不是 AegisFlow schema，或 `schema_migrations` 不完整/不连续；检查是否选错 dump。恢复更旧版本的 dump 不会因此失败（脚本按 dump 版本打印摘要并提示）。 |
| 恢复后凭据不可解密 | 未使用与备份时点对应的 `AEGIS_SECRET_KEY`（轮换期还需旧钥）。**不要**用 `cmd/rekey` 单钥运行校验：它需要 `AEGIS_SECRET_KEY_PREVIOUS` 且必须不同。按第 2 节做轮换模拟 dry run（`failed=0`）或走登录/部署目标探测路径核对；确认前不要删除旧钥。 |

## 与升级/回滚的关系

`deploy/README.md` 的「生产化运维（升级/回滚/备份/租户级迁移）」是这些脚本的流程上下文：schema v19 起迁移只向前、不提供 down migration，**回滚 = 恢复升级前备份 + 旧二进制**。并且 v18→v19（以及任何新增 NOT NULL 列并回填收紧的迁移）要求**全停窗口**：旧代码写入不兼容，旧二进制看到 schema 19 会拒绝启动，**不能滚动升级**。升级前必须先用本目录的 `backup-postgres.ps1` 备份，并记录当前 schema 版本与结构基线：v19+ 用 `verify-schema.ps1`，v18 基线用第 5 节 psql 检查（`verify-schema` 对 v18 会 `[SKIP]`/exit 2）。恢复演练与租户迁移演练都应在隔离库中进行，不接触生产凭据。
