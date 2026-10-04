# DAG 重命名可恢复单次发布 实现计划

## Repository Research

### 当前重命名链路（问题所在）

- Web API：[dags.go](file:///Users/ding/Documents/swe/0910031/project-09/internal/service/frontend/api/v1/dags.go#L372-L424) 的 `RenameDAG`
  1. 校验新名称、读取元数据（404）、工作区写权限、运行中拒绝（400）；
  2. `dagRepository.Rename` 直接 `os.Rename` 定义文件并失效索引（[store.go](file:///Users/ding/Documents/swe/0910031/project-09/internal/persis/file/dag/store.go#L509-L534)）；
  3. `migrateDAGSettingsAfterRename`（[dagsettings.go](file:///Users/ding/Documents/swe/0910031/project-09/internal/service/frontend/api/v1/dagsettings.go#L271-L305)）迁移运行配置（runtime profile 选择），**任何失败只打日志**；
  4. 写审计并返回 200；**当前完全不发变更通知**（`notifyDAGMutation` 未被调用）。
- MCP：[server.go](file:///Users/ding/Documents/swe/0910031/project-09/internal/service/mcp/server.go#L480-L496) 的 `renameDAG` 直接调用同一个 API handler，preview 只做读校验（[change_tool.go](file:///Users/ding/Documents/swe/0910031/project-09/internal/service/mcp/change_tool.go#L340-L370)）。修 API 即同时修 MCP。
- 运行配置 = DAG settings（`dagsettings.Store`，文件 JSON 集合 `dag-settings`）；迁移即「新名 Upsert + 旧名 Delete」，目前非原子且失败被吞。
- 定义存储：YAML 文件在 `DAGsDir`（可能只读/外部 symlink），索引在 `DataDir/cache/dag-index`，读时按文件元数据自愈重建。
- 其他事实：
  - 重命名唯一调用方是该 API handler；历史运行按 DAG 名归属（`dag-runs` 等），本次不移动。
  - 文件操作已有耐久原语：`fileutil.WriteFileAtomic`、`ReplaceFileDurable`、`RemoveFileDurable`、`SyncDir`、带 Windows 重试的 `fileutil.Rename`。
  - 跨进程锁 `cmn/dirlock`（文件 Collection 已在用）。
  - 前端服务统一从 `frontend.NewServer` 构造（`dagu server` / `start-all` 同路径），配置必有 `DataDir`（test helper 也设置）；`NewServer` 末尾构造 `apiV1`，`Serve` 才开始监听——是启动恢复的合适位置。
  - 错误映射已具备：`ErrDAGAlreadyExists→409`、`ErrDAGReadOnly→403`、`ErrDAGNotFound→404`（[api.go](file:///Users/ding/Documents/swe/0910031/project-09/internal/service/frontend/api/v1/api.go#L799-L818)）。
  - suspend flag 目前也不随重命名迁移——属既有行为，本次不动。

### 目标语义（用户要求拆解）

1. 目标名称、工作区权限、只读来源、运行状态全部先校验（规则与现状完全一致）。
2. 候选定义信息与运行配置**先暂存准备**，再一次性切换：切换点 = 定义文件的一次耐久 rename（同文件系统原子更名，不产生两个可编辑定义）。
3. 切换前任一步失败或进程中止：旧名定义与旧配置原样可用，暂存物清理。
4. 切换后失败/中止：新名已耐久化，旧配置在新名确认前保留；重启或重复请求时前向收敛（补 settings、补通知/审计、删旧配置），最终唯一名、唯一配置。
5. 新名确认持久化后才删旧配置，并且整次发布只发一次变更通知、一次成功审计、MCP 一次成功结果。
6. 重复请求识别：已完成（幂等成功，不再审计/通知）或仍在恢复（先收敛再返回成功）。
7. 历史运行归属、外部只读 DAG 拒绝、目标冲突、工作区权限、运行中拒绝规则不变。

## Files and Modules

- `internal/persis/file/dag/rename.go`（新建）：拆分文件存储重命名为 `PrepareRename`（只读/冲突预检）+ `CommitRename`（耐久更名+目录 fsync+失效索引）；现有 `Rename` 改为两者组合，行为不变。
- `internal/persis/dag_repository.go`：新增 `PrepareRename/CommitRename`，通过可选接口 `DAGRenameStore` 委托；不支持的存储回退到旧 `Rename`。
- `internal/dagrename/`（新建包）：
  - `journal.go`：操作目录、`state.json` 状态机原子读写、settings 暂存、opKey/名锁路径。
  - `coordinator.go`：校验后暂存→切换→配置收敛→发布的完整编排、崩溃恢复、幂等识别。
  - `coordinator_test.go`：各阶段失败/崩溃恢复/幂等等回归测试。
- [api.go](file:///Users/ding/Documents/swe/0910031/project-09/internal/service/frontend/api/v1/api.go)：API 增加 coordinator 字段，在 `New` 末尾（options 应用后）当 `cfg.Paths.DataDir != ""` 时构造；发布回调内做 `notifyDAGMutation(newName)` + `logAudit("dag_rename")`；新增 `RecoverPendingDAGRenames(ctx)`。
- [dags.go](file:///Users/ding/Documents/swe/0910031/project-09/internal/service/frontend/api/v1/dags.go)：`RenameDAG` 校验逻辑不变，随后调用 coordinator；coordinator 为 nil 时（DataDir 为空的嵌入式/极简测试场景）走旧路径作为回退。
- [server.go](file:///Users/ding/Documents/swe/0910031/project-09/internal/service/frontend/server.go)：`apiV1` 构造后调用一次启动恢复（失败仅告警，不阻断启动）。
- 测试：`api/v1` rename 用例（settings 随重命名唯一迁移、通知一次、重复完成幂等）；`service/mcp` 增加重复 apply 幂等用例。

## 设计细节

### 日志目录布局（`<DataDir>/dag-renames/`）

```
locks/<sha256(name)>/                        # 名称锁（old、new 各一把，按 hash 排序加锁）
ops/<opKey>/state.json                       # 状态机，WriteFileAtomic 耐久写
ops/<opKey>/settings.json                    # 暂存的新名运行配置（无配置则不存在）
```

`opKey = sha256(oldName + "\x00" + newName)`。`state.json`：

```json
{
  "version": 1,
  "old_name": "a", "new_name": "b",
  "source_sha256": "…",
  "has_settings": true,
  "stage": "prepared|definition_switched|settings_switched|published|completed",
  "actor_id": "…", "actor_name": "…",
  "created_at": "…", "updated_at": "…"
}
```

### 状态机与收敛

- `prepared`：预检通过、候选元数据（源文件 sha256）与运行配置已暂存、状态耐久落盘；定义尚未切换。
- `definition_switched`：`CommitRename` 完成（原子更名 + 父目录 fsync + 索引失效）。
- `settings_switched`：新名 settings 已 Upsert、旧名已 Delete（无配置则直接进入此阶段）。
- `published`：变更通知与成功审计均已发出。
- `completed`：终态标记，短期保留用于重复请求识别。

请求路径（持 old/new 名称锁）：
1. 查 `state.json`：
   - `completed` 且 new 存在（sha 匹配）、old 不存在 → 幂等返回成功，不再通知/审计；
   - `completed` 但环境已变迁（new 不在）→ 视为陈旧标记，覆盖后重新执行；
   - 非终态 → 直接走恢复收敛逻辑再返回。
2. 无 journal：`PrepareRename`（只读来源 → `ErrDAGReadOnly`；目标冲突 → `ErrDAGAlreadyExists`）；读源文件算 sha；`Get(old)` 配置暂存（clone，改名、UpdatedBy=当前 actor、UpdatedAt=now）；写 `prepared`。任一步失败删除整个 op 目录并返回错误。
3. `CommitRename` 切换定义 → 写 `definition_switched`。
4. 有暂存配置：`Upsert(new)` 再 `Delete(old)`（均幂等）→ 写 `settings_switched`。
5. 发布：`notifyDAGMutation(newName)` 与审计（恢复时用 journal 中保存的 actor 信息）→ 写 `published` → 写 `completed`。

启动/惰性恢复（逐 op 加锁后）：

| old | new | 处理 |
|---|---|---|
| 在 | 不在 | sha 匹配且预检仍通过 → 继续前向切换；sha 不符/已冲突 → 丢弃 journal（回滚，旧定义原样） |
| 不在 | 在 | sha 匹配 → 从 settings/发布阶段继续收敛 |
| 都不在 | | 丢弃 journal |
| 都在 | | 异常外部干预：保留 journal 并告警，不盲目收敛 |

启动时顺带清理超过保留期（90 天）的 completed 标记。

### 存储层改动

文件存储把现有 `Rename` 拆为：

- `PrepareRename(ctx, oldID, newID)`：locate old、拒外部 symlink、生成新路径（含 recursive 同目录规则）、冲突检查，不做任何修改。
- `CommitRename(ctx, oldID, newID)`：重新 locate+冲突检查（防暂存期间外部变化），`fileutil.Rename` → `fileutil.SyncDir(新文件目录)` → `invalidateIndex`。
- `Rename = PrepareRename + CommitRename`。

`persis.DAGRepository` 通过可选接口委托；测试桩/其他存储不受影响（接口 `DAGDefinitionStore` 不改动，避免全部 stub 改动）。

### API 层

- `New` 末尾构造 coordinator（DataDir 为空则 nil）；Publish 回调闭包绑定 API 自身的通知与审计方法，使启动恢复路径也能发布。
- handler 校验顺序与错误规则完全不动；通过 coordinator 执行，sentinel 错误沿用既有 HTTP 映射。
- `NewServer` 末尾（监听前）调 `RecoverPendingDAGRenames`；收敛失败只告警，后续重复请求/下次重启继续。

## Implementation Steps

1. 文件存储拆分 `PrepareRename/CommitRename`，`Rename` 改为组合；补存储级单测（预检失败不落任何修改、commit 幂等）。
2. `persis.DAGRepository` 增加委托方法与可选接口。
3. 新建 `internal/dagrename`：journal 读写 → coordinator 主编排（锁、prepare、commit、settings、publish、completed 标记）→ `RecoverAll` 与各崩溃阶段收敛；完成单元测试。
4. API 接入：字段与构造、handler 改造、nil coordinator 回退、恢复入口与 server 接线。
5. API/MCP 回归测试：成功路径（定义+配置一致、只通知一次新名、审计一次）、重复完成幂等、切换后阶段恢复、冲突/只读/运行中拒绝不变。
6. `make fmt`，跑受影响包测试；按需跑 spec022 MCP conformance；可行时 `make check`。

## Dependencies and Considerations

- settings 接口无「排他创建」：用 `Upsert`（暂存内容确定）+ 恢复时 `Get(new)` 比对；窗口内只可能被自身重放，收敛策略=暂存内容为准。
- 进程在「审计/通知已发出」与「published 落盘」之间崩溃存在极小的重复发布窗口：通知是进程内 SSE/缓存唤醒，重复无害；审计为本地日志，重复窗口仅一次崩溃间隙。这是本地文件系统无事务参与方能达到的上限，journal 阶段将其压缩到最小。
- 跨进程安全：dirlock 名称锁 + 全部 journal 写耐久化；DAGsDir 与 DataDir 不同卷时，切换只在 DAGsDir 内 rename（同卷，原子），暂存物在 DataDir 不参与更名。
- 历史运行、scheduler 轮询与索引自愈、suspend flag、外部只读/冲突/权限/运行中规则均保持现状。
- 不动 OpenAPI/proto/UI（接口契约不变），无生成代码。

## Validation

- `go test ./internal/persis/file/dag/... ./internal/persis/... ./internal/dagrename/...`
- `go test ./internal/service/frontend/api/v1/... ./internal/service/mcp/...`
- 手工/测试模拟四个崩溃点（prepared 后、definition_switched 后、settings 半程、published 前后）重启收敛，断言：唯一定义、唯一 settings、旧失败无残留两定义/两配置、通知/审计各一次、重复请求幂等。
- `make fmt` 检查 diff；可行时 `make check`；MCP 行为变化跑 spec022 conformance。

## Risks

- settings 存储在 commit 后持续 IO 失败：journal 保留、接口返回 500，重启/重复请求收敛；不会静默成功（修复核心问题）。
- 陈旧 completed 标记导致的幂等误判：以「new 仍存在且 sha 匹配 + old 不存在」为硬条件，不满足即按新操作处理。
- 名称锁陈旧（进程被杀）：dirlock 自带 30s stale 判定与 fence token，与 Collection 锁一致。
