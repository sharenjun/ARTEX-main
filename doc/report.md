# ARTEX 代码优化报告

## Go 大文件按职责拆分

- 日期：2026-10-10
- 状态：本轮拆分与验证完成
- 范围：`server/server.go`、`server/server_mgmt.go`，优先整理 HTTP 接口与服务装配。
- 基线：当前工作区；拆分前，这两个文件与发布标签 `v0.3.18` 没有代码差异。保留工作区原有的 ScopeSentry 修复、回归测试、CHANGELOG 和 `doc/plan.md`。
- 使用的 Go skills：`golang-how-to`、`golang-refactoring`、`golang-project-layout`、`golang-code-style`、`golang-naming`、`golang-documentation`。

### 拆分依据

按资源和职责归属组织文件：同一类接口及其直接辅助函数放在一起；HTTP 接口、服务运行时、纯数据转换分别组织。1000 行作为检查提醒，不设置固定切割线，也不为了平均行数切断完整函数。

所有声明仍在 `server` 包中，名称、可见性和调用方式保持一致。`Server` 类型、构造和启动恢复留在主文件；路由集中在一个文件，锁、缓存和上下文仍由原对象管理。此次是结构调整，没有修改 Planner 收官或其他业务行为。

### 完成内容

| 原文件 | 拆分前行数 | 拆分后保留行数 | 保留职责 |
| --- | ---: | ---: | --- |
| [server.go](../server/server.go) | 3950 | 269 | Server 状态、构造、启动恢复 |
| [server_mgmt.go](../server/server_mgmt.go) | 2260 | 25 | 管理接口共用的数据库可用性和请求解析 |

新增 30 个文件，与原文件合计 32 个；其中最大文件为 499 行。行数包含注释和空行，仅用于描述结果。

#### 服务运行与 HTTP 接口

| 文件 | 行数 | 职责 |
| --- | ---: | --- |
| [routes.go](../server/routes.go) | 325 | 路由注册与 CORS |
| [llm_runtime.go](../server/llm_runtime.go) | 372 | LLM 配置解析、Provider 缓存、Agent 装配与失效 |
| [health_api.go](../server/health_api.go) | 104 | 健康检查、系统统计、维护入口 |
| [http_response.go](../server/http_response.go) | 24 | 共用 JSON 响应、错误响应及整数默认值转换 |
| [task_api.go](../server/task_api.go) | 285 | 任务列表、创建、查询、状态控制和 LLM 链配置 |
| [intent_api.go](../server/intent_api.go) | 225 | 意图查询、暂停、取消与重跑 |
| [task_seed.go](../server/task_seed.go) | 141 | 初始目标解析、资产播种和首个意图 |
| [task_scope_api.go](../server/task_scope_api.go) | 172 | 任务范围、资产引用与覆盖度查询 |
| [findings_api.go](../server/findings_api.go) | 499 | 漏洞查询、修改、导出及来源追踪 |
| [exploration_api.go](../server/exploration_api.go) | 228 | 探索图谱、节点及继承历史 |
| [activity_api.go](../server/activity_api.go) | 303 | 活动历史、详情与 SSE 推送 |
| [token_stats_api.go](../server/token_stats_api.go) | 72 | Token 总量、日统计和会话统计 |
| [logs_api.go](../server/logs_api.go) | 115 | 服务日志查询、历史与 SSE 推送 |
| [traffic_api.go](../server/traffic_api.go) | 190 | 流量查询、删除、交换详情与正文读取 |
| [settings_api.go](../server/settings_api.go) | 354 | 全局设置、Python 检测与 Web 搜索配置验证 |
| [task_chat_api.go](../server/task_chat_api.go) | 238 | 任务主会话、运行控制与备用回复 |
| [report_api.go](../server/report_api.go) | 41 | 报告与审计输出 |
| [llm_api.go](../server/llm_api.go) | 311 | 全局 LLM 配置接口、连接测试与模型列表 |

#### 管理接口与独立转换

| 文件 | 行数 | 职责 |
| --- | ---: | --- |
| [task_delete.go](../server/task_delete.go) | 150 | 任务删除屏障、停止等待和删除结果 |
| [agents_api.go](../server/agents_api.go) | 345 | Agent 管理、运行参数和可见性配置 |
| [agent_prompts_api.go](../server/agent_prompts_api.go) | 199 | 提示词版本、收尾提示、变量与预览接口 |
| [tools_api.go](../server/tools_api.go) | 114 | 内置工具查询、编辑和恢复默认 |
| [mcp_api.go](../server/mcp_api.go) | 161 | MCP 配置、工具发现与资源可见性 |
| [skills_api.go](../server/skills_api.go) | 282 | Skill 列表、使用统计、创建、元数据编辑与可见性 |
| [skill_frontmatter.go](../server/skill_frontmatter.go) | 146 | Skill 元数据解析、YAML 重写及字段整理 |
| [skill_upload.go](../server/skill_upload.go) | 188 | Skill ZIP 上传、读取和大小限制 |
| [skill_files.go](../server/skill_files.go) | 267 | Skill 名称、路径校验和文件操作 |
| [llm_profiles_api.go](../server/llm_profiles_api.go) | 145 | LLM Profile 管理、激活和删除后的任务恢复 |
| [llm_policy_api.go](../server/llm_policy_api.go) | 74 | 重试策略与熔断状态接口 |
| [prompt_template.go](../server/prompt_template.go) | 129 | 全局变量目录、模板校验、渲染与字段提取 |

