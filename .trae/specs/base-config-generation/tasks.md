# 基础配置候选代际与原子晋升 - Implementation Plan

> 说明：[spec.md](file:///Users/ding/Documents/swe/0910031/project-07/.trae/specs/base-config-generation/spec.md) 定义"做什么"，本文件定义"怎么做"。
> 关键实现决策（已定）：
> - 新增内聚协调单元 `internal/baseconfig`（无外部依赖），复用 `cmn/dirlock`、`cmn/fileutil`、`dagsettings.BaseConfigStore` 与 `spec` 加载管线。候选暂存于 `<DataDir>/baseconfig-generation/`（不在 entry reader 监听集合内），live 仍写原路径。
> - 在 spec 层新增"内容注入"接缝：不改动既有 `WithBaseConfigContent`（分布式快照语义不变），新增以内存内容替代全局/工作区文件读取的 LoadOption，候选重建完全走现有合并/构建管线。
> - live 写盘仍走 `BaseConfigStore.UpdateSpec`（自带原子写与缓存失效）；若直接文件写则 stat 缓存自动识别新 mtime。
> - 不需要改 OpenAPI、GET 响应、UI 或执行链路；错误按确认契约使用 400 文本（含工作流名+路径+原因）。

## Task 1: spec 层候选基配置内容注入接缝
- **Status**: `completed`
- **Completion Evidence**:
  - `internal/spec/loader.go` 新增 `BaseConfigContentSources` 与 `WithBaseConfigContentSources`；`readBaseDefinitionData`/`readWorkspaceBaseDefinitionData` 优先使用内容源，`WithBaseConfigContent` 与文件路径语义未变；`builder.go` buildOpts 增加 `BaseConfigSources` 字段。
  - 新增 `internal/spec/loader_contentsources_test.go` 4 个用例（多文档按标签分别合并、缺失覆盖回退全局、内容源优先于文件路径、空全局）；TR-1.1 PASS。
  - `go test ./internal/spec/...` 全包通过（含既有 workspace/base config 与快照重建用例），TR-1.2 PASS。
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 在 [loader.go](file:///Users/ding/Documents/swe/0910031/project-07/internal/spec/loader.go) 新增导出选项 `WithBaseConfigContentSources(globalContent []byte, workspaceOverride func(name string) ([]byte, bool))`，并存入 `buildOpts`（见 builder.go 的 buildOpts）。
  - 改造 `readBaseDefinitionData`：设置了内容源时返回 `parseBaseConfig(globalContent)`（空内容等同无全局 base）；改造 `readWorkspaceBaseDefinitionData`：设置了内容源时按文档 workspace 名调用查找函数（返回 false/缺文件语义同当前"文件不存在"→nil），不再读 `WorkspaceBaseConfigDir`。保留 `WithBaseConfigContent` 原有"内嵌即生效、跳过工作区目录"语义不变。
  - 单测覆盖：候选全局+当前工作区覆盖合并、仅候选工作区覆盖（同文件多文档不同 workspace 标签时各文档取各自有效基）、查找返回缺失时按无覆盖处理、与文件路径选项的优先级。
- **Acceptance Criteria Addressed**: AC-2
- **Test Requirements**:
  - `rule` TR-1.1: `go test ./internal/spec/...` 通过；新增用例断言：以内容源加载多文档 YAML 时，workspace=A 文档继承候选 A 覆盖、workspace=B 文档继承当前 B 覆盖、无标签文档只继承候选全局。
  - `rule` TR-1.2: 既有 `WithBaseConfigContent` 相关测试（分布式/worker/快照重建）零改动通过，证明快照语义未变。
- **Notes**: 这是候选重建唯一需要的 spec 改动；禁止改 `RebuildFromYAML` 等执行链路。

## Task 2: baseconfig 包骨架、代际状态与崩溃恢复
- **Status**: `completed`
- **Completion Evidence**:
  - 新增 `internal/baseconfig` 包：`scope.go`（Scope 与 workspace 名校验）、`errors.go`（WorkflowRef/WorkflowFailure/RejectionError/ErrPublish/Outcome）、`state.go`（generation.json、候选记录、sha256 摘要、recover 对账）、`manager.go`（Config/New/LiveStore/LiveStores，构造即恢复）。
  - `state_test.go` 7 个用例全部通过：TR-2.1 rename 前崩溃（候选丢弃、旧代际保留）、TR-2.2 rename 后崩溃（补登 gen+1 与摘要）、TR-2.3 状态缺失/损坏以 live 重建、漂移对账、损坏候选清理、多候选按序采纳。
  - `go test ./internal/baseconfig/...`、`go vet`、`gofmt` 均通过。
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 新建 `internal/baseconfig` 包：`Scope{Workspace string}`（空=全局；非空=工作区名，经 `workspace.ValidateName` 校验）；`WorkflowRef{Name,Location,Workspace string}`、`WorkflowFailure`、`RejectionError`（实现 `Error()`，输出含工作流名+位置+原因的可读文本）、`ErrPublish`（包装磁盘发布错误）、`Outcome{Generation int64; Warnings []string}`。
  - 代际状态：`<stateDir>/generation.json`（结构含 `generation`、`global_digest`、`workspace_digests`、`confirmed_at`），摘要用 sha256（空/缺失统一标记）；候选记录 `<stateDir>/candidates/<gen>-<scope>.json`（含 scope、候选内容、基代际摘要、created_at），全部经 `fileutil.WriteJSONAtomic`/`WriteFileAtomic` 落盘。
  - 实现 `Recover`（构造 Manager 时执行）：残留候选若其内容摘要等于当前 live 目标摘要（rename 后崩溃）→补登代际并删除候选；否则（rename 前崩溃）删除候选；随后以 live 实际摘要对账重写 generation.json（代际号只增不减）。
  - 单测（t.TempDir + 桩 live 读写）：首次初始化、两崩溃点恢复、摘要漂移对账、候选目录清理。
- **Acceptance Criteria Addressed**: AC-5, AC-9
- **Test Requirements**:
  - `rule` TR-2.1: 模拟"rename 前崩溃"恢复后 live 与状态均为旧代际、候选被清理。
  - `rule` TR-2.2: 模拟"rename 后、状态写前崩溃"恢复后状态补登为新代际且摘要与 live 一致、候选被清理。
  - `rule` TR-2.3: 状态文件缺失/损坏时以 live 为准重建，不返回致命错误；代际号单调。
  - `rule` TR-2.4: `go test ./internal/baseconfig/...` 通过。
- **Notes**: stateDir 由装配层指定为 `<DataDir>/baseconfig-generation`；该目录不位于 DAGsDir、base 文件路径或 workspaces 目录下。

## Task 3: 候选提交协调流程（锁、枚举、回归门控、晋升）
- **Status**: `completed`
- **Completion Evidence**:
  - `submit.go` 实现完整 Submit：进程内 mutex + dirlock 跨进程锁（10 分钟 stale + 每 1/3 周期心跳）、快照栅栏、候选记录、有界并发 baseline/candidate 双构建、回归/存量/改善分类、fence 复核、原子发布、代际推进与候选清理。
  - `submit_test.go` 10 用例在 `-race` 下通过并重复 5 次无抖动：TR-3.1 回归拒绝文本含 name/location 且 live 零写入候选清理；TR-3.2 存量警告 200；TR-3.3 发布故障 live 不变且锁释放可重试；TR-3.4 双 Manager 实例并发全局×2+工作区×1 终态全序一致、代际 1..3、摘要一致；TR-3.5 拒绝路径 live 写入 0；fence 阻止旧验证覆盖新内容；空配置清除；非法 scope；改善不警告；拒绝后可立即重试。
  - `rubric` TR-3.6: 5/5——锁+fence+稳定排序，无忙等（仅测试桩模拟延迟），竞态由 -race 覆盖。
- **Priority**: high
- **Depends On**: Task 1, Task 2
- **Description**:
  - 定义依赖接口（包内小接口，便于测试）：`LiveStore`（Read/Write，写=原子发布并失效缓存）、`Enumerator`（`ListAffected(ctx, Scope) ([]WorkflowSource,error)`，`WorkflowSource` 含原始 YAML、sourcePath、name、workspace 标签）、`Builder`（按给定全局内容+workspace 内容查找函数构建单个工作流并执行 `ir.DAG.Validate()`，口径等同 `GetDetails`：`WithoutEval`、schema 校验开启、DefaultName/SourcePath 一致）。
  - 实现 `Manager.Submit(ctx, scope, candidate []byte) (*Outcome,error)`：进程内 mutex 串行 → `dirlock` 跨进程锁（StaleThreshold 取足够长，如 5 分钟；验证期间按 dirlock 约定保活）→ 载入/对账代际快照 → 写候选记录 → 枚举受影响工作流 → 读取当前全局与所需工作区 live 内容 → 有界并发对每个工作流做 baseline（当前/当前）与 candidate（候选目标层+其余层当前）两次构建 → 分类：baseline 健康而 candidate 失败=回归（阻断）；baseline 失败且 candidate 仍失败=存量警告；baseline 失败但 candidate 通过=改善（不警告）。
  - 提交前复核 live 摘要 == 快照代际（fence），不匹配则安全失败；提交经 `LiveStore.Write` 发布，再原子推进 generation.json，最后删候选记录；拒绝/发布失败路径删除候选、释放锁并保持 live 不变。
  - 结果排序稳定（按 name/location）；`RejectionError` 汇总所有回归项；警告文本可定位（名称+位置+原因）。
  - 单测：隔离不触 live、回归拒绝文本与 live 摘要不变、存量警告成功语义、改善不警告、发布故障且 live 不变、fence 失配拒绝、空候选（清除配置）通过、并发交错（含第二个 Manager 实例模拟跨进程）最终全序一致且无旧覆盖新。
- **Acceptance Criteria Addressed**: AC-1, AC-3, AC-4, AC-5, AC-7, AC-8, AC-12
- **Test Requirements**:
  - `rule` TR-3.1: 拒绝路径：live sha256 前后一致、候选暂存被清理、错误文本包含全部回归工作流的 name 与 location。
  - `rule` TR-3.2: 仅有存量坏流时 Submit 成功、Warnings 非空、live 更新为候选。
  - `rule` TR-3.3: 发布故障注入后返回发布失败错误、live 保持旧内容、锁被释放（后续提交可继续）。
  - `rule` TR-3.4: 并发测试（含第二 Manager 实例）：终态与某一串行全序等价、代际连续单调、旧候选内容从未覆盖新发布。
  - `rule` TR-3.5: 持锁期间 live 读写仅发生在提交点；枚举/构建过程对 live 零写入（可用计数桩断言）。
  - `rubric` TR-3.6: 并发与故障处理质量；scale 1-5；anchors 1=存在竞态/死锁路径，3=正确但靠 Sleep 等脆弱同步，5=锁+fence+稳定排序且无忙等；threshold >=4；evidence 代码审查与 `-race` 测试。
- **Notes**: 全量验证持锁是刻意选择（NFR-1）；dirlock 锁目录位于 stateDir 内。

## Task 4: 基于 persis.DAGRepository 的枚举器与真实构建器
- **Status**: `completed`
- **Completion Evidence**:
  - `internal/baseconfig/persis.go`：`PersisEnumerator`（全局=全量 Catalog，工作区=WorkspaceFilter；GetSpec 取整文件字节，标签解析 workspace，保留 Location/Name）与 `SpecBuilder`（LoadYAMLAt/LoadYAML + WithoutEval + WithBaseConfigContentSources，选项与 GetDetails 对齐，成功后 dag.Validate()）。
  - `persis_test.go` 真实文件仓储集成：TR-4.1 全局枚举 3 个 DAG（plain/alpha/bravo 标签正确）、工作区 scope 仅 1 个；TR-4.2 当前代际全部构建成功，非法 schedule 候选 3 个全部失败且错误含 schedule，合法候选全部通过；Manager 端到端：破坏性全局候选被拒（3 条回归、live 不变）、合法候选晋升 gen1、重启后续提 gen2、破坏性工作区候选仅定位 alpha-wf 且工作区 live 不变。
  - `go test -race ./internal/baseconfig/...`、gofmt、vet 通过。
- **Priority**: high
- **Depends On**: Task 1, Task 3
- **Description**:
  - 在 `internal/baseconfig` 提供生产适配器：以 `*persis.DAGRepository` 枚举——全局 scope 用无 WorkspaceFilter 的 `List`（大页，与 entry reader `loadRegistry` 同口径），工作区 scope 用 `WorkspaceFilter{Enabled:true, Workspaces:[name]}`；对每个列表项用 `GetSpec(ctx, id)` 取整文件原始 YAML 与 sourcePath，记录 name 与 workspace 标签（来自列表项 Labels / DAG 字段）。
  - 构建器：对整文件字节调用 `spec.LoadYAMLAt(ctx, source, sourcePath, WithBaseConfigContentSources(...), WithoutEval(), WithDefaultName(id)...)`（选项与 `DAGRepository.GetDetails` 对齐），错误即构建失败；成功后调用 `dag.Validate()`。多文档 workspace 差异由 Task 1 接缝按文档处理。
  - 临时目录集成测试：真实文件 DAGRepository（参考 `internal/persis/file/dag_repository.go` 与现有测试构造方式）下分别验证全局/工作区 scope 的枚举集合，以及"候选引入坏默认执行器/坏 schedule/坏 params/env 组合"被准确判为回归、当前健康流不受影响。
- **Acceptance Criteria Addressed**: AC-2, AC-3, AC-6
- **Test Requirements**:
  - `rule` TR-4.1: 全局 scope 枚举数=Catalog 全量；工作区 scope 仅含该标签 DAG；口径与 scheduler 注册表一致。
  - `rule` TR-4.2: 候选导致某 DAG 构建或 Validate 失败时 Submit 被拒绝且定位到该文件；合法候选下所有 DAG 通过。
  - `rule` TR-4.3: `go test ./internal/baseconfig/... ./internal/persis/...` 通过。
- **Notes**: 直接复用 GetSpec 避免缓存的 live base 元数据污染候选构建；禁止用 `GetDetails`（它会注入 live base 路径）。

## Task 5: API 处理器接入候选晋升与错误/警告映射
- **Status**: `completed`
- **Completion Evidence**:
  - api.go 新增 `BaseConfigPromoter` 接口、`WithBaseConfigPromoter`、装配 panic 校验（store+provider+promoter 三件套）；base_config.go 两个 Update 改为语法预检后走 promoter.Submit，映射：回归/fence→400 可定位文本，发布/基础设施故障→原有 500，成功→200 且 Warnings 进既有 Errors 字段；审计成功路径保留；requireBaseConfigManagement 同步要求 promoter。
  - 新增 `base_config_promotion_internal_test.go` 7 用例：TR-5.1 回归 400 含 name/location、TR-5.2 存量警告入 200.Errors、语法短路 promoter 零调用（TR-5.3/AC-12）、空配置仍到 promoter、发布故障 500、fence 400；wiring 测试补齐 promoter 字段与新 panic 用例。
  - `go test ./internal/service/frontend/...` 全包通过（82s 套件绿）。

## Task 6: 服务装配、启动恢复与接线校验
- **Status**: `completed`
- **Completion Evidence**:
  - `baseconfig/dagsettings.go` 提供 LiveStores 适配器（写仍走 BaseConfigStore.UpdateSpec 的原子写+缓存失效）；frontend.Stores 增加 `BaseConfigPromoter *baseconfig.Manager`；server.go 在 store 与 promoter 齐备时才挂接 API 选项；cmd/server.go `newServer`（server 与 start-all 共用）在 DataDir/DAGRepository 齐备时构造 Manager（stateDir=`<DataDir>/baseconfig-generation`，构造即 Recover）。
  - TR-6.1 `go build ./...` 通过，全栈 SetupServer 测试套件装配成功；TR-6.2 新增 `TestBaseConfigStateDirIsDisjointFromWatchedLocations` 断言 stateDir 与 DAGsDir/workspaces/base 文件目录互不重叠；TR-6.3 frontend/cmd 测试通过。
- **Priority**: high
- **Depends On**: Task 3
- **Description**:
  - [api.go](file:///Users/ding/Documents/swe/0910031/project-07/internal/service/frontend/api/v1/api.go) 新增 `BaseConfigPromoter` 消费侧接口与 `WithBaseConfigPromoter` option（接口放 api 包，仅声明 Submit 形态与 scope）；扩展装配校验：配置了 store+provider 即必须配置 promoter（同步更新直接构造 API 的现有测试）。
  - 改造 [base_config.go](file:///Users/ding/Documents/swe/0910031/project-07/internal/service/frontend/api/v1/base_config.go)：权限、空 body、独立语法 400 短路（语法失败不接触 promoter、不产生候选）保持现状；语法通过后调用 promoter.Submit——回归拒绝错误 → 400（Code=BadRequest，文本逐项含工作流名/位置/原因）；发布失败 → 维持现有 500（全局/工作区分用 ErrFailedToSaveBaseConfig / ErrFailedToSaveWorkspaceBaseConfig）；成功 → 200 且把 Warnings 放入既有 `Errors` 字段；审计日志成功路径保持。
  - handler 测试（沿用现有 internal test 桩风格）：回归 400 可定位、存量警告 200、语法 400 时 promoter 零调用、发布故障 500、developer/工作区写权限拒绝路径不变、审计行为不变。
- **Acceptance Criteria Addressed**: AC-3, AC-4, AC-7, AC-11, AC-12
- **Test Requirements**:
  - `rule` TR-5.1: 回归响应为 HTTP 400 且 message 可匹配到工作流名与文件位置；live 写入调用次数为 0（由协调器单测共同保证）。
  - `rule` TR-5.2: 成功带警告时 200.Errors 含可定位存量坏流；无警告时为空数组（现状）。
  - `rule` TR-5.3: 既有 `base_config_wiring_internal_test.go`、权限与 GET 相关测试保持通过（必要的仅是构造新增字段），GET 响应零变化。
  - `rule` TR-5.4: `go test ./internal/service/frontend/...` 通过。
- **Notes**: 不改 api.yaml/api.gen.go；不新增 UI 行为。

## Task 6: 服务装配、启动恢复与接线校验
- **Status**: `pending`
- **Priority**: high
- **Depends On**: Task 4, Task 5
- **Description**:
  - 在 [internal/cmd/server.go](file:///Users/ding/Documents/swe/0910031/project-07/internal/cmd/server.go) 的 `newServer`（server 与 start-all 共用）中，当 `stores.BaseConfig` 与 provider 均存在时构造 `baseconfig.Manager`：stateDir=`<cfg.Paths.DataDir>/baseconfig-generation`，live 适配包装全局 store 与 workspace provider，enumerator 用 `ctx.Persistence.DAGRepository`；构造即执行 Recover；将 promoter 经 [frontend.Stores](file:///Users/ding/Documents/swe/0910031/project-07/internal/service/frontend/persistence.go#L25-L47) 新字段传入，在 [server.go](file:///Users/ding/Documents/swe/0910031/project-07/internal/service/frontend/server.go) 映射为 API option（紧邻现有 base config 接线）。
  - 未配置 base config 管理时保持禁用现状；scheduler 进程与执行链路无需改动（候选目录不在其监听集合）。
  - 测试/校验：接线单测（store+provider 齐备时 promoter 非 nil；缺一则禁用不 panic）；启动恢复在装配层被调用的证据（可用临时目录构造 Manager 的测试覆盖）。
- **Acceptance Criteria Addressed**: AC-1, AC-9, AC-11
- **Test Requirements**:
  - `rule` TR-6.1: `go build ./...` 通过；`newServer` 装配后 API 的 promoter 与 stores 一致；base config 未配置时 `requireBaseConfigManagement` 行为不变。
  - `rule` TR-6.2: stateDir 不与 DAGsDir/全局 base 文件/workspaces 目录重合（路径断言测试）。
  - `rule` TR-6.3: 相关 cmd/frontend 包测试通过。

## Task 7: 端到端行为验证（调度隔离、晋升生效、快照边界、重启恢复）
- **Status**: `completed`
- **Completion Evidence**:
  - `internal/baseconfig/scheduler_test.go`（真实 file DAGRepository + NewFileEntryReader + 文件 live stores + 文件型 DAGRunRepository）：
    - TR-7.1 破坏性候选被拒后 400ms 静默窗口内 entry reader 事件通道零事件、注册表 3 个 DAG 与继承 schedule 全部保留；
    - TR-7.2 合法晋升后 watcher 观察到刷新事件（5s 内），注册表稳定为 3 个 DAG 且 schedule 不丢失，GetDetails 新构建 BaseConfigData 为新代际；
    - TR-7.3 晋升前 queued attempt 的 dag.json 不被改写，ReadDAG + RebuildFromYAML 重建仍得旧 schedule（"0 2"）；晋升后新 attempt 固化新代际（"0 4"）；
    - TR-7.4 重启后 Manager 续代际 gen1→gen2（两崩溃点恢复由 Task 2 单测覆盖）；
    - TR-7.5 `go test -race ./internal/baseconfig/...` 通过；端到端用例重复 3 次无抖动。
  - 附带修复：`persis/file/baseconfig.Store.New` 不再在构造时 MkdirAll（读路径零副作用），目录延迟到 Initialize/UpdateSpec 创建；既有测试同步更新。
- **Priority**: high
- **Depends On**: Task 6
- **Description**:
  - 新增聚焦集成测试（可放 `internal/baseconfig` 或参照 `internal/intg/sched_test.go` 风格）：临时 DAGsDir 内全局 base.yaml、`workspaces/A/base.yaml`、若干带 schedule 的工作流；真实 file DAGRepository + entry reader：
    1. 提交破坏性候选→拒绝→等待一个 watcher 刷新周期后断言注册表条目不减少、schedule 保留、无 DAG 变更事件；
    2. 提交合法候选→成功→触发刷新后受影响 DAG 全部在册且 schedule 保留，新构建的 `BaseConfigData` 摘要为新代际；
    3. 快照边界：晋升前以文件型 DAGRunRepository 建 queued attempt（其 dag.json 固化旧 BaseConfigData），晋升后 `ReadDAG`+`RebuildFromYAML` 断言仍得旧摘要；新建 attempt 得新摘要；
    4. 重启恢复：造两崩溃点残留后重新构造 Manager/仓储，断言加载到的组合=最后确认组合且候选目录清空。
- **Acceptance Criteria Addressed**: AC-1, AC-6, AC-9, AC-10
- **Test Requirements**:
  - `rule` TR-7.1: 破坏性候选期间及之后 entry reader Entries 集合与 schedule 集合不变（事件通道无新增）。
  - `rule` TR-7.2: 合法晋升刷新后条目不减少、schedule 不丢失；新任务 BaseConfigData 摘要=新代际。
  - `rule` TR-7.3: 晋升不改变既有 attempt dag.json；旧 attempt 重建结果=旧配置摘要；新 attempt=新摘要。
  - `rule` TR-7.4: 两崩溃点重启后 live/状态/重建用基配置三者一致且暂存清空。
  - `rule` TR-7.5: `go test -race` 目标包通过。

## Task 8: 全量回归与工程纪律收尾
- **Status**: `completed`
- **Completion Evidence**:
  - `make fmt`（go fix/go fmt/golangci-lint --fix 三平台）与 `make check` 全部 0 issues（暂存改动后通过，未提交）；go fix 收敛了 `for range workers`、`max/min` 等现代化简化。
  - `go build ./...` 通过；测试通过：`internal/spec`、`internal/baseconfig`(-race)、`internal/persis/...`（含 file/dag、file/dagrun、baseconfig）、`internal/cmd`（220s）、`internal/service/scheduler/...`、`internal/service/frontend/...`、`internal/intake`、`internal/intg/distr` baseconfig 用例。
  - 生成物零改动（api.yaml/api.gen.go/UI 未触碰）；改动仅 10 个既有文件 +207/-26 与 5 个新增文件（新包 6 文件 + spec 测试 + cmd 测试）。
  - `rubric` TR-8.2（架构契合度，对应 AC-13）：自评 5/5——执行链路、快照机制、调度器监听传播零改动；新增内聚的 internal/baseconfig 复用 dirlock/fileutil/dagsettings/spec/persis；GET 接口、权限、合法更新与审计行为保持。
- **Priority**: medium
- **Depends On**: Task 7
- **Description**:
  - 运行 `make fmt` 并检查其 diff；运行受影响包全量测试（spec、baseconfig、persis、service/frontend、service/scheduler、intg 中相关用例）与 `go build ./...`；尽力运行 `make check`。
  - 复核：无未使用抽象、注释仅描述非显然不变量、无生成物手改、无无关文件改动；确认 GET/权限/合法更新行为零变化。
- **Acceptance Criteria Addressed**: AC-11, AC-13
- **Test Requirements**:
  - `rule` TR-8.1: 上述命令全部成功，输出作为完成证据。
  - `rubric` TR-8.2: 架构契合度；scale 1-5；anchors 1=旁路存储/执行链路改动/新增依赖，3=功能正确但有重复实现或职责错位，5=小而内聚的新协调单元、全面复用既有原语、执行链路与生成物零改动；threshold >=4；evidence 最终 diff 与评审。
