# 受控执行环境与仓库 clone 设计（P3 下一批）

> 状态：**④a、④b 已实现；④c 待开工**。本文定义边界、选项与验收标准。
> 已落地：环境变量白名单、0700 构建目录、**输出脱敏（fail closed）**、**④b 容器执行器**（`AEGIS_BUILD_SANDBOX=container`，按 D1/D2/D4 建议实现：容器隔离、2 核/2 GiB/512 进程默认配额、启动探测 fail closed）。仍待 ④c（clone，迁移 v6）。
> 关联：[TODO](../TODO.md) P3 第 3 项、[后端文档](../backend/README.md)「构建执行」、[鉴权契约](auth-contract.md)「构建执行边界」。

## 1. 现状与风险

当前 `internal/builds` 的执行器是 P3 第一批的最小实现：

- 以**服务进程身份**运行 `sh -c` / `cmd /c`，命令来自项目配置；
- 工作目录是系统临时目录（`os.MkdirTemp`），构建结束删除；
- **继承服务进程的全部环境变量**（`exec.Command` 默认行为），包括 `AEGIS_JWT_SECRET`、`AEGIS_DATABASE_URL`、`AEGIS_REDIS_URL`、bootstrap 凭据等；
- 只有 wall-clock 超时（`AEGIS_BUILD_TIMEOUT`），没有 CPU/内存/进程数上限；
- 没有输出脱敏；
- 不 clone 仓库，只执行命令；`repoUrl` 字段当前仅为登记信息。

按严重度排序的缺口：

| # | 缺口 | 影响 | 修复批次 |
| --- | --- | --- | --- |
| 1 | 构建命令继承服务环境变量 | 任意项目命令可读取 JWT 密钥、数据库/Redis 凭据，等同凭据泄露 | ④a（已实现） |
| 2 | 以服务账号身份执行 | 命令可读写服务可访问的任何文件；可修改二进制/配置 | ④b |
| 3 | 无资源上限 | 一个构建可耗尽 CPU/内存/进程数，拖垮同实例的其他请求 | ④b |
| 4 | 输出未脱敏 | 命令输出可能把自身读到的敏感值写进日志表并被所有角色读取 | ④a（已实现，fail closed） |
| 5 | 临时目录权限 | 同机其他用户可读（默认 0700 于 MkdirTemp，实际已受限，但依赖 umask 与平台） | ④a（已实现，显式 chmod） |
| 6 | 不 clone 仓库 | 无法构建真实仓库；直接 clone 会把不可信代码引入执行环境 | ④c（依赖 ④b） |

**结论：在 ④a/④b 完成前，只能把项目命令视为可信输入；不要把不可信仓库或外部贡献者的命令接入当前执行器。** 这一点必须继续写进 README 与鉴权契约。

## 2. 目标与非目标

目标：

- 构建命令**看不到**服务凭据；环境变量按白名单显式注入。
- 构建进程以**低权限身份**运行，无法写服务目录、无法访问数据库/Redis 网络（除非显式放行）。
- 每个构建有 CPU、内存、进程数与磁盘的**硬上限**，超限被终止并记录原因。
- 日志输出经过**脱敏**后才落库。
- 可选地，从项目 `repoUrl` **浅克隆**仓库到构建目录，并在其中执行命令。

非目标（本批明确不做）：

- 多租户隔离、按项目分配独立机器；
- 构建产物（artifact）的持久化、缓存与制品库；
- 私有仓库凭据管理（依赖 P4 密钥管理；本批只支持无凭据的 https/git 或本机路径）；
- 分布式 worker（仍由单实例租约保证只有一个 worker）。

## 3. 执行身份与隔离：三个选项

### 选项 A：专用 OS 账号（最低成本）

Linux 用 `setpriv`/`runuser` 以 `aegis-builder` 运行；Windows 用独立本地账号或 `CreateProcessAsUser`。构建目录归该账号所有。

- 优点：不需要容器运行时；改动集中在 `execute`；
- 缺点：仍与宿主共享内核/网络/文件系统视图；需要部署方预先创建账号与目录权限；Windows 下实现复杂（服务账号权限、桌面会话）；
- 适用：单机、自托管、命令来源可信。

