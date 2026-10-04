# 基础配置候选代际与原子晋升 - Product Requirements Document

## Overview
- **Summary**: 全局或工作区基础配置（base config）的写接口在写盘前先形成候选代际，用候选配置把所有实际受影响的工作流按生产加载语义完整重建一遍；只有不存在"当前健康、候选下变坏"的回归时，候选才原子替换当前代际。失败时返回可定位到具体工作流的错误，调度器、手工启动与队列消费继续使用上一代。
- **Purpose**: 解决今天"配置文件语法合法、继承它的工作流却因默认执行器/参数/环境/调度字段组合而不可加载，保存后定时任务成批消失或启动失败"的问题，并保证全局与工作区覆盖并发变更时按代际顺序收敛、崩溃后只恢复最后确认的组合。
- **Target Users**: 通过 API/UI 修改全局或工作区基础配置的平台开发者（developer 及以上、对目标工作区有写权限者）。

## Goals
- 基础配置更新走"候选 → 受影响工作流重建验证 → 原子晋升"流程，验证不过不触碰 live 配置。
- 验证失败与发布失败均可定位到工作流（名称、文件位置、原因），且系统各执行链路继续使用上一代。
- 全局与工作区覆盖的并发更新按单调代际串行收敛，旧验证结果不得覆盖新发布。
- 进程在任意时刻中止后，重启只加载最后确认的全局+工作区组合，候选残留不泄漏到加载路径。
- 已入队与运行中任务沿用创建时配置快照、新任务统一使用确认后代际的既有语义不被破坏。
- 未受影响工作区、权限检查、读取接口与合法配置的正常更新保持原有行为。

## Non-Goals
- 不改变已入队/运行中任务的快照机制（attempt 的 `dag.json`、`RebuildFromYAML`、worker 内嵌 base config）。
- 不改变调度器既有的文件监听传播方式；晋升后仍由其 watcher 自然刷新注册表。
- 不提供候选的 UI 预览、人工审批流或候选代际列表查询接口。
- 不把多个目标文件（全局+多个工作区）捆绑为一次事务；每次请求只晋升一个目标，并发请求间靠代际协调串行化。
- 不引入控制面数据库或新的进程间中介；协调基于现有本地文件系统（共享文件系统是分布式部署既有前提）。
- 不修改任何 GET 接口的响应结构，不新增 OpenAPI schema 字段。
- 不改动 UI；错误沿用现有 400 文本错误展示通道。

