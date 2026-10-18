# sandbox-numa-affinity Delta

## Purpose

让声明了 NUMA 要求的 sandbox VM 被绑定到单个 NUMA node（进程树 cpuset + vCPU/内存 zone），由 cubelet 侧账本按"账面 + 实测"双门选择 node，保证绑定正确性可验证；未声明的 VM 行为不变，装不下的按 besteffort/strict 策略降级。

## ADDED Requirements

### Requirement: NUMA 绑定为 opt-in 声明制
sandbox 只有在请求携带 NUMA annotation（`cube.master.instance.numa_node`，值为 `auto` 或 node 编号）时才执行 NUMA 分配与绑定；未声明的 sandbox 必须走现有创建流程（v1 池，不进入 NUMA 账本，不产生 cpuset 约束，现有记账行为不变）。`auto` 表示由账本自动选择 node；数字值指定目标 node，语义为 hint（尽力满足，非强制）；非法值按 auto 处理并留警告痕迹。

#### Scenario: 未声明 annotation 的 sandbox
- **WHEN** 创建请求不含 NUMA annotation
- **THEN** 系统按现有流程分配资源并部署，不写入任何 NUMA 相关 cpuset，不占用 NUMA 账本额度

#### Scenario: 声明了 NUMA 的 sandbox
- **WHEN** 创建请求携带 NUMA annotation（无论是否指定 node）
- **THEN** 系统执行 NUMA node 选择与绑定流程，并将该 VM 的承诺量记入所选 node 的账本

### Requirement: node 选择执行账面门准入
为声明 VM 选择 node N 时，系统必须满足：已绑定到 node N 的全部声明 VM 的账面承诺量（含本 VM）不超过 node N 物理内存乘以全局内存超卖比例（当前为 5/4）。账面门用于放置决策，超卖比例复用系统整体比例，不新增独立旋钮。

#### Scenario: 账面额度充足
- **WHEN** node N 的账面承诺量加新 VM 承诺量仍低于 N 物理 × 超卖比例
- **THEN** node N 可作为该 VM 的绑定候选

#### Scenario: 账面额度耗尽
- **WHEN** node N 的账面承诺量加新 VM 承诺量超过 N 物理 × 超卖比例
- **THEN** 该 VM 不得被放置到 node N

### Requirement: node 选择执行实测门刹车
账面门通过后，系统还必须满足实测门：node N 的实测不可回收内存用量（覆盖该 node 上全部不可回收物理页的占用者，包括未声明 VM；可即时回收的页缓存不计入）加上新 VM 请求量与保底水位不超过 node N 物理内存。实测门用于阻止向压力已高的 node 继续放置。

#### Scenario: 实测水位健康
- **WHEN** node N 实测使用 + 新 VM 请求 + 保底水位 ≤ N 物理内存
- **THEN** node N 通过实测门，可被选为绑定目标

#### Scenario: 实测水位超限（含不可见邻居）
- **WHEN** node N 实测使用 + 新 VM 请求 + 保底水位 > N 物理内存（即使账面门有余量）
- **THEN** 该 VM 不得被放置到 node N

### Requirement: 指定 node 的 hint 降级路径
声明 VM 指定了 node 且该 node 双门通过时，必须绑定该 node；指定 node 不满足但其他 node 满足时，系统必须改绑其他满足条件的 node，而不是直接失败或放弃绑定。

#### Scenario: 指定 node 可用
- **WHEN** 用户指定 node 1 且 node 1 通过双门
- **THEN** VM 绑定到 node 1

#### Scenario: 指定 node 不可用但其他 node 可用
- **WHEN** 用户指定 node 1 但 node 1 双门不通过，node 0 双门通过
- **THEN** VM 绑定到 node 0

### Requirement: 装不下时按 besteffort/strict 策略降级
所有 node 均无法通过双门时，按 cubelet 启动配置的 NUMA 分配策略处理：besteffort 策略下 VM 照常部署但不绑定（走非绑定流程）且留痕；strict 策略下创建失败，返回类型化错误给调用方，master 不重试其他宿主机。策略为 cubelet 节点级配置。

#### Scenario: besteffort 策略下全部 node 装不下
- **WHEN** 声明 VM 在所有 node 上双门均不通过，且策略为 besteffort
- **THEN** VM 照常创建并部署，无 NUMA 绑定，创建结果中留有降级痕迹