### 选项 B：每次构建一个容器（推荐）

worker 调用容器运行时（Docker/Podman）启动一次性容器：只挂载构建目录（可写）与仓库（只读），`--network none`（或按需 egress 白名单），`--memory`/`--cpus`/`--pids-limit` 限制资源。

- 优点：隔离最强、资源上限原生、跨平台一致；镜像可控（固定工具链）；
- 缺点：引入容器运行时依赖（当前 compose 已有 Docker，但生产不强制）；需要镜像构建与版本管理；容器内 UID 映射与目录权限要处理；
- 适用：面向不可信仓库、多人协作的正式部署。

### 选项 C：OS 沙箱原语（Linux bubblewrap/firejail；Windows AppContainer/Job Objects）

- 优点：比 A 强、比 B 轻；
- 缺点：平台差异大（Windows 侧要单独实现），维护成本高；
- 适用：只部署 Linux 且不想引入容器。

**建议**：默认实现 **B**；A 作为「无容器运行时的降级路径」，并用配置开关显式声明（例如 `AEGIS_BUILD_SANDBOX=container|user|none`，`none` 仅允许开发环境并在启动日志/README 警告）。无论选哪个，④a（环境变量白名单 + 目录权限 + 脱敏）都是前置条件且独立可交付。

## 4. 资源上限

| 资源 | Linux（容器/cgroup v2） | 无容器降级 | Windows |
| --- | --- | --- | --- |
| CPU | `--cpus` / `cpu.max` | `systemd-run --scope -p CPUQuota` 或 `nice` | Job Object CPU rate control |
| 内存 | `--memory` / `memory.max` | `ulimit -v`（近似） | Job Object `JOB_OBJECT_LIMIT_PROCESS_MEMORY` |
| 进程数 | `--pids-limit` | `ulimit -u` | Job Object active process limit |
| 磁盘 | 容器临时层 + 目录配额 | 目录配额（XFS project quota）或事后清理 | 目录配额 |
| 时间 | 现有 `AEGIS_BUILD_TIMEOUT` | 同左 | 同左 |

默认配额建议（可在配置中覆盖）：CPU 2 核、内存 2 GiB、进程 512、构建目录 2 GiB、超时 10 分钟。超限行为：终止进程树，构建记为 `failed`，并写一条 system 日志说明超限类型（不含命令内容）。

## 5. 环境变量与输出脱敏（④a）

- `cmd.Env` 只包含显式白名单：`PATH`、`HOME`（指向构建目录）、`LANG`/`LC_ALL`、`TMPDIR`（指向构建目录内），以及未来的项目级构建变量（存储在密钥管理后）。
- 明确**不继承** `AEGIS_*`、代理变量、云厂商凭据变量。
- 输出脱敏：对日志行做模式替换（`AEGIS_[A-Z_]+=...`、`Bearer ...`、`postgres://user:pass@`、`redis://:pass@`、常见 token 前缀），替换为 `[redacted]`；脱敏在写入 `build_log_events` 前完成，且统计被替换次数写入 system 行（不写原值）。**已实现**：规则见 `internal/builds/redact.go`，覆盖上述模式并扩展了 JWT 与 PEM 私钥头；命中后若仍能匹配到原值，整行丢弃并计入 system 统计（D6 fail closed）。未覆盖多行私钥正文与自由文本口令。
- 验收：构建命令执行 `env` 时，日志中不出现任何 `AEGIS_` 变量；用包含伪凭据的命令验证替换生效。（两条均已由 `internal/builds` 的集成测试覆盖。）

## 6. 仓库 clone（④c）

- 仅当项目 `repoUrl` 非空时 clone；`--depth=1 --single-branch --branch <build.branch 或 defaultBranch>`；
- 目标目录 `workdir/repo`，命令在该目录执行（`cmd.Dir = repoDir`）；
- URL 校验沿用 `projects.validateRepoURL`（https/git@/file:/// 绝对路径）；`file:///` 与绝对路径仅在配置显式允许时使用（默认关闭，避免读取宿主任意仓库）；
- 私有仓库：本批不支持凭据；遇到认证失败记录明确的 system 日志并失败，不提示输入凭据；
- clone 超时计入构建超时；clone 失败 → `failed`，system 日志记录 exit code 与 stderr 脱敏摘要；
- 构建记录增加 `repoUrl` 快照字段（迁移 v5），保证构建可复现，且项目后续修改 URL 不影响历史构建。

