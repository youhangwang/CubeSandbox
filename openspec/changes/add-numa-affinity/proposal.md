# Add NUMA Affinity for Sandboxes

## Why

多 socket 宿主机上，sandbox VM 的落位是随机的：2U2G 典型规格的 VM 约有一半概率内存/线程跨 NUMA node，性能不可预测。目标不是提升均值，而是**确定性**——声明的 VM 被正确 NUMA 亲和后，同规格 VM 的落位质量一致。验收口径 = 绑定正确性可验证（cpuset 生效、vCPU 落核、页落 node），不依赖基准分数。

## What Changes

- **annotation 语义（opt-in，复用现有键）**：`cube.master.instance.numa_node`（已验证全仓库无生产者）：不设 = 不绑定；`auto` = 账本自动选 node；数字 = 指定 node（hint 语义：尽力满足）。不声明的 sandbox 走现有流程原样（v1 池，不进 NUMA 账本，现有记账行为不变）
- **Cubelet per-node 双门账本**：为声明 VM 挑选 NUMA node。账面门做放置（已绑定声明 VM 的 `HostMemQ` 累加 ≤ node 物理内存 × 全局超卖比例 5/4；per-node CPU 承诺比为次级评分键）；实测门做刹车（node 不可回收用量 + 新请求 + 保底水位 ≤ node 物理，口径排除可即时回收的页缓存），实测口径天然覆盖未声明 VM 的占用
- **cgroup v2 池绑定（一期唯一落地机制）**：NUMA 总开关替换 `cgroup/local.go` 硬编码的 `usePoolV2 := false`；声明 VM 经 `numa<N>` 池层级以 cpuset 继承获得进程树 CPU + 内存双约束。**CubeShim / hypervisor 一期零改动**
- **模板级亲和配置**：`numa.template_affinity`（默认 off）——开启时同模板声明 VM 粘住同一 node，使共享的模板 base 页缓存与绑定 node 对齐（共享收益与局部性同时保住，零内存膨胀）
- **降级链 besteffort / strict**：cubelet 启动时配置。指定 node 满但其他 node 有位 → 换 node 绑定；所有 node 装不下 → besteffort 照常部署不绑定（留痕），strict 返回类型化错误给用户（master 不换主机重试）

明确不做（Non-goals）：

- **大规格 VM（单 node 装不下）不做亲和**：跨 node guest NUMA 拓扑、multi-node cgroup 组、zone 拆分记账全部不在本期。大 VM 统一走降级链终点
- **shim→hypervisor 拓扑传参（per-vCPU affinity、1:1 vCPU pinning）不在一期**：v2 池 cpuset 继承已提供 node 粒度的 CPU+内存约束，node 内精调留二期
- **绑定 VM 不带专项 memory.max**："绑定即限额"被否决，限额遵循全局 `disable_host_cgroup` 开关，不为绑定开专项口子
- 不修改 cube-hypervisor（VMM 侧能力已齐备）

## Capabilities

### New Capabilities

- `sandbox-numa-affinity`: sandbox 的 NUMA 亲和完整行为——annotation 语义、node 选择账本（双门准入）、cgroup/vMM 绑定落地、降级链（besteffort/strict）与配置项

### Modified Capabilities

（仓库尚无既有 spec，无）

## Impact

- **Cubelet**：`services/cubebox/resource.go`（NumaNode 决策点已在，改为账本）、`plugins/cube/internals/cgroup/local.go`（usePoolV2 开关）、`pool.go`/`pool_v2.go`（绑定路径已建好）、`handle/v2`、`pkg/config`（新 knob、besteffort/strict 配置）、`services/cubebox/service.go` 与 `cube_container_create.go`（annotation 解析、创建链路）
- **CubeShim / hypervisor**：一期零改动（绑定由 cgroup 继承完成；hypervisor 的 affinity/host_numa_node 能力留二期传参使用）
- **CubeAPI（可选，随"用户自指定"产品决定）**：`create_sandbox` 的 metadata→annotation 提升规则加一条 NUMA 键（仿 `HOSTDIR_MOUNT_KEY` 先例），API 契约不变
- **配置**：Cubelet 新增 NUMA 配置段（总开关、账本参数、降级策略、模板亲和）；生产 `disable_host_cgroup` 实际值需运维确认（影响绑定 VM 的止损现状评估）
- **验证**：需多 NUMA 真机（本机单 node 无法验证 NUMA 行为）；pause→resume 重新过账本、不保证回原 node