#### Scenario: strict 策略下全部 node 装不下
- **WHEN** 声明 VM 在所有 node 上双门均不通过，且策略为 strict
- **THEN** 创建失败并向用户返回明确的 NUMA 容量不足错误，master 不将该请求改派其他宿主机

### Requirement: 绑定对进程树生效且可验证
绑定成功后，该 sandbox 的 shim+VMM 进程树的 CPU 与内存分配必须被限定在所选 node 内（cpuset.cpus ⊆ node 核集，cpuset.mems = 所选 node），且该约束必须可以在宿主上验证（cgroup 文件与进程亲和性可读）。

#### Scenario: 绑定生效
- **WHEN** 声明 VM 被绑定到 node 0 并完成创建
- **THEN** 该 VM 的 cgroup 有效 cpuset.mems 为 node 0，其 vCPU 线程的 CPU 亲和性集合为 node 0 的核子集

#### Scenario: 绑定正确性核验
- **WHEN** 在多 NUMA 宿主机上按验收清单检查一台已绑定 VM
- **THEN** cpuset 生效值、vCPU 落核、私有内存页落 node 三项均可观察且与所选 node 一致（共享模板页缓存的落位由模板亲和配置决定）

### Requirement: 单 node 装不下的大规格 VM 不做亲和
当声明 VM 的规格无法容纳于任何单个 NUMA node 时，系统不为其提供跨 node 绑定能力，必须直接走降级链终点（besteffort 不绑定部署，或 strict 失败）。

#### Scenario: 规格超过单 node 容量
- **WHEN** 声明 VM 的内存规格大于任意单 node 的物理容量
- **THEN** 系统不尝试跨 node 拆分，直接按 besteffort/strict 策略处理

### Requirement: 暂停恢复后重新过账本
绑定 VM 暂停后再恢复时，恢复产生的是新 VM 实例，必须重新执行 node 选择与双门准入；系统不保证恢复到原 node，恢复结果遵循当时的账本状态与降级策略。

#### Scenario: 恢复时原 node 有额度
- **WHEN** 绑定在 node 0 的 VM 暂停后恢复，且 node 0 双门通过
- **THEN** 恢复的 VM 重新绑定（node 由账本决定，可为 node 0 或其他可用 node）

#### Scenario: 恢复时无任何 node 有额度
- **WHEN** 绑定 VM 暂停后恢复时所有 node 双门均不通过
- **THEN** 按 besteffort/strict 策略降级，与新建声明 VM 的行为一致

### Requirement: 模板级亲和配置
系统必须提供模板级亲和配置（默认关闭）。开启时，**共享同一快照基线（lineage 根，即实际共享的 base 镜像；含增量派生的模板家族）**的声明 VM 必须粘住同一 NUMA node：该 lineage 首个声明 VM 由账本按双门选择 node，其后同 lineage 声明 VM 沿用粘滞映射；粘滞映射须在 cubelet 重启后从现网已绑定 VM 恢复，且粘滞 node 持续无法通过双门时必须支持重映射；粘滞 node 双门不通过时走既有降级链。配置关闭时行为与普通声明 VM 一致。

#### Scenario: 模板亲和开启时的粘滞
- **WHEN** lineage L 的第一台声明 VM 绑定到 node 0，随后一台共享 L 的声明 VM（可为派生模板）创建
- **THEN** 该 VM 在双门通过时同样绑定 node 0

#### Scenario: 粘滞 node 容量不足
- **WHEN** lineage L 的粘滞 node 双门不通过
- **THEN** 该 VM 走既有降级链（换 node 绑定或按策略降级），不影响已绑定 VM；粘滞 node 持续不通过时映射被重选

### Requirement: NUMA 特性可配置且默认关闭绑定
NUMA 绑定能力必须由 cubelet 配置控制总开关；总开关关闭时，即使请求携带 annotation，系统也不执行绑定（annotation 被忽略）。配置项包括：总开关、降级策略（besteffort/strict，默认 besteffort）、实测门保底水位、模板级亲和开关。总开关只改变声明 VM 的池选择（v2）；未声明 sandbox 恒走 v1 池，cgroup 位置与行为不变。超卖比例复用全局配置，不新增独立比例。

#### Scenario: 总开关关闭
- **WHEN** cubelet 配置关闭 NUMA 特性，请求携带 annotation
- **THEN** VM 按未声明流程部署，annotation 不产生任何效果

#### Scenario: 默认策略
- **WHEN** 配置未显式指定降级策略
- **THEN** 使用 besteffort（装不下照常部署不绑定）
