# 项目级成员权限设计（P4 前置）

> 状态：**设计草案，待所有者评审**。本文只定义模型、边界与验收标准，不包含实现。
> 关联：[鉴权契约](auth-contract.md) 角色矩阵、[TODO](../TODO.md) P4「对象级授权」、[受控执行环境设计](build-execution-design.md)。

## 1. 现状

- 角色是**全局**的：admin / member / viewer。admin 管理用户与审计；admin/member 写项目与构建；所有角色可读所有项目、构建与日志。
- 没有项目级成员：任何 viewer 都能看到全部项目的仓库地址、构建命令与日志；任何 member 都能修改/归档任意项目、触发/取消任意构建。
- 现有约定明确写着「项目级成员权限留待后续设计」，页面守卫不能代替服务端授权。

在多人、多实例部署目标下，这有两个问题：

1. **横向越权**：一个团队的项目对其他团队完全可见、可写；
2. **无法按项目委托**：管理员无法让某人只维护某个项目。

## 2. 目标与非目标

目标：

- 项目可以有**成员**；成员身份决定该项目的可见性与写权限。
- 全局角色仍是上限：viewer 永远只读，admin 永远可管理，成员身份不能提升全局角色。
- 默认行为向后兼容：现有项目保持「公开」，不因本次变更突然不可见。
- 服务端每次请求校验；未授权读私有项目返回 **404**（避免枚举），写操作返回 403/404。

非目标：

- 多租户（按组织隔离数据、独立数据库/schema）；
- 按项目自定义角色矩阵（保持少量固定角色）；
- 部署/审查/扫描等未实现模块的成员权限（本批只覆盖项目与构建，其余模块接入时复用同一模型）。

## 3. 模型

### 3.1 可见性与成员

- `projects.visibility`：`public`（默认）| `private`。
- `project_members(project_id, user_id, role, created_by, created_at)`：
  - `role`：`maintainer`（可读写项目、触发/取消构建、管理成员）| `viewer`（只读项目与其构建/日志）。
- 项目创建者自动成为 `maintainer`（owner 语义），不可被移除；`created_by` 仍是快照。
- 全局 admin 隐含对所有项目拥有 maintainer 权限（可管理成员），不需要成员行。

### 3.2 授权规则

| 操作 | 全局 admin | 全局 member | 全局 viewer | 项目 maintainer | 项目 viewer | 非成员 |
| --- | --- | --- | --- | --- | --- | --- |
| 读 public 项目/构建/日志 | 允许 | 允许 | 允许 | 允许 | 允许 | 允许 |
| 读 private 项目/构建/日志 | 允许 | 仅成员 | 仅成员 | 允许 | 允许 | **404** |
| 修改/归档 private 项目 | 允许 | 仅 maintainer | 禁止 | 允许 | 禁止 | 404 |
| 触发/取消 private 项目构建 | 允许 | 仅 maintainer | 禁止 | 允许 | 禁止 | 404 |
| 管理 private 项目成员 | 允许 | 仅 maintainer | 禁止 | 允许 | 禁止 | 404 |
| 修改/归档 public 项目 | 允许 | 允许 | 禁止 | — | — | — |
| 触发/取消 public 项目构建 | 允许 | 允许 | 禁止 | — | — | — |
| 管理 public 项目成员 | 允许 | 允许（仅成员列表） | 禁止 | — | — | — |

> 规则概括：**全局角色决定"能不能写"，项目成员决定"能写/能看哪个项目"**。private 项目在全局角色之上再叠加成员门槛；public 项目维持现状（成员列表可选，仅用于将来切换 private 时预置）。

### 3.3 对象继承

构建、日志、审计对象继承项目的可见性：

- `GET /builds` 与 `GET /builds?projectId=` 只返回当前用户可见项目的构建；
- `GET /builds/{id}`、`/logs`、`/events`：项目不可见时 404；
- 触发/取消：项目不可见 404，可见但无写权限 403；
- 审计行包含项目对象时，仅 admin 可查（现状不变）。

## 4. API 变更

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| PATCH | /api/v1/projects/{id} | 全局 admin/member + 项目写权限 | 新增 `visibility: public\|private` 字段 |
| GET | /api/v1/projects/{id}/members | 项目可见者 | 成员列表（id/username/role/createdAt） |
| POST | /api/v1/projects/{id}/members | admin 或项目 maintainer | `{userId, role}`，重复添加 409 |
| PATCH | /api/v1/projects/{id}/members/{userId} | admin 或项目 maintainer | `{role}` |
| DELETE | /api/v1/projects/{id}/members/{userId} | admin 或项目 maintainer | 创建者不可移除（400） |

