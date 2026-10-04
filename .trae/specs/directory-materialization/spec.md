# 目录树路径物化 - 产品需求文档

## Overview
- **Summary**: 扩展 build 工作流的路径物化能力，使步骤可以直接声明目录输入和目录输出，而无需先把目录打包为单个文件。
- **Purpose**: 支持网站构建目录、模型目录、分区数据集等天然树状产物的稳定指纹、暂存发布、崩溃恢复与跨步骤复用。
- **Target Users**: 在本地 build DAG 中生产或消费目录数据集的数据工程师、机器学习工程师和站点/文档构建工程师。

## Goals
- 输入路径可以是普通文件或完整目录树。
- 输出路径可以显式声明为目录，运行时为每次尝试提供独立的暂存目录。
- 目录指纹稳定覆盖相对路径、文件内容、空目录、符号链接信息以及文件/目录/符号链接类型变化。
- 发布以 manifest 和 journal 记录提交状态；失败、中止或重启后不得把未确认产物当作可复用结果。
- 保持现有单文件物化、复用决策、dry-run、`--no-reuse`、控制依赖、重试和旧 manifest 的行为。

## Non-Goals
- 不实现分布式 worker 之间的目录物化围栏；该能力仍限定本地执行。
- 不把目录复制到中心化对象存储或引入控制面数据库。
- 不改变普通值输出（`type: string`、`type: json`）的语义。
- 不支持设备文件、套接字、命名管道等非常规树成员。
- 不保证未参与 Dagu 路径锁协议的外部写入者不会主动修改输入树。

## Background & Context
现有实现位于：
- `internal/build`：路径解析、快照、复用评估、暂存路径和会话生命周期。
- `internal/persis/file/materialization`：路径锁、manifest、journal、文件替换和恢复。
- `internal/runtime/build_plan.go` 与 `internal/runtime/build_execution.go`：构建期路径规划、推断依赖和执行集成。
- `specs/033-build-workflows.md`：当前 build 工作流规范性说明。

现有 manifest 为 schema version 1，只描述普通文件，输出通过同目录临时文件和原子文件替换发布。目录输出首次执行前不存在，因此用户已确认使用显式 `type: directory` 声明；目录树内符号链接仅允许解析后仍位于树根内的链接。

## Functional Requirements
- **FR-1 声明模型**
  - `outputs[].path` 条目支持 `type: file` 或 `type: directory`；省略时输出默认 `file`，以兼容现有工作流。
  - `inputs[]` 条目支持 `type: file` 或 `type: directory`；省略时根据现有路径接受普通文件或目录，显式类型用于提前校验。
  - 路径型 `type: file` / `type: directory` 与值型 `type: string` / `type: json` 不得混用。
- **FR-2 规划与引用**
  - 目录输入和目录输出解析为稳定绝对路径，继续支持 `${inputs.<name>}`、`${outputs.<name>}` 与 `${steps.<id>.outputs.<name>}`。
  - 相同目录路径继续推断生产者到消费者的依赖；目录与其后代路径之间的重叠必须被识别为依赖或验证错误，避免树的部分读写失去顺序。
  - 每个 canonical 输出路径仍只能有一个生产者。
- **FR-3 目录快照与指纹**
  - 普通文件继续使用 SHA-256 内容摘要。
  - 目录快照使用规范化、排序后的相对路径记录全部成员。
  - 空目录必须作为目录成员进入快照；根目录本身也必须被记录。
  - 每个普通文件记录相对路径、文件类型、大小和内容摘要。
  - 每个符号链接记录相对路径、符号链接类型和链接目标；合法目标必须在完全解析后仍位于该目录树根内。
  - 文件、目录、符号链接之间的类型变化必须改变快照与复用判断。
  - 相对路径分隔符使用规范化 slash 表达，使同一树结构在不同平台上得到稳定摘要。