### 行为保持与验证

先使用 CodeGraph 梳理入口、调用和测试，再用 Go 标准库 `go/parser`、`go/ast` 确定完整声明边界。搬移前保留工作区快照，生成后执行 `gofmt`，逐项比较排除位置和注释信息的声明语法树。

- 199 个声明、186 个函数的语法树全部一致；函数体、类型、初始化表达式、路由注册和参数标签保持一致。
- 新文件仅增加所需导入；没有新增包、导出接口、适配层或依赖。
- 原有 ScopeSentry Skill、元数据回归测试、CHANGELOG、`doc/plan.md` 的内容哈希保持一致。
- 沿用现有回归测试，本次没有增加仅检查文件搬移的测试。

| 检查 | 结果 |
| --- | --- |
| `gofmt -l`（本轮 32 个文件） | 通过，无待格式化文件 |
| `git diff --check` | 通过 |
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go run ./tests/run.go --all -run '^$'` | 通过，ARTEX 162 个、Norma 98 个包内测试文件可编译 |
| 完整 server 回归，拆分前后对照 | 均为 129 项通过、32 项失败、66 项跳过；失败集合完全相同，无新增失败 |
| 排除基线已失败的 32 项后运行 server 回归 | 通过：129 项通过、66 项跳过 |

测试统计以顶层测试为单位。32 项基线失败中，31 项缺少 PostgreSQL 测试配置，1 项是已有 `TestSkillRelPath` 在 Windows 上固定期待 `/` 而实际返回 `\`。这些问题未在此次结构调整中修复；完整测试不能表述为全部通过。66 项跳过测试也不代表已验证通过。

验证环境为 Windows amd64、Go 1.26.8。本机 `CGO_ENABLED=0` 且未找到 GCC/Clang，本次没有执行竞态检测；涉及数据库的集成场景仍需在具备隔离测试数据库的环境验证。

### 后续范围

超过 1000 行的 Go 源文件从 9 个降为 7 个：`traffic/traffic.go`、`agent/tools.go`、`db/exploration.go`、`server/manager.go`、`db/assets.go`、`server/engine.go`、`db/config.go`。

这些文件不在本轮修改范围。后续应分别评估流量存储、工具目录、探索图谱、任务管理、资产存储、执行引擎和数据库配置的自然边界；先明确状态与事务的归属，再决定是否拆分。文件长度本身不作为必须重构的依据。

## 本地 PostgreSQL 补充验证

- 日期：2026-10-10。
- PostgreSQL / psql：16.15；程序位于 `C:\App\postgresql\pgsql\bin`。
- Docker 当时未启动，因此直接使用本地 PostgreSQL 程序。
- 新建临时数据目录，监听 `127.0.0.1:55432`；主测试使用 `artex_validation`，失败复核分别使用新建的 `artex_validation_before` 和 `artex_validation_after`。
- 仅在测试子进程中设置 `ARTEX_PG_DSN`。测试不连接原有 `data-artex` 或其他业务数据库；验证结束后已关闭临时 PostgreSQL。
- 本次继续使用 `golang-how-to`、`golang-testing`、`golang-database`，未修改业务代码或测试断言。

### 拆分与运行性能

同一 Go 包的文件会一起编译。此次保持函数和逻辑一致，没有调整数据库查询、并发、缓存、LLM 调用或其他运行策略，因此运行流畅度预计基本不变。拆分改善的是代码定位、职责划分、评审和维护，并为后续定位性能瓶颈提供更清晰的结构。

本次执行功能回归，没有进行性能基准测试，不据此承诺响应时间、吞吐量或资源占用的改善。

### 接入 PostgreSQL 后的回归结果

运行命令：

```powershell
go run ./tests/run.go --all -p 1 -json -count=1 -timeout=3m
```

`ARTEX_PG_DSN` 指向上述临时测试数据库；`-p 1` 使不同包顺序执行。命令退出码为 1，完整回归未通过。

| 范围 | 通过 | 失败 | 跳过 | 说明 |
| --- | ---: | ---: | ---: | --- |
| ARTEX 主模块 | 674 | 14 | 13 | 包含以下 db 和 server 结果 |
| db 包 | 172 | 0 | 0 | 数据库回归全部通过 |
| server 包 | 215 | 9 | 3 | 从原先 66 项跳过降至 3 项，数据库用例实际执行 |
| Norma 模块 | 656 | 4 | 8 | llm 包在一个用例中发生 panic，后续用例未全部执行 |

统计以已完成的顶层测试为单位，不包含无测试文件的包；模块总数已包含其所属包，不能重复相加。跳过不代表通过。Norma 的统计不能表述为该模块所有用例均已执行。

server 剩余 3 项跳过：`TestLiveContextReview` 缺少专用真实模型配置；`TestTaskArchivePackageSkipsSymlink`、`TestWorkspaceUploadRejectsOutsideFileLink` 因 Windows 当前用户缺少创建符号链接的权限而跳过。

### 拆分前后的失败用例复核

主模块完整执行时，server 出现 9 项失败。使用拆分前保留的两个源文件快照，通过 Go 原生 overlay 恢复原源码并隐藏 30 个新增文件；测试源码和其他代码保持一致。拆分前、拆分后分别连接一个全新的数据库，只复核这 9 项用例，避免全模块运行留下的数据影响判断。

两次结果完全一致：2 项通过、7 项失败，没有因本轮拆分产生结果差异。

| 用例 | 完整回归 | 拆分前独立复核 | 拆分后独立复核 |
| --- | --- | --- | --- |
| `TestCoreTaskLifecyclePG` | 失败：目标节点数量为 0 | 通过 | 通过 |
| `TestInheritedActivityDetailAndRelationDeletion` | 失败：临时目录清理不完整 | 通过 | 通过 |
| `TestFindingCaseGroupedTrafficExportAndInheritedAccess` | 失败 | 失败 | 失败 |
| `TestFindingTrafficAPIAndExport` | 失败 | 失败 | 失败 |
| `TestFindingTrafficArchiveV3RoundTripAndRetry` | 失败 | 失败 | 失败 |
| `TestFindingTrafficUTF8SegmentsAndInheritedWrites` | 失败 | 失败 | 失败 |
| `TestFindingWorkflowAutoHintToPlannerAndSetting` | 失败 | 失败 | 失败 |
| `TestFindingWorkflowReporterBindsBeforeWritingReport` | 失败 | 失败 | 失败 |
| `TestSkillRelPath` | 失败 | 失败 | 失败 |

前两项在干净数据库的独立复核中通过，完整执行中的失败涉及测试数据、顺序或后台任务清理干扰，具体原因仍需单独定位。不能将一次独立通过表述为已修复完整回归中的问题。

### 已定位的失败与后续事项

| 范围 | 现象与已确认原因 | 后续事项 |
| --- | --- | --- |
| evidence 4 项及 server 6 项证据相关用例 | 证据保存执行 `evidence/store.go` 中的目录 `Sync()` 时，Windows 返回 `Access is denied`。server 6 项在拆分前后均可复现。 | 单独修复目录同步的平台兼容处理，并保持持久化与失败反馈语义。 |
| server：`TestSkillRelPath` | 函数使用 `filepath.Clean`，Windows 返回反斜杠，测试固定期待正斜杠。 | 明确接口路径与本地文件路径约定，再修复实现或测试。 |
| agent：`TestGraphOverviewExpandsAssociatedCompanyScope` | 断言要求 `coverage.scope`，当前 `graphOverviewData` 未输出该字段。agent 代码不在本轮拆分范围。 | 核对企业范围上下文的接口约定和用例预期。 |
| server 的上述 2 项完整运行失败 | 干净数据库独立运行时，拆分前后均通过。 | 排查跨用例数据残留、后台任务停止及临时目录清理。 |
| Norma：`TestDoStreamHonorsConfiguredRetries` | `net/http.(*Client).do` 收到 nil client，引发空指针 panic，终止 llm 包测试。 | 核对重试测试的 HTTP client 初始化及调用约定，再执行该包剩余用例。 |
| Norma：3 项 tool 用例 | `TestBashEnvInjection` 未获得预期环境变量；`TestManagerSpawnCompleteNotifies` 子进程退出码为 1；`TestCleanupRemovesDir` 清理后目录仍存在。 | 分别检查 Windows Shell 环境、命令兼容性和进程清理。 |

这些失败尚未修复。本次数据库验证补足了原先缺少配置时的测试空白，不能据此宣布项目完整测试通过。

原始 JSON 测试日志、分包统计、拆分前后复核记录及环境信息保存在本机临时目录：

```text
C:\Users\JK\AppData\Local\Temp\artex-pg-validation-1791630748727
```

主要记录：`all-tests.jsonl`、`all-tests-summary.json`、`baseline-failures-comparison.json`、`environment-summary.json`。

