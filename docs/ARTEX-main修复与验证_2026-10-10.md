# ARTEX 修复与验证（2026-10-10）

## 线上核查

已按授权登录并只读核查任务 6、会话 119、全局模型与 LLM 录制页面。任务 6 的任务模型链指定“超实惠 / claude-fable-5.1”；会话 119 使用“默认（deepseek-中转）”，当前全局配置的模型为 glm-5.3。这解释了只改全局模型后，独立会话可以对话，而任务规划仍受原配置影响。

模型选择顺序为 Agent 角色绑定、任务模型链、全局默认。任务重启不会自动清除前两层。恢复时，应先核对 Planner 会话配置来源，再在任务配置面板更换链，或清空任务链跟随默认；有角色绑定则需在对应 Agent 设置中调整。

任务历史可见早期 model_error 和后续没有文字、没有工具调用的 completed。线上 LLM 录制关闭且列表为 0，不能还原这些回合的原始请求/响应，也不能断言空响应来自余额、接口协议或服务商解析中的哪一种问题。建议重跑前在“功能 → LLM 录制”开启开关，重跑后按任务选择 Planner 记录，检查实际模型、错误和原文，下载 JSON 审查。录制只作用于后续调用。

线上核查时显示 seconddev-9332a85；修复开发基线为 da2e26e（0.3.16）。本次未修改线上模型、录制设置或触发任务重跑。代码和本报告提交到独立修复分支，不包含发布或线上部署。

## 本地处理

| 项目 | 处理结果 |
| --- | --- |
| 规划空输出 | 整次 Prompt 没有可用文字或工具调用时记录 model_error 与具体诊断；工具写回后静默结束仍允许，旧会话产出不混入新运行判断 |
| 多个高危遗漏 | 未找到 report_finding 按资产/类型覆盖前条的确定性缺陷；增强 Planner/Worker 运行时指引，逐项跟踪独立入口和候选，分别登记漏洞、明确未测试/受阻/阴性结论 |
| A01 | 策略读取/解析失败向执行侧传递并阻断；完整加载后发布缓存，成功空规则也缓存 |
| A02 | 文件管理器通过 os.Root 完成实际 I/O，覆盖读、写、上传、下载、目录操作和删除，约束链接目标且避免单次检查后的链接替换问题 |
| A03 | 审批完成后只记一条最终 allow/block，拒绝记录保留原命令，等待审批期间不提前标为 allow |
| A04 | 解析错误中明确的上下文窗口，按较小上限恢复并保持到当前 noa Session 后续回合；排除 max_tokens 等输出预算 |
| A05 | 恢复自适应增长参数，并按压力到 emergency 的窗口余量限制恢复阈值；窗口收紧时重置旧抑制锚 |

任务详情增加配置优先级说明、下一次调用的配置来源和按任务跳转录制入口；录制页增加完整单条 JSON 下载和失败提示。

## 验证及边界

- 使用仓库 tests/run.go overlay，agent、guard、intercept 包回归通过；工作区 junction 越界读写/列表/下载/上传/删除、正常文件操作、路径穿越、根目录保护和未认证访问回归通过。任务模型链的离线切换回归通过。
- 前端 TypeScript 检查、变更页面 lint 和生产构建通过。以本地 mock 页面验证任务筛选、清除筛选、记录详情及实际 JSON 下载；下载文件包含请求、响应及原文。mock 数据不代表线上调用。
- `go run ./tests/run.go --module norma ./noa ./noaadapter -count=1` 完整回归通过。40K 窗口的部分遵从模拟峰值为 32,855 tokens；永不遵从模拟 60 回合收到 15 次提示；128K 普通压缩的最小间隔为 10 回合。这些为模拟验证，不代表真实模型保证。
- 相关包 Go vet 和 git diff --check 通过。
- 未设置 ARTEX_PG_DSN，相关 PostgreSQL 集成用例跳过；Windows 单文件符号链接用例因权限不足跳过，但目录 junction 越界回归实际执行通过。Windows chmod 无法模拟 Unix 目录不可写，相关用例在 Windows 明确跳过，Unix 原断言保留。未执行 race 检测或真实外部模型请求。
- 多漏洞指引降低过早收敛和结论混淆风险，不能保证模型完全不漏报。资产 coverage 仍是资产被事实触及的粗略比例，不等于全部漏洞机理均验证。
- 学到的上下文上限仅保留在当前 noa Session 内存，不跨新 Session 或进程持久化。未启用 noa 时不经过该恢复路径。
- 若模型始终不执行压缩且工具输出无法有效裁剪，长会话仍可能以 prompt-too-long 结束。
- docs 目录默认被仓库忽略；按本次提交要求，单独将本报告纳入版本管理，其他忽略规则保持不变。变更记录已写入 CHANGELOG 的 Unreleased。

## 提交前复核

- Agent、guard、intercept 离线回归通过；`TestCaptureApprovalLifecycle` 和 `TestWorkerReviewContextAcrossToolCalls` 必须连接 PostgreSQL，本次未提供测试数据库，使用 runner 的 `-skip` 明确排除这两项，未更改测试断言。首次完整命令因这两项数据库连接被拒绝而失败，不能宣称该命令全部通过。
- `go run ./tests/run.go ./server -run 'TestWorkspace' -count=1 -timeout=2m` 通过，包含 Windows junction 越界验证；符号链接权限限制仍按上述边界处理。
- 提交前再次运行 `noa`、`noaadapter` 完整回归，两包均通过；相关 Go 文件格式检查及 `agent`、`guard`、`intercept`、`server` 的 `go vet` 通过。
- `noa`、`noaadapter` 的 `go vet`、前端 `npx tsc --noEmit` 与 `npm run build` 再次通过。