- **FR-4 暂存**
  - 文件输出继续使用新的、不存在的同目录兄弟暂存文件。
  - `type: directory` 输出的每次尝试获得一个新的、不存在的同目录兄弟暂存路径，运行时在执行前创建空暂存目录，命令只在该目录内生成树。
  - 每次重试使用新的暂存目录；失败或未提交的尝试不得污染目标目录。
- **FR-5 验证与发布**
  - 提交前必须重新验证暂存树和所有输入快照。
  - 暂存根类型必须符合声明；树成员只能是普通文件、目录或树内符号链接。
  - 输入树在执行期间变化、符号链接逃逸、暂存树读取失败或成员不完整时，提交必须失败且不得写入可复用 manifest。
  - 发布过程通过 journal 记录暂存路径、目标路径、旧树快照、新树快照、旧 manifest 和待提交 manifest。
  - 普通文件发布保持现有原子替换语义；目录发布优先使用操作系统支持的原子交换能力，使锁协调下的消费者只能看到完整旧树或完整新树。
  - 文件与目录之间的类型切换必须完整切换目标树，并因旧 manifest 的输出类型不匹配而失效。
- **FR-6 恢复与清理**
  - 重启或新会话获取排他输出锁时，必须根据 journal、manifest、当前目标树和暂存树判断提交是否已完成。
  - 新 manifest 与新树均确认时完成提交；否则回滚到上一份确认树和 manifest。
  - 无法识别为旧树或新树的目标内容不得被覆盖。
  - 没有 journal 保护的本实例暂存目录在取得输出锁后必须清理。
- **FR-7 并发**
  - 多个步骤并发读取同一文件或目录可以共享读锁。
  - 写入相同输出仍必须排他串行。
  - 读取目录树与写入该树内任何已存在成员必须通过后代路径锁或规划依赖发生互斥/排序。
- **FR-8 兼容性**
  - schema version 1 manifest 必须继续可读，并按普通文件处理。
  - 新 manifest 使用新的 schema version；旧版本二进制不能误读新格式。
  - `--no-reuse` 只绕过命中，不绕过暂存、验证、journal 和发布安全。
  - dry-run 不获取锁、不创建暂存树、不写 manifest。
  - 显式控制依赖仍使物化步骤执行；重试仍获得新尝试路径。

## Non-Functional Requirements
- **NFR-1 安全边界**: 路径规范化和符号链接检查必须防止通过绝对路径、多级 `..`、链接链或大小写别名逃出声明根。
- **NFR-2 可恢复性**: 所有目标树和 manifest 的切换步骤必须可由 journal 重放判断；正常错误不得破坏上一份确认产物。
- **NFR-3 最小侵入**: 复用现有 build、file materialization、runtime 和 spec 结构，不引入新的数据库或外部依赖。
- **NFR-4 可测试性**: 核心行为通过 focused Go 单测覆盖，并通过 runtime/integration 级测试验证目录生产者和消费者。
- **NFR-5 跨平台**: macOS/Linux 使用可用的原生目录交换能力；其他平台使用排他锁保护的 journal 回退流程，代码仍须通过 Windows 格式和静态检查约束。

## Constraints
- **Technical**: Go 标准库与现有 `golang.org/x/sys`、`gofrs/flock`、`internal/cmn/fileutil`；不得手改生成代码。
- **Business**: build 物化仍限定本地命令/shell 执行；容器、secret、动态输出、并行/foreach/sub-DAG 等现有不可复用条件不变。
- **Dependencies**: 文件系统需支持目录枚举、fsync/目录同步和现有 flock 语义；原子交换不可用时启用回退发布。

## Assumptions
- 用户已确认输出目录通过 `type: directory` 显式声明。
- 用户已确认目录内符号链接采用“仅允许树内链接”策略。
- 空目录输出是合法产物；是否执行业务内容由步骤命令负责，物化层只验证树结构可完整读取。
- 树内符号链接允许指向目录、文件或另一个树内符号链接；提交时链接目标必须存在且解析后仍在根内。
- Dagu 步骤消费者通过路径锁访问声明路径；原生交换用于消除本机切换窗口， journal 回退路径至少保证 Dagu 锁协调消费者不会看到未确认树。