- 私有项目的 404 语义同时用于以上所有路径（非成员无法确认项目存在）。
- 成员变更写审计：`project.member_add` / `project.member_update` / `project.member_remove`，objectType `project`；detail 含成员用户名与角色（沿用 #7 的事务内审计模式）。
- OpenAPI 增加 `visibility`、成员端点与 `AuditEntry.action` 枚举；`openapi_test` 覆盖路由。

## 5. 数据与迁移

迁移 v5：

```sql
ALTER TABLE projects ADD COLUMN visibility TEXT NOT NULL DEFAULT 'public'
  CHECK (visibility IN ('public','private'));
CREATE TABLE project_members (
  project_id TEXT NOT NULL REFERENCES projects(id),
  user_id    TEXT NOT NULL REFERENCES auth_users(id),
  role       TEXT NOT NULL CHECK (role IN ('maintainer','viewer')),
  created_by TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  PRIMARY KEY (project_id, user_id)
);
CREATE INDEX project_members_user_idx ON project_members(user_id, project_id);
```

- 存量项目 `visibility='public'`，无成员行 → 行为不变。
- 删除用户时成员行如何处理：当前没有删除用户接口（只有禁用）；禁用用户的成员行保留，但禁用账号无法登录。**待决策 D5**。
- 查询列表时避免 N+1：`projects` 列表用 `LEFT JOIN project_members` 或 `EXISTS` 过滤，并为 `(user_id, project_id)` 建索引。

## 6. 前端

- 项目列表/详情显示「公开/私有」徽章；非成员看不到私有项目。
- 新建/编辑项目增加可见性选择（默认公开）。
- 项目详情增加成员管理页（列表 + 添加 + 改角色 + 移除），仅 admin/maintainer 可见。
- 构建列表按可见项目过滤（后端负责，前端无需额外逻辑）。
- 所有按钮隐藏只是交互优化，服务端仍逐请求校验。

## 7. 测试与验收

- `internal/projects`：可见性过滤（public/private × admin/member/viewer/成员/非成员）、成员增删改、创建者不可移除、重复添加 409、404 枚举语义；
- `internal/builds`：`List` 过滤、Get/Logs/触发/取消的 404/403；
- `httpapi`：成员端点 + 审计行 + OpenAPI 路由契约；
- E2E：创建私有项目 → 非成员直接访问 URL 得到未找到；邀请 viewer 成员后可读不可写；maintainer 可触发构建并管理成员；
- 回归：现有 public 项目行为不变（auth.spec 现有用例应全部通过）。

## 8. 分阶段计划

| 阶段 | 内容 | 依赖 | 说明 |
| --- | --- | --- | --- |
| P1 | 迁移 v5、visibility、成员表与成员 API、读过滤（项目 + 构建） | D1-D5 | 不做 UI 也可先交付后端 |
| P2 | 写权限叠加（成员 maintainer）、审计行、OpenAPI | P1 | |
| P3 | 前端徽章、成员管理页、E2E | P2 | |

## 9. 需要所有者拍板的决策

| # | 决策 | 选项 | 建议 |
| --- | --- | --- | --- |
| D1 | 默认可见性 | public / private | **public**（向后兼容，避免现有项目突然消失） |
| D2 | 成员角色粒度 | 两种（maintainer/viewer）/ 三种（+owner）/ 一种 | **两种**；创建者即 owner 语义 |
| D3 | 成员管理权 | admin+maintainer / 仅 admin / 仅 maintainer | **admin + maintainer** |
| D4 | 私有项目不可见时的响应 | 404 / 403 | **404**（避免枚举） |
| D5 | 用户禁用/删除后的成员行 | 保留 / 级联删除 | 保留（禁用已阻止登录），删除用户接口出现时再级联 |
| D6 | 本批范围 | 仅项目+构建 / 同时改造未实现模块 | **仅项目+构建**，其余模块接入时复用 |

> 确认后按 P1→P3 拆 PR；P1 可先只做后端（迁移 + 读过滤 + 成员 API），P3 前端单独评审。
