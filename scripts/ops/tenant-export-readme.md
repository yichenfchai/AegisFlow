# AegisFlow 租户导出包

本目录由 `scripts/ops/export-tenant.ps1` 生成，用于把一个租户的全部业务数据迁移到另一个 AegisFlow 部署（或同一部署的另一个数据库）。目录中每个 CSV 对应一张表，`manifest.json` 记录 schema 版本、租户信息、逐表行数、列顺序与 SHA-256，本 README 说明恢复方式与安全边界。

## 内容

| 文件 | 说明 |
| --- | --- |
| `manifest.json` | 导出格式版本、源库 schema 版本、租户 id/slug/name/status、每张表的行数/列顺序/字节数/SHA-256、总行数、工具版本与生成时间。 |
| `tenants.csv` … `project_members.csv` | 该租户的逐表数据（UTF-8、CSV HEADER）。按外键安全顺序排列，可直接按顺序导入。 |
| `README.md` | 本文件。 |

导出范围：`tenants`（仅该租户一行）、`auth_users`、`auth_audit`、`projects`、`builds`、`build_log_events`、`deployment_targets`、`reviews`、`review_external_results`、`review_notifications`、`deployments`、`deployment_events`、`approvals`、`approval_notifications`、`scans`、`security_events`、`knowledge_docs`、`secrets`、`project_policies`，以及 `project_members`（该表没有 `tenant_id`，通过所属项目归属租户；不导出会丢失私有项目成员）。

刻意排除：`auth_sessions`、`auth_refresh_tokens`、`oidc_login_states` 属于短期会话/一次性状态，迁移后用户重新登录即可。

## 密钥绑定（最重要）

- `secrets.csv` 中的 `ciphertext` 与 `deployment_targets.csv` 中的 `secret_cipher` 是 **AES-256-GCM 密文**，不是明文。它们由源部署的 `AEGIS_SECRET_KEY` 密封。
- 导入到目标部署后，**必须使用与源部署相同且含同一密钥的 `AEGIS_SECRET_KEY`**（若源部署正在轮换密钥，还需同时提供 `AEGIS_SECRET_KEY_PREVIOUS`），否则这些密文无法解密，独立密钥库与部署目标凭据都只能重新录入。
- 若源部署做过 `cmd/rekey` 且已移除旧钥，则只需当前 `AEGIS_SECRET_KEY`；恢复演练时可用 `cmd/rekey` 的 dry run 验证目标库中的密文能被提供的密钥打开。
- `auth_users.password_hash` 是 bcrypt 哈希（不是明文口令），导入后用户可用原密码登录；本导出包不包含任何明文密码、访问令牌或 Cookie。

## 恢复步骤

目标数据库必须已经由同一发布线的 server 启动过并完成迁移（schema 版本 >= manifest 的 `schemaVersion`）。导入在单个事务内完成，任一行数/表头/校验和不符都会整体回滚。

```powershell
# Windows PowerShell 5.1；连接口令通过 PGPASSWORD 传入，不写在命令行
$env:PGPASSWORD = Get-Content C:\secrets\pg.txt
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/import-tenant.ps1 `
  -TenantDir <本目录的绝对路径> `
  -Database <目标库> -Host <主机> -Port <端口> -User <用户>
```

- 目标库中已存在同 slug（或同 tenant id）的租户时，脚本默认拒绝；确认要覆盖时加 `-Force`（只删除并重建该租户的数据，不动其他租户，删除与导入在同一事务）。
- 目标 schema 比导出包更新时，脚本要求显式 `-AllowNewerSchema`，请先确认新增迁移是加法变更（AegisFlow v19 起迁移不回退，升级/回滚见 `deploy/README.md` 的生产化运维章节）。
- 导入完成后保持 `AEGIS_DEFAULT_TENANT_SLUG` 指向目标库中仍存在的租户，再启动/滚动重启 API，并用租户 slug 登录验证项目列表与 `/readyz`。

## 安全与生命周期

- 本导出包包含用户密码哈希、审计记录与加密凭据密文，**泄漏等价于数据库泄漏**：请加密存储、限制访问、按备份保留策略管理，不要把目录提交到 Git 或粘贴到工单/CI 日志。
- 传输/归档前核对 `manifest.json` 中的 SHA-256；`import-tenant.ps1` 会在写入前重新校验每个 CSV。
- 迁移/恢复建议在停写窗口内导出与切换；导出本身在单个 `REPEATABLE READ` 快照中完成，不会因其他租户写入而撕裂，但业务侧仍建议避免对导出租户发起部署、审批等写操作。