## Acceptance Criteria

### AC-1: YAML 可显式声明文件或目录路径
- **Type**: `rule`
- **Given**: 一个 `type: build` DAG 的输入或输出条目包含 `path`。
- **When**: 加载 YAML 并构建 IR。
- **Then**: 输出省略 `type` 时为文件输出，`type: file` 与 `type: directory` 被接受；输入可显式或隐式接受目录；非法类型或路径/值输出混用被拒绝。
- **Pass Condition**: `internal/spec` 测试覆盖默认文件、显式文件、目录和非法组合，IR 中保留路径类型。
- **Evidence**: `go test ./internal/spec/...`。

### AC-2: 目录指纹稳定覆盖完整树
- **Type**: `rule`
- **Given**: 同一目录包含嵌套文件、空子目录和树内符号链接。
- **When**: 连续两次生成快照，且仅改变文件遍历顺序。
- **Then**: 两次根摘要、成员顺序和 manifest 输出完全一致。
- **Pass Condition**: 重命名/移动相对路径、修改文件内容、增删空目录、改变文件/目录/符号链接类型或修改链接目标都会改变摘要。
- **Evidence**: `internal/build` 快照单元测试。

### AC-3: 逃逸符号链接和非法成员不能提交
- **Type**: `rule`
- **Given**: 暂存目录包含绝对逃逸链接、多级 `..` 链接、链接链逃逸、链接环或套接字/设备等特殊成员。
- **When**: 步骤尝试提交该暂存树。
- **Then**: 提交失败，目标树和已提交 manifest 保持原状，失败结果不可复用。
- **Pass Condition**: 每种非法情况均有 focused regression test，且合法树内链接通过。
- **Evidence**: `go test ./internal/build/... ./internal/persis/file/materialization/...`。

### AC-4: 每次目录尝试使用独立暂存目录
- **Type**: `rule`
- **Given**: 一个声明 `type: directory` 输出的可复用步骤。
- **When**: 步骤第一次执行或失败后重试。
- **Then**: 运行时在目标路径的同目录创建新的、空的、不存在冲突的暂存目录；`${outputs.<name>}` 指向该目录；文件输出仍取得暂存文件路径。
- **Pass Condition**: 多次尝试路径不同，失败尝试被递归清理，目标路径在提交前不存在新内容。
- **Evidence**: build session 与 runtime 重试测试。

### AC-5: 目录输出提交后可被第二个运行复用
- **Type**: `rule`
- **Given**: 目录生产者第一次运行成功提交包含文件和空目录的树。
- **When**: 使用相同 recipe 和输入再次运行消费者/生产者。
- **Then**: 决策为 `reuse`，消费者获得最终目录路径，树内容和空目录与 manifest 一致。
- **Pass Condition**: 连续两次 build session 断言 `BuildDecisionReuse`、producer run 和目录内容。
- **Evidence**: `go test ./internal/build/...` 与 runtime 集成测试。

### AC-6: 执行期间输入树变化会否决本次复用
- **Type**: `rule`
- **Given**: 输入目录在评估后被修改，包括文件内容、相对路径、空目录或成员类型变化。
- **When**: 步骤执行结束并提交。
- **Then**: Commit 报输入变化错误，不替换目标树，不写新的可复用 manifest。
- **Pass Condition**: 修改发生在执行后时断言提交失败且旧 manifest 仍可命中。
- **Evidence**: build commit regression test。