## Background & Context
- 全局 base config 位于 `cfg.Paths.BaseConfig`；工作区覆盖位于 `<DAGsDir>/workspaces/<name>/base.yaml`，加载时在全局之上合并（[loader.go](file:///Users/ding/Documents/swe/0910031/project-07/internal/spec/loader.go#L661-L691)）。
- 写入口为 [base_config.go](file:///Users/ding/Documents/swe/0910031/project-07/internal/service/frontend/api/v1/base_config.go) 的 `UpdateBaseConfig` / `UpdateWorkspaceBaseConfig`：当前仅以 `spec.LoadYAML(WithoutEval())` 做独立语法校验，随后直接 `UpdateSpec` 写 live 文件。
- 调度器 entry reader 监听两个 live 位置并刷新注册表；带 `BuildErrors` 的 DAG 被排除（[entryreader.go](file:///Users/ding/Documents/swe/0910031/project-07/internal/persis/file/dag/entryreader.go#L444-L448)），表现为定时任务消失。其判定口径为 `OnlyMetadata + WithoutEval + SkipSchemaValidation + AllowBuildErrors` 加载后是否有 BuildErrors。
- 手工启动的加载口径为 `DAGRepository.GetDetails`（`WithoutEval`、schema 校验开启），并在 `dag.Validate()` 上把关合法工作流更新（`UpdateSpec`）。
- 文件存储 [store.go](file:///Users/ding/Documents/swe/0910031/project-07/internal/persis/file/baseconfig/store.go#L117-L130) 已用 `fileutil.WriteFileAtomic`（同目录 temp + fsync + rename + fsync 目录），单文件发布本身崩溃安全，且写后失效自身文件缓存。
- 快照事实：入队时 `BaseConfigData` 随 DAG 写入 attempt 的 `dag.json`；队列消费、retry/restart 子进程、coordinator/worker 全部以 `WithBaseConfigContent` 从快照重建；唯一有意的例外是 SMTP 会在执行时从当前 live 配置刷新。
- 前端 API 已持有 `*persis.DAGRepository`，可枚举完整 Catalog 并按 `workspace.WorkspaceFilter` 过滤；DAG 与工作区的唯一关联是 `workspace` 标签。

## Functional Requirements

- **FR-1（候选形成与隔离）**：全局或工作区基础配置更新请求先产生候选代际（记录目标范围、候选内容、所基于的已确认代际摘要），候选在验证完成前不得出现在 `cfg.Paths.BaseConfig` 与 `<DAGsDir>/workspaces/` 这两个加载/监听位置。
- **FR-2（受影响范围）**：全局候选必须用"候选全局 + 每个工作区当前已确认覆盖"（无工作区标签的 DAG 只用候选全局）重建全部工作流；工作区候选只重建带该工作区标签的工作流，使用"当前已确认全局 + 候选工作区覆盖"。多文档 YAML 文件按文档各自的 workspace 标签应用对应有效基配置。
- **FR-3（可执行可调度判据）**：每个受影响工作流在候选有效基配置下必须：①按与 `GetDetails` 一致的加载语义（`WithoutEval`、schema 校验开启）构建零错误；②通过 `ir.DAG.Validate()`（覆盖默认执行器、参数、环境、调度字段组合等）。
- **FR-4（回归门控）**：仅当存在"当前已确认代际下健康、候选下失败"的工作流时拒绝晋升；当前代际下本就失败的存量坏流不阻断，但其名称、位置与原因作为非阻断警告随成功响应返回。
- **FR-5（错误可定位）**：拒绝时 API 返回 400，错误文本逐一包含工作流名称、文件位置与失败原因；语法非法仍按现状返回 400 且不写盘；磁盘发布失败按现状返回 500 且 live 不被改动。
- **FR-6（原子晋升）**：全部受影响工作流通过后才把候选内容写入对应 live 文件（沿用既有原子写与缓存失效路径），随后推进持久化的单调代际记录；成功响应 200，警告置于既有 `errors` 字段。
- **FR-7（上代继续可用）**：从候选形成到被拒绝（或发布失败）的全过程中，调度器注册表不发生因该候选引起的增删改；手工启动、新入队与队列消费读到的仍是上一代配置。
- **FR-8（代际顺序收敛）**：全局与工作区更新并发时，"读取已确认快照→验证→晋升"全程由跨进程协调锁串行化，代际号单调递增；晋升前校验 live 摘要未偏离快照代际，使较旧候选的验证结果不可能覆盖较新的成功发布。
- **FR-9（崩溃恢复）**：候选写盘、验证、live rename、代际记录写入任一点进程中止，重启后 live 文件只呈现完整的旧代际或完整的新代际；候选残留被清理或忽略，绝不进入工作流加载路径；代际记录与 live 内容不一致时以 live 为准对账重建。
- **FR-10（快照边界）**：晋升前已入队与正在运行的任务仍用其 `dag.json` 快照配置执行；晋升后新创建的任务（手工启动、调度触发、入队）使用确认后的新代际；SMTP 的既有"执行时刷新"行为不变。
- **FR-11（行为保持）**：未受影响工作区的工作流不被重新验证或报错；权限要求（全局 developer 及以上、工作区写角色）、GET 接口响应、空配置清除、合法配置正常 200 更新、审计日志成功记录均保持现状。

## Non-Functional Requirements
- **NFR-1（正确性优先于吞吐）**：设置更新是低频管理操作，允许在持锁期间完成全量重建；重建可有限并发，但枚举与提交必须基于同一快照代际。
- **NFR-2（最小侵入）**：复用 `fileutil.WriteFileAtomic`、`dirlock`、既有 spec 加载选项与 `BaseConfigStore`；不引入新第三方依赖；尊重既有包依赖方向。
- **NFR-3（跨平台）**：文件锁与原子写在 macOS/Linux/Windows 上均可用（复用既有跨平台原语）。
- **NFR-4（可测试性）**：协调逻辑以接口（枚举器、live 读写、锁、时钟）隔离，支持用临时目录与桩件做确定性单测；端到端语义有集成级测试覆盖。

## Constraints
- **Technical**：Go 单一模块；生成物（`api/v1/api.gen.go`、UI API 类型）不得手改——本方案不触发 OpenAPI 变更；状态文件须位于 `cfg.Paths.DataDir` 下且避开 DAG 目录与 base config 监听路径。
- **Business**：本地优先、文件即状态；不得要求额外服务即可保证一致性。
- **Dependencies**：依赖既有 `persis.DAGRepository` 枚举能力、`spec` 加载/合并能力、`cmn/dirlock` 与 `cmn/fileutil`。

## Assumptions
- 同一时刻共享同一文件系统的前端实例通过文件锁协调；不支持对同一存储的非协同脑裂写入（既有部署模型相同）。
- 受影响工作流集合以调度器实际加载的 Catalog（`DAGRepository.List`）为准；alt search paths 与调度注册表口径保持一致。
- 候选验证采用与手工启动相同的确定性加载口径（`WithoutEval`），不执行运行期表达式求值；调度表达式语法、执行器/参数/环境声明等组合错误在该口径下已可被构建或 `Validate()` 捕获。
- 每次请求只改一个目标文件，因此"全局+工作区同时变化"指并发请求而非单次多文件事务。

## Acceptance Criteria

### AC-1: 候选在验证完成前与 live 加载路径隔离
- **Type**: `rule`
- **Given**: 平台开发者对全局或某工作区 base config 发起一次会被拒绝的更新请求
- **When**: 请求处理期间（候选已形成、重建验证进行中）以及请求返回 400 之后
- **Then**: 对应 live 文件字节内容与请求前完全一致；候选内容只存在于 DataDir 下的候选暂存区；调度器 entry reader 不因该候选产生任何 DAG 增删改事件
- **Pass Condition**: 进程内/文件级断言 live 摘要不变、暂存路径不在两个监听位置之内、entry reader 事件通道无新增事件
- **Evidence**: 协调器单测 + scheduler/entryreader 集成测试

### AC-2: 受影响工作流按范围与有效基配置重建
- **Type**: `rule`
- **Given**: 存在无标签 DAG、工作区 A 的 DAG、工作区 B 的 DAG（含同文件多文档不同 workspace 标签情形）
- **When**: 分别提交全局候选与工作区 A 候选
- **Then**: 全局候选重建全部三类 DAG，其中 A/B 的有效基配置为"候选全局+各自当前覆盖"，无标签 DAG 为候选全局；工作区 A 候选只重建 A 的 DAG，有效基配置为"当前全局+候选 A 覆盖"
- **Pass Condition**: 桩枚举器记录到的 (workflow, workspace, 有效基配置摘要) 集合与上述期望完全一致
- **Evidence**: 协调器单测（枚举/合并参数断言）+ spec 层内容注入加载测试

### AC-3: 候选引致回归时拒绝并定位到工作流
- **Type**: `rule`
- **Given**: 某工作流在当前代际下健康；候选基配置使其构建失败或 `Validate()` 失败（如默认执行器/参数/env/schedule 组合冲突）
- **When**: 提交候选
- **Then**: API 返回 400；错误文本包含该工作流的名称、文件位置与原因；live 文件与代际记录均不变；暂存候选被清除
- **Pass Condition**: HTTP 400 且文本可匹配到工作流名与路径；live 文件 sha256 与处理前相同；暂存目录无该候选残留
- **Evidence**: API handler 测试 + 协调器单测

### AC-4: 存量坏流不阻断且作为警告返回
- **Type**: `rule`
- **Given**: 某工作流在当前代际下已构建失败（与候选无关），且无任何"健康→失败"的回归工作流
- **When**: 提交一个在其余工作流上全部通过的候选
- **Then**: 晋升成功并返回 200；响应 `errors` 中包含可定位该存量坏流的非阻断警告；live 文件更新为候选内容
- **Pass Condition**: HTTP 200、live 内容为候选、errors 含该工作流名称与位置且不包含"拒绝"语义
- **Evidence**: API handler 测试 + 协调器单测

### AC-5: 全量通过后原子晋升并推进代际
- **Type**: `rule`
- **Given**: 所有受影响工作流在候选下健康
- **When**: 提交候选
- **Then**: 候选内容经既有原子写路径替换目标 live 文件并失效相关缓存；持久化代际记录单调 +1 且记录新内容摘要；写 live 与写代际记录之间崩溃时两边要么全旧要么可对账为全新
- **Pass Condition**: live 内容等于候选；代际号严格增加且摘要与 live 一致；模拟两点崩溃后恢复结果满足"旧或新、无中间态"
- **Evidence**: 协调器单测（含崩溃注入）

### AC-6: 晋升后工作流保持可调度、新任务使用新代际
- **Type**: `rule`
- **Given**: 候选已成功晋升
- **When**: 调度器文件监听完成一次刷新；随后手工启动/调度触发/入队一个受影响工作流
- **Then**: 注册表中受影响工作流无一因 base config 被排除、其 schedule 条目保留；新建任务加载到的 `BaseConfigData` 摘要等于新代际
- **Pass Condition**: 刷新前后注册表中受影响 DAG 集合不减少且 schedule 不丢失；新 attempt 的 `dag.json` 中 base config 摘要为新值
- **Evidence**: scheduler 集成测试（真实文件 DAGRepository + entry reader 刷新断言）

### AC-7: 拒绝与发布失败期间上一代继续服务
- **Type**: `rule`
- **Given**: 候选将被拒绝，或 live 原子写被注入故障
- **When**: 同期发生手工启动、队列消费与调度 tick
- **Then**: 三条链路均成功使用上一代配置：手工启动加载到旧 `BaseConfigData`；队列中既有任务按快照执行；调度注册表未丢失既有计划
- **Pass Condition**: 故障注入下手工启动成功且 base 摘要为旧值、队列任务执行成功、注册表条目数不减少；发布失败返回 500
- **Evidence**: 协调器/API 测试（故障注入）+ 队列快照测试

### AC-8: 并发全局/工作区更新按代际顺序收敛
- **Type**: `rule`
- **Given**: 多个客户端并发提交两个全局候选 G1/G2 与一个工作区候选 W（内容与先后顺序刻意交错，验证耗时不同）
- **When**: 全部请求结束
- **Then**: 实际生效内容等价于按某一全序串行应用各请求；代际号单调连续；最终 live 内容对应最后一个成功提交；任何先完成验证的旧候选都未覆盖后提交并已成功发布的新内容
- **Pass Condition**: 最终全局/工作区 live 摘要与按提交顺序模拟的结果一致；代际历史无回退；无"旧内容覆盖新内容"
- **Evidence**: 协调器并发单测（含跨进程第二实例持锁场景）

### AC-9: 重启只恢复最后确认组合且候选不泄漏
- **Type**: `rule`
- **Given**: 暂存区存在上次中止遗留的候选文件；live 为代际 N（覆盖"写 live 后、写代际记录前被杀"与"写代际记录后被杀"两种情形）
- **When**: 重新构造协调器并执行启动恢复
- **Then**: 加载路径可见的全局+工作区组合与代际 N 的 live 内容一致；遗留候选被清理或忽略；代际记录被对账为与 live 摘要一致；随后枚举工作流不读取任何候选内容
- **Pass Condition**: 恢复后 live 摘要、代际记录摘要、重建用基配置三者一致；暂存区被清空；重建断言不使用候选内容
- **Evidence**: 协调器恢复单测（两种崩溃点）

### AC-10: 已入队与运行中任务不受晋升影响
- **Type**: `rule`
- **Given**: 晋升发生前一个任务已入队（attempt 的 `dag.json` 已写旧 `BaseConfigData`），另一个任务正在运行
- **When**: 候选晋升为新代际后消费队列并重建该任务
- **Then**: 该任务仍按旧快照重建与执行（`RebuildFromYAML` 使用 `WithBaseConfigContent(旧)`）；`dag.json` 内容未被晋升改写；新任务则使用新代际
- **Pass Condition**: 晋升后旧 attempt 重建出的 base 配置摘要等于旧值且执行行为反映旧配置；新建 attempt 摘要为新值
- **Evidence**: intake/persis 快照测试 + RebuildFromYAML 断言

### AC-11: 既有行为面保持不变
- **Type**: `rule`
- **Given**: 现有用户与权限模型、GET 接口与合法更新流程
- **When**: 执行权限不足访问、GET 全局/工作区 base config、提交一个完全合法且不引入回归的更新、提交空配置清除、未涉及工作区的 DAG 正常加载
- **Then**: 权限判定与状态码、GET 响应字段、合法更新 200 与成功审计、空配置语义、未受影响工作区行为均与变更前一致
- **Pass Condition**: 既有相关测试不修改断言即通过；新增针对未受影响工作区"零验证零报错"的断言通过
- **Evidence**: 既有 `base_config*_test.go`、workspace 权限测试保持通过 + 新增补充测试

### AC-12: 语法非法候选被短路拒绝
- **Type**: `rule`
- **Given**: 提交的 YAML 自身无法解析
- **When**: 调用任一更新接口
- **Then**: 保持现状返回 400；不产生候选暂存文件；不枚举/重建任何工作流；live 不变
- **Pass Condition**: 400 返回、暂存区无新增、枚举器调用次数为 0
- **Evidence**: API handler 测试

### AC-13: 方案与代码库架构的契合度
- **Type**: `rubric`
- **Dimension**: 最小侵入与架构一致性
- **Scale**: 1-5
- **Anchors**: 1 = 新引入旁路存储/依赖或大范围改动执行链路；3 = 功能正确但存在重复实现或包职责错位；5 = 以小而内聚的新协调单元复用既有原子写、锁、spec 加载与存储抽象，执行链路零改动，命名与注释符合仓库规范
- **Pass Threshold**: >= 4
- **Evidence**: 代码审查（包依赖方向、复用点、diff 规模、`make fmt`/lint 结果）

## Open Questions
- 无（两个影响契约的决策已确认：存量坏流仅作非阻断警告；错误契约复用 400 文本、不改 OpenAPI）。
