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
| `verify-schema.ps1` | 只读校验 schema 版本、租户隔离列/外键/索引与行数摘要 | `-Database`（必填）、`-Host`、`-Port`、`-User`、`-ExpectedVersion` | 0 全部通过；非 0 有失败项 |

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
- **schema 版本**：租户导出/导入与多租户校验要求 schema v19（阶段 F2）。目标库必须**先由 server 启动完成迁移**；导入脚本不会执行迁移。
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
- 恢复后自动校验：`schema_migrations` 存在且从 1 连续、24 张 schema-v19 表齐全、打印关键表/治理数据行数摘要；结构校验失败则退出码非 0。
- 恢复演练还要验证密文可解：用备份时相同的 `AEGIS_SECRET_KEY`（轮换期为当前钥+旧钥）对恢复库执行 `cd backend; go run ./cmd/rekey`（默认 dry run，`failed=0` 表示全部可解）。详见 [deploy/README.md](../../deploy/README.md) 的备份与恢复、密钥轮换章节。

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
```

校验与安全语义：

1. 读取 `manifest.json`：格式/格式版本、租户 id/slug、表清单、列顺序、逐文件 SHA-256 与表头均必须在写入前通过。
2. 目标库 `schema_migrations` 必须 **>= 导出包 schemaVersion**；更高时要求显式 `-AllowNewerSchema`（先确认新迁移是加法）。
3. 目标库已有同 slug（或同 tenant id）租户时默认拒绝；`-Force` 在**同一事务**内先删除该租户的全部数据（子表优先，最后删 tenants 行，外键 RESTRICT 兜底）再导入，绝不隐式覆盖其他租户。
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

## 演练执行顺序（建议）

1. 记录版本与配置：`cmd/tenant list`、`SELECT max(version) FROM schema_migrations`、`AEGIS_SECRET_KEY` 指纹（只记录哈希前的标识，不要记录密钥本身）。
2. 全库备份并保留 `.sha256`；`backup-postgres.ps1` 输出中的 SHA-256 写入演练记录。
3. 恢复演练：`restore-postgres.ps1` 到隔离临时库 → `verify-schema.ps1 -ExpectedVersion <当前版本>` → 用相同密钥 `cmd/rekey` dry run 验证密文可解 → 临时 API 实例（独立端口/Redis namespace）确认 `/readyz`、管理员登录、项目/构建列表。
4. 租户迁移演练：`export-tenant.ps1` → 在另一个已迁移的临时库 `import-tenant.ps1` → `verify-schema.ps1` → 逐表行数/关键数据与源库比对 → 临时实例按 `tenantSlug` 登录该租户验证。
5. 记录：时间、版本、备份哈希、恢复/导入行数、校验结果、RTO/RPO 实测、负责人；模板见 [deploy/README.md](../../deploy/README.md) 的「生产化运维」章节。
6. 清理临时库/实例与本地导出目录，确认未改动生产库。

## 安全注意事项

- **备份/导出即敏感数据**：dump 与租户导出包包含用户 bcrypt 密码哈希、审计、会话/refresh 哈希（整库备份）、以及 `AEGIS_SECRET_KEY` 密封的凭据密文。必须加密存储、最小权限访问、按保留策略销毁；不要把文件放进仓库、CI artifact、工单或聊天。
- **不要在日志中打印秘密**：脚本只打印库名/host/port、行数、文件大小、dump 文件哈希；不打印 `PGPASSWORD`、密钥、密文或用户哈希。排障时也不要手工 `SELECT ciphertext` 贴到工单。
- **恢复目标隔离**：`restore-postgres.ps1` 默认拒绝覆盖已存在库；`-Force` 仅用于可丢弃演练库。恢复出的库在验证前不要接入生产流量；恢复到生产必须按变更窗口停写、切连接串并保留回滚备份。
- **租户导入的破坏性**：`import-tenant.ps1 -Force` 会删除目标库中同 slug/同 id 租户的数据（同一事务内，失败回滚）。对生产执行前先确认目标库、备份目标库、并确认没有其他租户数据被误删。
- **密钥绑定**：备份/导出包只有在持有对应 `AEGIS_SECRET_KEY` 时才完整；密钥与备份必须分开保存，二者同时泄漏等价于凭据泄漏。轮换期间保留 `AEGIS_SECRET_KEY_PREVIOUS`，`cmd/rekey` 完成后才可移除。
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
| `target database ... already contains tenant ...` | 目标库已有同 slug/同 id 租户；确认要替换时用 `-Force`，否则先清理目标或换库。 |
| `SHA-256 mismatch` | 备份/导出包损坏或被修改；不要继续恢复/导入，回到备份源重新获取。 |
| `tenant import row count mismatch` | 导出包与 manifest 不一致或列映射不兼容；事务已回滚，检查 psql 报错与 schema 版本。 |
| `restored database ... failed structural verification` | 归档不是 AegisFlow schema 或 schema_migrations 不完整；检查是否选错 dump、是否来自更旧版本。 |
| 恢复后凭据不可解密 | 未使用与备份时相同的 `AEGIS_SECRET_KEY`（轮换期还需旧钥）；用 `cmd/rekey` dry run 核对，不要删除旧钥。 |

## 与升级/回滚的关系

`deploy/README.md` 的「生产化运维（升级/回滚/备份/租户级迁移）」是这些脚本的流程上下文：schema v19 起迁移只向前、不提供 down migration，**回滚 = 恢复升级前备份 + 旧二进制**。升级前必须先用本目录的 `backup-postgres.ps1` 备份、用 `verify-schema.ps1` 记录基线版本；恢复演练与租户迁移演练都应在隔离库中进行，不接触生产凭据。
