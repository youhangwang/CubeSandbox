# Design: sandbox-numa-affinity

## Context

考古结论（全部有代码实证）：系统的四层骨架大部分已存在，本设计是把它们接通，而非造新轮子。

- **决策点已存在**：`Cubelet/services/cubebox/resource.go` 已读取 annotation `cube.master.instance.numa_node` 决定 `NumaNode`（无生产者，实际恒走 round-robin），但该值当前是纯记账——`cgroup/local.go:320` 硬编码 `usePoolV2 := false` 使它到不了 cgroup。
- **v2 池完整但停用**：`pool.go` 同时构建 v1/v2 池；v2 按 `numa.GetAllNumaNodes()` 建每 node 子组，`initPoolV2Late` 经 `CreateWithCpuSet` 写父组 `cpuset.cpus`（node 核集）+ `cpuset.mems`（node）；v2 继承语义下子组 effective 值同时约束进程树的 CPU 与内存。
- **资源模型**：承诺货币 `ResourceWithOverHead.HostMemQ` 已存在且被心跳聚合；节点配额 = 物理内存 × 5/4（cubelet 自报，master 只按账面排程）；全局管道**没有任何实测内存准入**（memfilter 物理门的 MemUsage 无写入者恒为 0）。
- **内存真相（影响绑定语义与验收）**：恢复 fast path 将快照 base 以 `MAP_PRIVATE` 文件映射挂入 guest（`memory_manager.rs:1510-1516`），读缺页命中**共享页缓存**（跨 VM 共享同一份物理页，住在首碰者的 node；balloon 打洞明确跳过 immutable base 即为佐证），写才 CoW 出 node 本地私有页。因此绑定保证的是"新分配页 + vCPU 线程"落 node；共享 base 缓存的落位由首碰者决定，需模板级亲和配置对齐。KSM 未启用；guest RAM 是创建时定死的 slab（无 balloon 充气、无热插拔），free-page reporting 持续还页，pause 后进程退出零占用。
- **机制级真机验证（2026-10-08，4-node 验证机 192.168.25.138，openEuler 6.6 aarch64）**：①cpuset.mems 约束首碰分配——node0 组进程首碰 512MB 文件，131072 页全部落 N0；②**node1 组进程（`Mems_allowed_list: 1` 自验证）读同一文件，131072 页仍全部在 N0，100% 远程读**——共享页缓存归属由首碰者决定、绑定不改变既有页归属，A1 结论实证；③node1 组进程写全部页后 VMA 页计数变为全 N1、200MB 匿名页全落 N1——CoW/私有页严格本地。设计模型与实测一致。
- **动机与范围**见 proposal.md，行为契约见 specs。

## Goals / Non-Goals

**Goals**

- 声明 VM 的进程树被可验证地钉在单个 NUMA node 上（v2 池 cpuset 继承：CPU 与内存双约束）
- node 选择有真实的准入依据（账面 + 实测双门），未声明 VM 的占用通过实测口径被看见
- 模板级亲和配置：同模板 VM 粘住同一 node，使共享 base 页缓存与绑定 node 对齐
- 降级行为确定且可配置（besteffort/strict）
- 一期改动只落在 Cubelet；CubeShim、hypervisor 零改动

**Non-Goals**

- shim→hypervisor 拓扑传参（per-vCPU affinity、内存 zone）与 1:1 vCPU pinning——二期（档 2），一期由 cpuset 继承提供 node 粒度约束
- 跨 node VM（guest NUMA 拓扑、multi-node cgroup 组、zone 拆分记账）
- 为绑定 VM 开设独立 memory.max 口子（遵循全局 `disable_host_cgroup`）
- 修改全局超分模型或 master 侧调度
- NUMA 预热（模板按 node 预热）

## Decisions

### D1. 账本放 cubelet，不放 master
node 选择是**节点内局部决策**，依赖节点本地的实测水位与 cgroup 状态；master 的职责边界是宿主机选择。现状 `resource.go` 的 NumaNode 计算就在 cubelet，沿用该位置。
*备选*：master 决定 node 随创建请求下发——被否：master 看不到节点内 per-node 实时水位，会让心跳结构膨胀且引入竞态。

