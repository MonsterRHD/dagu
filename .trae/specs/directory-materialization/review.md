# 目录树路径物化 - Independent Review

- [x] CP-R1: 声明解析、规划依赖与端到端运行行为正确
  - **Type**: `rule`
  - **Covers**: AC-1, AC-12
  - **Evidence**: `TestLoadBuildStepDirectoryPathTypes`、`TestPrepareBuildPlanInfersFileDependency`、`TestPrepareBuildPlanInfersOverlappingDirectoryDependencies`、`TestPrepareBuildPlanDependsOnAllOverlappingProducers`、`TestBuildRunnerProducesAndConsumesDirectory` 通过；`go test ./internal/spec/... ./internal/runtime/...` 通过。

- [x] CP-R2: 目录快照、稳定指纹与符号链接边界正确
  - **Type**: `rule`
  - **Covers**: AC-2, AC-3
  - **Evidence**: `TestSnapshotDirectoryIsStableAndComplete`、`TestSnapshotDirectoryDetectsTreeChanges`、`TestSnapshotDirectoryRejectsUnsafeSymlinksAndSpecialFiles`、`TestSnapshotForPublicationValidatesFinalSymlinkRoot` 通过；覆盖相对路径、内容、空目录、类型、链接目标、逃逸链接、链接环和命名管道。

- [x] CP-R3: 暂存、复用、失效与既有决策语义保持
  - **Type**: `rule`
  - **Covers**: AC-4, AC-5, AC-6, AC-7, AC-10
  - **Evidence**: `TestNewAttemptCreatesDirectoryStaging`、`TestPrepareCommitAndReuseDirectory`、`TestCommitDirectoryRejectsChangedInputAndWrongStagingType`、`TestDirectoryFileTypeSwitchCommitsAndReuses`、`TestDirectoryDryRunNoReuseAndControlDependency`、`TestBuildRepeatRemovesUncommittedStagingDirectory` 通过；既有文件物化测试继续通过。

- [x] CP-R4: journal 发布、崩溃恢复、锁扩展与旧 manifest 兼容正确
  - **Type**: `rule`
  - **Covers**: AC-8, AC-9, AC-11
  - **Evidence**: `TestCommitPublishesDirectoryAndVersionTwoManifest`、`TestRecoverDirectoryExchangeCommit`、`TestCommitPublishesDirectoryWithFinalAbsoluteSymlink`、`TestCommitRollbackPreservesOldTreeWithAbsoluteSymlink`、`TestFailedFirstDirectoryCommitCanBeRetriedWithBrokenSymlink`、`TestAcquirePathsRecoversCommittedDirectoryJournal`、`TestAcquirePathsExpandsDirectoryLocksAndCleansStaging`、`TestGetAcceptsVersionOneFileManifest` 通过。

- [x] CP-U1: 实现结构、跨平台边界与可维护性满足要求
  - **Type**: `rubric`
  - **Covers**: AC-13
  - **Scale**: 1-5
  - **Anchors**: 1 = 目录遍历、平台交换和恢复状态耦合；3 = 功能可用但回退路径分散；5 = 快照、会话、journal、平台交换、锁扩展和 runtime 集成边界清晰并有回归测试。
  - **Pass Threshold**: >= 4
  - **Evidence**: 4/5。树快照集中在 `internal/build/tree.go`，发布/回滚集中在 `tree_store.go`，平台原语按 OS 分文件；`make fmt`、Windows/Linux 交叉编译、`go test -race` 和 `make conformance` 通过。

## Review History

### Review R1
- **Result**: `fail`
- **Evidence**: 独立审查发现 3 个 Major：父目录输入只依赖一个重叠生产者；暂存树绝对符号链接发布后悬空；目录排他恢复早于后代锁获取。
- **Findings**:
  - R1-M1: `actionable`; Major；多个不相交目录生产者与父目录消费者之间只添加随机一条 inferred edge。
  - R1-M2: `actionable`; Major；暂存根内绝对符号链接经 rename/exchange 发布后可能悬空，且发布后缺少 final root 复验。
  - R1-M3: `actionable`; Major；排他 journal recovery 在后代锁全部获取前执行。
  - R1-m1/m2: `advisory`; Minor；瞬时恢复错误删除 journal、树成员未逐个 fsync。
- **Remediation**:
  - R1-M1: 改为收集全部重叠生产者并排序添加依赖，新增 `TestPrepareBuildPlanDependsOnAllOverlappingProducers`。
  - R1-M2: 新增 `SnapshotForPublication` 和发布后 final root 快照验证，新增 final absolute symlink 测试。
  - R1-M3: 排他恢复移至全部扩展锁稳定后；共享锁通过完整排他 AcquirePaths 恢复。

### Review R2
- **Result**: `fail`
- **Evidence**: 复审确认 R1-M1/R1-M3 主路径正确，但发现符号链接修复与回滚/恢复交互存在 2 个 Major。
- **Findings**:
  - R2-M1: `actionable`; Major；含 final-绝对符号链接的旧树在 staging/backup 位置被 strict snapshot 拒绝，导致回滚失败。
  - R2-M2: `actionable`; Major；首次发布悬挂绝对链接时 `os.IsNotExist` 无法识别包装后的成员缺失错误，回滚失败并可能卡死后续提交。
  - R2-m1: `advisory`; Minor；共享锁恢复路径需要完整扩展锁验证；缺少 AcquirePaths 级树 journal 测试。
  - R2-m2: `advisory`; Minor；changed-while-hashing 使用错误字符串判断。
- **Remediation**:
  - staging/backup 身份识别改用 `SnapshotForPublication(actual, finalPath)`。
  - live rollback 使用 `force=true`，崩溃恢复使用 `force=false` 保留未知 final 保护。
  - 首次发布坏树 live rollback 可清理，健康提交可重试。
  - 新增 `TestCommitRollbackPreservesOldTreeWithAbsoluteSymlink`、`TestFailedFirstDirectoryCommitCanBeRetriedWithBrokenSymlink`、`TestAcquirePathsRecoversCommittedDirectoryJournal`。
  - `errPathChanged` 改为 sentinel wrapping；共享锁通过完整排他 AcquirePaths 恢复。

### Review R3
- **Result**: `pass`
- **Evidence**:
  - `go test ./internal/build/... ./internal/persis/file/materialization/... ./internal/runtime/... ./internal/spec/... ./internal/ir/...` 通过。
  - `go test -race -count=1 ./internal/build/... ./internal/persis/file/materialization/...` 通过。
  - Windows/Linux 测试二进制交叉编译通过。
  - `make fmt` 报告 Go/Windows/darwin linter 0 issues；`make conformance` 通过。
  - 独立审查确认第二轮两个 Major 均关闭；live rollback 可清理本次未确认坏树，crash recovery 对无法识别的 final 保持保守不覆盖。
- **Checkpoint Results**:
  - CP-R1 (`rule`): `pass`
  - CP-R2 (`rule`): `pass`
  - CP-R3 (`rule`): `pass`
  - CP-R4 (`rule`): `pass`
  - CP-U1 (`rubric`): `pass`; score 4/5；平台与恢复职责清晰，剩余 fsync 大规模树成员和锁文件扇入属于后续运维增强，不阻塞本需求。
- **Findings**: 无 Critical、Major 或 actionable Minor。
