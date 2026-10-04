# 基础配置候选代际与原子晋升 - Independent Review

- **Review R1**（待独立评审执行）

## 评审契约

- 仓库根：`/Users/ding/Documents/swe/0910031/project-07`
- 需求：`.trae/specs/base-config-generation/spec.md`
- 任务：`.trae/specs/base-config-generation/tasks.md`
- 关键实现产物：
  - `internal/spec/loader.go`、`builder.go`（WithBaseConfigContentSources）
  - `internal/baseconfig/`（scope/errors/state/manager/submit/persis/dagsettings）
  - `internal/persis/file/baseconfig/store.go`（读路径零副作用）
  - `internal/service/frontend/api/v1/`（api.go、base_config.go）
  - `internal/service/frontend/{persistence.go,server.go}`、`internal/cmd/server.go`
- 评审者不得修改任何文件；结论写入本文件。

## Checkpoints（初始为待核）

### CP-1: AC-1 候选与 live 隔离
- **Type**: `rule`
- **Covers**: AC-1
- **Status**: Pass (R1)
- **Evidence**: 待评审独立复核（实现：submit.go 候选仅写 stateDir/candidates；live 写仅在 fence 后的 Write；`TestPromotion_SchedulerIsolationSchedulabilityAndSnapshots` 静默断言）。

### CP-2: AC-2 受影响范围与有效基配置
- **Type**: `rule`
- **Covers**: AC-2
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（loader_contentsources_test 多文档断言；submit_test 记录 (workflow, global, override)；persis_test scope 枚举）。

### CP-3: AC-3 回归拒绝可定位
- **Type**: `rule`
- **Covers**: AC-3
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（RejectionError 文本含 name/location/workspace/reason；handler 400 测试；live 摘要不变断言）。

### CP-4: AC-4 存量坏流警告非阻断
- **Type**: `rule`
- **Covers**: AC-4
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（submit_test warnings；API 200.Errors 测试）。

### CP-5: AC-5 原子晋升与代际推进
- **Type**: `rule`
- **Covers**: AC-5
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（WriteFileAtomic + generation.json 单调；state_test 两崩溃点）。

### CP-6: AC-6 晋升后可调度/新任务新代际
- **Type**: `rule`
- **Covers**: AC-6
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（scheduler_test 注册表 3 个不减少、schedule 保留、GetDetails 新 BaseConfigData）。

### CP-7: AC-7 拒绝/发布失败期间上代可用
- **Type**: `rule`
- **Covers**: AC-7
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（publish 故障 500 + 锁释放 + live 不变；拒绝期间 watcher 无事件）。

### CP-8: AC-8 并发代际收敛
- **Type**: `rule`
- **Covers**: AC-8
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（双 Manager 实例 dirlock 串行测试；fence 外部写入测试）。

### CP-9: AC-9 重启恢复
- **Type**: `rule`
- **Covers**: AC-9
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（state_test 7 用例：rename 前/后两崩溃点、损坏状态/候选、多候选按序）。

### CP-10: AC-10 快照边界
- **Type**: `rule`
- **Covers**: AC-10
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（scheduler_test：旧 attempt dag.json 与 RebuildFromYAML 得旧 schedule；新 attempt 得新代际）。

### CP-11: AC-11 既有行为面
- **Type**: `rule`
- **Covers**: AC-11
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（GET/权限/审计未改；frontend 全包测试；空配置测试；未受影响 scope 不枚举）。

### CP-12: AC-12 语法短路
- **Type**: `rule`
- **Covers**: AC-12
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（API 语法 400 时 promoter 零调用测试）。

### CP-13: AC-13 架构契合度
- **Type**: `rubric`
- **Covers**: AC-13
- **Scale**: 1-5，threshold >= 4
- **Status**: Pass (R1)
- **Evidence**: R1 独立复核通过，详见 Review History R1（diff 规模、依赖方向、复用度、三平台 lint）。

## Review History

### R1: Pass with findings（2026-10-03，全新上下文独立评审，只读）
- **总结论**：13 个 CP 全部 Pass，CP-13 = 5/5；`-race`、构建、vet、gofmt、三平台 lint 均由评审者独立执行通过；生成物零改动经 git 核对。
- **Findings**：
  - F1（minor，**已修复**）：运行期 live 与 generation.json 漂移时快照点直接 ErrFence，进程内不可自愈（手改 base.yaml 后 API 保存永久 400 至重启）。修复：快照点漂移时持锁执行 live 对账重拍快照，发布前 fence 保留。
  - F2（suggestion，**已修复**）：陈旧锁被接管后旧持有者发布前未复检所有权。修复：fence 点增加 `IsHeldByMe()` 检查，失败按 ErrFence 中止。
  - F3（minor，**已修复**）：多文档文件根文档无 workspace 标签、后续文档带标签时，工作区候选枚举漏检该文件。修复：spec 导出 `WorkspaceLabelsFromDocuments`，枚举器取全文件文档标签并集，全局/工作区 scope 统一按并集匹配；Manager 预读并集工作区覆盖。
  - F4（suggestion，**已修复**）：枚举/读取类基础设施错误也返回 "Failed to save" 文案。修复：新增校验阶段 500 错误，仅 ErrPublish 映射为保存失败。
  - F5（suggestion，**接受为已知限制**）：workspace_digests 不随工作区删除修剪；状态文件每工作区一条且极小，修剪需要额外 DAGsDir 配置，不纳入本次范围。
- 评审命令证据：`go test -race ./internal/baseconfig/...`、关键包 -count=1、`-race -count=3` 并发/栅栏/调度用例、`go vet`、`gofmt -l`、cmd/scheduler/frontend 套件、git diff 核对生成物零改动。
- 修复后由实现者复跑受影响测试并更新本文件（见下轮记录）。

### R2: Pass（2026-10-03，全新上下文，仅复核修复，只读）
- **结论**：F1/F2/F3/F4 均 **Resolved**，无新 blocker/major；`go test -race ./internal/baseconfig/...`、spec/baseconfig/api-v1 关键包、`go build ./...` 均通过；改动面与修复一致。
- **Findings**：N1（low，混合双 workspace 多文档文件缺直接 Submit 断言，**已补** `TestSubmit_PrereadsAllWorkspacesInMultiLabelSource`：alpha 候选下 bravo 文档仍取 live bravo 覆盖、baseline 取当前 alpha）；N2（low，workspace 版校验 500 无独立 handler 测试，共享映射逻辑已由全局用例覆盖，**接受**）；N3（info，非协作者在 workspace 验证期间改动其他 live 不触发目标 fence，属既有锁模型假设，**接受**）。
- 修复后 `-race ./internal/baseconfig/...` 与三平台 `make check` 均 0 issues。

## 最终结论
- 全部 13 个 CP 经两轮独立评审 Pass（CP-13 = 5/5）；R1 的 5 项 findings：4 项修复并经 R2 确认，1 项（F5）接受为已知限制；R2 的 3 项观察：1 项补测、2 项接受。
- 流水线门禁：`go build ./...`、`make check`（go fix/go fmt/golangci-lint linux+windows+darwin）、受影响包与集成套件（含 `-race`、cmd 220s、scheduler 68s、frontend 82s、intg/distr baseconfig）全部通过。