### D2. 双门账本（账面放置 + 实测刹车）
- **账面门**：`Σ HostMemQ(绑到 node N 的声明 VM) ≤ node N 物理 × 5/4`。货币复用 `HostMemQ`（对运行中 VM 的最终物理占用是可靠代理：guest RAM slab 架构自限 ≈ 规格 + 开销），比例复用全局超卖比，不新增旋钮。放置评分以内存为主键、**per-node CPU 承诺比为次级键**（防止内存驱动的放置把 CPU 装箱压向单侧）。
- **实测门**：`node N 不可回收用量 + 新请求 + 保底水位 ≤ node N 物理`。**口径必须排除可即时回收的页缓存**——模板 base 缓存可达 GB 级且为全体同模板 VM 共享，naive 的 total−free 口径会把它当压力，导致过刹、误降级与 node 间反向失衡。候选口径（真机标定）：meminfo 推导（匿名 + 内核 + 脏页）或 per-cgroup `memory.current` 汇总（共享页只记一次账）。
*为什么必须有实测门*：全局超分成立靠统计支柱且无实测兜底；绑定把 node 变成硬分区，`cpuset.mems` 下 node 内物理耗尽时内核无回旋，本地 OOM 是突发的。实测口径同时把"不可见邻居"（未声明 VM）纳入视野。
*备选*：纯账面——被否：对"邻居挤爆"失明；账面 + 静态水位——被否：水位是猜测，实测是事实。

### D3. 池选择按 sandbox 声明状态决定：声明 → v2 numa 组；未声明 → v1 原样
**开关语义 = NUMA 特性总开关，不是全局换池**。声明 VM 从对应 `numa<N>` 子池分配（`pool.Get` 本就 per-call 传 usePoolV2），cpuset 由父组经 v2 继承生效（验证读 `.effective` 文件）；未声明 VM 维持 v1 路径与 cgroup 位置，行为零变化。
**cgroup v1 宿主机分叉（已在验证机实证）**：v1 cpuset 子组创建后 cpus/mems/effective 全空、任务无法挂入——v1 handle 下 per-VM 子组必须在创建时显式回填 cpuset，或在 `numa<N>` 父组设 `cgroup.clone_children=1`（子组 mkdir 即拷贝父值，与池布局天然匹配，推荐后者）；v1 同样有 `cpuset.effective_*` 文件（显式设置后），验收断言可跨版本统一。
*备选*：v2 增加"不绑组"收编未声明 VM——被否（本期）：升级期新旧 cgroup 路径混合、v1 清理/复用/tombstone 路径被绕开，爆炸半径不成比例；作为 v2 验证充分后的独立收编 change。

### D4. 一期仅 cgroup 绑定；shim→hypervisor 传参推迟二期
v2 池父组 cpuset（cpus + mems）经继承同时约束进程树的 CPU 与内存，node 粒度目标已达成；per-vCPU 1:1 pinning（档 2：拓扑感知选核、SMT 兄弟规避、per-node CPU 指派表）是 node 内精调，属二期。
*备选*：一期同时做 hypervisor affinity——被否：CubeShim/VmConfig/annotation 链路改动大而增益主要在高规格低密度场景；cpuset 继承已消除跨 node 迁移这一主害。

### D5. annotation 复用 `cube.master.instance.numa_node`
已验证：全仓库无生产者（CubeMaster 仅定义常量，`resource.go:41` 消费），是死通道，复用零迁移。语义：不设 = 不绑定（round-robin 记账行为与 `cube.numa_node` label 保留）；`"auto"` = 账本自动；合法 int = 指定 node（hint）；非法值 = 按 auto 处理 + 警告留痕。解析块重写于 `resource.go`。
**写入者链路（已验证）**：master 创建接口本就收 annotations map（CubeAPI 现有调用即在发送）。用户侧：`NewSandbox` 已有 `metadata` 字段，且 CubeAPI 已有"metadata 键提升为 master annotation"的先例（`HOSTDIR_MOUNT_KEY`，`sandboxes.rs:190`）——**终端用户自指定 = 照先例加一条提升规则（几行 Rust，API 契约不变）**。提升规则须是白名单式（仅 NUMA 键），且平台 annotation（模板 id 等）不可被用户 metadata 覆盖。遗留产品细化：对外暴露的语义程度——建议用户仅可见 `auto`（性能优化语义），node 编号留给内部/企业直写 annotation 场景。
*备选*：新键——被否：重复通道徒增约定成本；由 master 解释——被否：见 D1。