## 7. 数据模型与配置变更

- 迁移 **v6**：`builds.repo_url TEXT NOT NULL DEFAULT ''`（触发时快照）。原计划写的是 v5，但 v5 已被项目可见性与成员表占用（[项目级成员权限设计](project-permissions-design.md) §5）；`internal/dbmigrate` 是共享的版本化账本，按落地顺序编号，④c 需追加 v6。
- 配置新增：
  - `AEGIS_BUILD_SANDBOX=container|user|none`（默认 `none` 用于开发，生产应设 `container`）。**已落地**：`container` 与 `none` 可用，`user` 仍在启动校验中被拒绝；`none` 启动打印警告。
  - `AEGIS_BUILD_IMAGE`（默认 `alpine:3.20`）、`AEGIS_BUILD_CPU`（默认 2）、`AEGIS_BUILD_MEMORY`（默认 `2g`）、`AEGIS_BUILD_PIDS`（默认 512）——**已落地**，仅在 `container` 模式生效。
  - `AEGIS_BUILD_DISK`（默认 2 GiB）：容器临时层与目录配额尚未强制执行，留到 ④c 一并处理；
  - `AEGIS_BUILD_ALLOW_LOCAL_REPO=true|false`（默认 false；随 ④c 加入）；
  - `AEGIS_BUILD_CLONE=true|false`（默认 false，④c 灰度用）。
- 启动校验：**已落地**——`AEGIS_BUILD_SANDBOX=container` 时探测容器运行时（`docker version`），不可用即拒绝启动；`none` 时启动日志明确警告；`user` 拒绝启动。

## 8. 验收标准

- ④a：`env` 构建不泄露服务变量；日志脱敏命中伪凭据；目录权限测试（非属主不可读）；E2E 不变。
- ④b：容器/低权限账号下构建成功；内存/CPU/进程数超限能终止且状态为 `failed`；服务目录不可写测试通过。
- ④c：公开 https 仓库浅克隆成功并在仓库内执行命令；clone 失败给出脱敏原因；本地路径默认拒绝；构建记录含 repoUrl 快照；E2E 增加一次真实 clone（或本地 git 仓库）的构建。

## 9. 分阶段计划

| 阶段 | 内容 | 依赖 | 风险 |
| --- | --- | --- | --- |
| ④a | 环境变量白名单、显式目录权限、输出脱敏、配置开关（默认行为不变） | 无 | 低；应立即做 |
| ④b | 容器执行（或 OS 账号降级）、资源上限、启动校验 | ④a；需确定部署形态 | 中；需要 CI/本机容器运行时 |
| ④c | repoUrl 快照、浅克隆、clone 失败处理 | ④b；需确定是否支持私有仓库 | 高；涉及不可信代码 |

## 10. 需要所有者拍板的决策

| # | 决策 | 选项 | 建议 | 状态 |
| --- | --- | --- | --- | --- |
| D1 | 生产部署形态 | 单机二进制 / Docker Compose / 容器平台 | 若已有 Compose，选容器执行器 | 已按建议实现（2026-10-30） |
| D2 | 执行隔离 | A 专用账号 / B 容器 / C OS 沙箱 | B；A 作为无容器降级 | B 已实现；A 未做 |
| D3 | 是否本批支持私有仓库 clone | 否 / 用部署密钥 / 等 P4 密钥管理 | 本批否 | ④c 待办 |
| D4 | 默认资源配额 | 2C/2G/512 进程/2G 磁盘 | 按上表，可配置 | CPU/内存/进程已实现；磁盘未强制 |
| D5 | 本地路径仓库 | 默认禁用 / 允许（仅自托管） | 默认禁用 | ④c 待办 |
| D6 | 日志脱敏失败策略 | 记录原文并告警 / 丢弃该行 | 丢弃并计数（fail closed） | 已实现 |

> ④a/④b 已按建议落地；④c 仍按 D3/D5 拆分独立 PR，开工前需所有者确认私有仓库与本地路径策略。
