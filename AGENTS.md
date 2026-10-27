# AegisFlow 项目约束

适用于本项目的一切贡献者，**包括 AI 编码代理**。与 [TODO.md](TODO.md)、[鉴权契约](docs/auth-contract.md)、[后端文档](backend/README.md) 配套阅读；如有冲突，以本文与鉴权契约为准。

## 协作纪律（硬性）

1. 所有变更通过 Pull Request 进入 main，不直接推送 main。
2. **每个 PR 必须由仓库所有者（@yichenfchai）本人 review 并批准后方可合并。** AI 代理不得自行合并、不得代替所有者批准、不得以任何方式绕过该要求。
3. AI 代理允许：创建分支、本地提交、推送功能分支、创建 PR；禁止：合并 PR、推送或改写 main、force push、无视或关闭 review 状态。
4. PR 描述必须写明变更内容与**验证记录**（执行了哪些命令、结果如何）；未经验证的变更不具备合并条件。
5. PR 标题使用 conventional commits 风格（feat / fix / refactor / test / docs / chore / ci）。

## 开发纪律

- **基础设施不先行于业务验证**：先明确业务需求与验收标准，再扩展基础设施；不为“以后可能用到”提前建设。
- **演示页边界**：不提供业务演示数据、演示页或演示开关；未接入的模块恒定显示未实现声明，不用 mock 冒充业务结果。鉴权、项目、构建、账号与审计始终调用真实接口（见 [鉴权契约](docs/auth-contract.md)「开发模式和验收」）。
- **鉴权永远真实**：无 mock 回退、无 JSON refresh token 通道；不因实现便利放宽既定安全约定（Origin 白名单、Sec-Fetch-Site、Cookie 属性、限流 fail-closed 等，详见鉴权契约）。
- **秘密与隐私**：日志、审计、错误信息不得出现密码、令牌、Cookie、请求正文明文；存储的秘密必须哈希或加密；禁用状态等账号信息不得向无凭据者泄露。
- **品牌与事实性**：统一使用 AegisFlow；不得引入旧品牌 GoPulse、虚构版本号、未声明的许可证或其他法律性断言。
- **架构**：业务逻辑不依赖 Gin Context，业务模块按需抽 Repository；沿用 auth 包确立的事务内审计、advisory 锁、`context.Context` 传递模式。

## 验证基线（PR 合并前必须全绿）

- 后端：`go build ./...`、`go vet ./...`、`gofmt -l ./cmd ./internal` 输出为空，真实 PostgreSQL + Redis 下 `go test -race ./...`。测试因缺少真实服务而 Skip 不得视为通过。
- 前端：`npm run build`、`npm run lint`（0 错误；既有 2 条警告可保留，不新增）、`npm test`。
- 浏览器：`npm run test:e2e`（一次性 E2E 数据库，用例使用唯一用户名避免跨运行残留）。涉及 UI 的变更必须过 E2E。

## 文档同步（与代码同一 PR）

接口或行为变更必须同步：`backend/openapi.yaml`（`openapi_test.go` 强制校验路由已写入契约）、`docs/auth-contract.md`、`backend/README.md`、根 `README.md` 与 `TODO.md`（勾选状态与验证记录）。

## 约束变更

修改本文同样走 PR，并由所有者 review。