### AC-7: 文件与目录类型切换使旧 manifest 失效
- **Type**: `rule`
- **Given**: 同一步骤同一目标路径曾提交文件，随后声明并生成目录（或反向切换）。
- **When**: 新运行评估和提交。
- **Then**: 旧 manifest 因输出根类型/recipe 不匹配而不能复用；新提交完成后目标路径和 manifest 均为新类型。
- **Pass Condition**: 两个方向的切换都有测试，且旧树在失败发布时保留。
- **Evidence**: build 与 materialization 测试。

### AC-8: journal 覆盖目录发布、回滚和重启恢复
- **Type**: `rule`
- **Given**: 目录提交在备份/交换、目标替换、manifest 写入等不同阶段被模拟中断，或存在未 journal 化的暂存目录。
- **When**: 重新获取排他输出锁并恢复。
- **Then**: 已确认的新树和新 manifest 被保留；未确认状态回滚到旧树和旧 manifest；首次提交失败时清理目标；无 journal 的暂存目录被清理；未知目标内容保持不动并报错。
- **Pass Condition**: 恢复状态矩阵覆盖旧树、新树、暂存树、manifest 存在/缺失和类型切换。
- **Evidence**: `go test ./internal/persis/file/materialization/...`。

### AC-9: 读写锁继续支持共享与排他
- **Type**: `rule`
- **Given**: 多个会话访问相同或有祖先/后代关系的声明路径。
- **When**: 并发获取锁。
- **Then**: 同路径读者共享；同路径写者排他；目录读者与树内写者不会在未排序情况下并发修改同一成员。
- **Pass Condition**: 锁测试覆盖同目录共享、同输出串行以及目录后代冲突。
- **Evidence**: materialization store lock tests。

### AC-10: dry-run、no-reuse、控制依赖和重试保持原语义
- **Type**: `rule`
- **Given**: 目录物化步骤分别运行 dry-run、`--no-reuse`、带运行过的显式控制依赖、失败重试。
- **When**: 评估或执行步骤。
- **Then**: dry-run 不加锁不创建暂存目录；no-reuse 执行但仍安全提交；控制依赖运行过则执行；重试获得新暂存目录且不污染旧尝试。
- **Pass Condition**: 现有文件语义测试继续通过，并新增目录对应测试。
- **Evidence**: `go test ./internal/build/... ./internal/runtime/...`。

### AC-11: 旧版普通文件 manifest 继续可用
- **Type**: `rule`
- **Given**: store 中存在 schema version 1 的普通文件 manifest。
- **When**: 新版本读取并评估相同文件输出。
- **Then**: manifest 被识别为普通文件；匹配时可复用，不匹配时按既有原因重新执行。
- **Pass Condition**: 版本 1 fixture 或等价构造通过反序列化、复用和输出验证。
- **Evidence**: materialization store 测试。

### AC-12: 目录依赖规划和运行时引用端到端可用
- **Type**: `rule`
- **Given**: 一个目录生产者和一个消费该目录路径的下游步骤，另包含目录/后代重叠场景。
- **When**: 准备 build plan 并运行工作流。
- **Then**: 相同路径推断为依赖，重叠路径被排序或拒绝；执行环境中的 `${inputs.<name>}`、`${outputs.<name>}` 和步骤输出引用均解析为正确绝对路径。
- **Pass Condition**: runtime plan 测试和一个本地命令目录生产/消费测试通过。
- **Evidence**: `go test ./internal/runtime/...`，必要时 `make conformance`。

### AC-13: 实现保持局部、可维护且跨平台约束清晰
- **Type**: `rubric`
- **Dimension**: 代码结构与平台抽象质量
- **Scale**: 1-5
- **Anchors**: 1 = 在业务流程中堆砌平台分支且难以恢复；3 = 功能可用但快照/发布/恢复职责混杂；5 = 快照、暂存、journal、锁和 runtime 集成边界清晰，平台回退有明确注释与测试。
- **Pass Threshold**: >= 4
- **Evidence**: 代码审查、受影响包测试和 `make fmt` 检查结果。

## Open Questions
- 无。目录输出声明方式与树内符号链接策略已由用户确认。