### D6. 模板级亲和配置（对齐共享页缓存与绑定 node）
配置 `numa.template_affinity`（off 默认 / on）。开启时：模板家族（**快照 lineage 根**，即实际共享的 base 镜像——增量派生模板共享祖先 base，粘滞键必须与共享单元对齐，否则同一份 base 缓存被劈到多个 node）的首个声明 VM 由账本挑 node，此后同 lineage 声明 VM 粘住同一 node。映射存 cubelet 本地 store；**重启后从现网已绑定 VM 反推**（同 lineage 多数派 node）；**连续 N 次粘滞 node 双门失败触发重映射**（避免长期满 node 上的退化为每台白试一轮）。粘滞 node 双门满 → 走既有降级链。作用域默认仅声明 VM；若配置扩展到未声明 VM，语义为 operator 强制 opt-in，需在文档明示。已知边界：未声明 VM 恢复同 lineage 时可能在其他 node 散落 base 页（无 cpuset 约束），混合负载下共享缓存纯净度非 100%，进观察期指标。
*理由*：base 缓存是共享机制（D2/Context），模板级亲和让共享发生在正确的 node——共享收益与局部性同时保住，零内存膨胀；与"对抗共享"的私有化恢复方案（内存膨胀、恢复变慢）相对，代价最小。页缓存是 per-host 的，per-host 粘滞映射天然正确。

### D7. 降级链单一，策略节点级配置
`numa_alloc_policy = "besteffort" | "strict"`（cubelet 配置，默认 besteffort）。唯一一条链：指定 node 满 → 换 node；全满 → 策略终点（besteffort 照常部署不绑定留痕 / strict 类型化错误直接返回用户，master 不重试）。大 VM（规格 > 单 node 物理）是几何事实而非竞争失败，同样走这条链，无特例。
*备选*：strict 下 master 换宿主机重试——被用户否决（直接返回用户）。

### D8. 不为绑定 VM 开限额口子
绑定 VM 的 `memory.max` 是否写入，完全遵循全局 `disable_host_cgroup` 开关（"绑定即限额"方案被否决）。
*代价*（进 Risk 节）：绑定 VM 行为失准（泄漏等）时无单机止损，膨胀页全部落在绑定 node。接受此代价换取配置模型的一致性；实测门在准入侧限制向高水位 node 继续放 VM。

## Risks / Trade-offs

- [生产 `disable_host_cgroup` 实际值未知（仓库 dynamicconf 为 true）→ 若为 true，绑定 VM 无 memory.max，坏 VM 可耗尽单 node] → 运维确认实际值；上线后监控 per-node 水位与 `numastat`；必要时未来再议限额口子
- [共享 base 页缓存落位由首碰者决定：模板亲和关闭、或同模板含未声明 VM 时，绑定 VM 存在远程读] → 推荐生产开启模板亲和；观察期指标：同模板 VM 的远程页占比（numastat）；验收口径区分私有页与共享页
- [实测门口径与保底水位需真机标定，初值靠经验] → 多 NUMA 真机按候选口径对数标定；初期取保守值，宁紧勿松
- [busy 宿主（未声明 VM 物理吃满）上实测门双 node 全不过 → 声明 VM 全部 besteffort 不绑定，特性"静默失效"] → 行为正确（安全优先）；灰度期降级率为一等监控指标，migration 明示预期
- [内存驱动的放置造成 node 间 CPU 装箱失衡] → 放置评分含 per-node CPU 次级键；观察 node 间 vCPU:逻辑核比
- [装箱相关性：同模板 VM 聚集同 node，CoW 胃口相关，削弱账面超卖统计前提] → 与 D6 叠加放大；观察期监控同 node 同模板内存水位方差
- [账本/cgroup 泄漏（shim 崩溃不走 Destroy）] → 周期对账：v2 池 `AllGroups()` 枚举现网目录，与账本差异告警并修正
- [恢复不保证回原 node] → 已接受：缓存随 pause 落盘丢失，落位质量由账本保证而非位置记忆；模板亲和开启时倾向粘回同 node
- [本机单 node 无法验证任何 NUMA 行为] → 验收依赖多 NUMA 真机，清单：`.effective` 文件、vCPU `Cpus_allowed_list`（cpuset 继承）、`numastat` 页落 node（区分私有/共享）

## Migration Plan

1. 合入后总开关默认关闭，行为与现状完全一致
2. 多 NUMA 真机验证绑定正确性清单，标定实测门口径与水位
3. 灰度节点打开总开关（besteffort + 模板亲和 on），观察账本水位、降级率、远程页占比
4. 运维按需对特定节点启用 strict
5. 回滚 = 关闭总开关（等价回到现状；已绑定存量 VM 重建后自然回落）

## Open Questions

- 实测门口径：**字段可用性已验证**（per-node meminfo 具备 AnonPages/Shmem/SUnreclaim/Dirty/PageTables/KernelStack/FilePages 全套），候选公式 = 不可回收 ≈ AnonPages + Shmem + SUnreclaim + Dirty + PageTables + KernelStack；剩余为水位参数真机标定
- 模板→node 粘滞映射的存储形态（cubelet 本地 KV 即有 bucket 可复用）与重启重建规则
- `numa.template_affinity` 是否需要 per-template 白名单粒度（先全局开关起步）
