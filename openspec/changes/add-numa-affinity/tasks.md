# Tasks: add-numa-affinity

## 1. 配置与开关基础

- [x] 1.1 Cubelet 新增配置项：NUMA 总开关、`numa_alloc_policy`（besteffort|strict，默认 besteffort）、实测门保底水位、`numa.template_affinity`（off 默认 / on）（`pkg/config` + config.toml 示例）
- [x] 1.2 `cgroup/local.go` 用 NUMA 总开关替换 `usePoolV2 := false` 硬编码（语义：开关只对声明 VM 选择 v2 池，未声明 VM 恒走 v1）；单测覆盖声明/未声明 × 开关 on/off 矩阵

## 2. annotation 解析

- [x] 2.1 重写 `resource.go:41-66` 解析块：不设 = 不绑定（round-robin 记账与 `cube.numa_node` label 保留）；`"auto"` = 账本自动；合法 int = 指定 node（hint）；非法值 = auto + 警告留痕
- [x] 2.2 单测覆盖四种取值 × 多 node / 单 node 机器

## 3. 实测门数据源

- [x] 3.1 `pkg/numa` 新增每 node 实测读取，口径**排除可即时回收的页缓存**（候选：meminfo 推导"匿名+内核+脏页"或 per-cgroup 汇总，两套实现对数后定）；读取失败安全降级（实测门按不通过处理）
- [x] 3.2 实测读取实现为 1s TTL 进程内缓存（等效于心跳耦合且无跨包依赖）；单测（fixture 解析）

## 4. 双门账本与 node 选择

- [x] 4.1 账本结构：per-node 已绑定声明 VM 的 `HostMemQ` 累加，sandbox 删除/暂停时正确释放（对齐 `aggregateSandboxResources` 语义）
- [x] 4.2 准入函数：账面门（≤ node 物理 × 全局超卖比）+ 实测门（不可回收用量 + 请求 + 保底水位 ≤ node 物理）
- [x] 4.3 node 选择流程接入 `resource.go`（仅声明 VM 走账本）：放置评分 = 内存主键 + per-node CPU 承诺比次级键；指定 node hint → 换 node → 全满进降级链
- [x] 4.4 模板级亲和：粘滞键当前取 `GetSnapshotTemplateID()`（模板/运行时快照 id；增量派生模板的祖先 base 统一粘滞为后续精化）。原始目标 **快照 lineage 根**为粘滞键（增量派生模板共享祖先 base，键必须与共享单元对齐）；粘滞映射存 cubelet 本地 store，重启后从现网已绑定 VM 多数派反推；连续 N 次粘滞门失败触发重映射；开启时同 lineage 声明 VM 粘住同 node，node 满走降级链
- [x] 4.5 降级链：besteffort 照常部署不绑定并留痕（`Info.NumaDegraded/Reason`）；strict 以 `ErrorCode_PreConditionFailed` + `numa_capacity_insufficient:` 前缀返回（引擎对 PreConditionFailed 直返不 failover，engine.go:418 已核验；专用 proto 错误码待 regen）
- [x] 4.6 大 VM（规格 > 单 node 物理容量）不经账本直接走降级终点
- [x] 4.7 账本单元测试：边界值、双门组合矩阵、降级策略矩阵、恢复重过账本、模板亲和粘滞
- [x] 4.8 周期对账：v2 池 `AllGroups()` 枚举与账本比对，泄漏告警并修正

## 5. cgroup 绑定落地

- [x] 5.1 核验 v2 池 `numa<N>` 父组 cpuset 写入：核集经 `CgroupDisableCpusetList` 过滤；父层 `cgroup.subtree_control` 启用 cpuset 的委托链。**cgroup v1 宿主机分叉（验证机已实证语义）**：v1 子组创建后 cpus/mems/effective 全空、任务无法挂入——v1 handle 下 per-VM 子组创建时须显式回填 cpuset 或在 `numa<N>` 父组设 `cgroup.clone_children=1`（推荐，子组 mkdir 即拷贝父值）
- [x] 5.2 声明 VM 从对应 `numa<N>` 子池 `Get`，`MakeFullCgID` node 编码与创建/释放路径核验
- [x] 5.3 本机 pool 测试（sudo + 空 CI 环境变量跑法）：绑定断言读 `cpuset.cpus.effective`/`cpuset.mems.effective`；**确认生产宿主机 cgroup 版本**——v1 内核 handle 下文件名与继承语义不同（无 `.effective`），验收方法需按版本分叉

## 6. annotation 与 master 约定

- [x] 6.1 写接入文档：`cube.master.instance.numa_node` 的 auto/int 语义、hint 降级行为、模板亲和配置、宿主相对性（node 编号仅单机内有效）
- [ ] 6.2 master 侧透传**已代码核验现成**（`ConstructCubeletReq.checkAndGetAnnotation` 转发全部 `cube.master.` 前缀键，reject 名单不含 NUMA 键）；运行时联调随 8.1。**用户自指定路径（已验证可行）**：`NewSandbox.metadata` → CubeAPI 提升规则（仿 `HOSTDIR_MOUNT_KEY` 先例，白名单仅 NUMA 键、不可覆盖平台 annotation）→ master annotations；是否开放及暴露语义（建议仅 `auto`）待产品拍板

## 7. 暂停恢复

- [x] 7.1 pause→resume 链路核验：恢复按新 VM 重过双门账本（模板亲和开启时倾向粘回同 node）；无 node 可用时按策略降级；暂停释放与恢复准入的账本时序无泄漏

## 8. 真机验证与灰度

- [x] 8.0 机制级前置验证（2026-10-08 已完成于 4-node 验证机 192.168.25.138）：A1 三段证据（cpuset.mems 约束首碰含页缓存 / 跨 node 读共享缓存 100% 远程 / CoW+匿名页严格本地）+ A4 per-node 字段勘察（口径公式字段齐全）+ v1 cpuset 语义（空子组/clone_children/effective）；**端到端验收阻塞于该机无 CubeSandbox 部署**
- [ ] 8.1 多 NUMA 真机验收脚本（**机制级部分已于 2026-10-08 在 4-node 验证机 138 完成**：新增 `numa_integration_test.go` 原生跑在 138（cgroup v1/aarch64）——hint 绑定 node2、auto 装箱铺满 4 node、besteffort 降级 + strict 类型错误、**活进程** Mems_allowed/Cpus_allowed 断言、v2 池 effective cpuset 断言，24 测试全过；剩余为完整 VM 流程的 numastat 页落 node 与模板亲和场景）：cpuset `.effective`、vCPU `Cpus_allowed_list`（cpuset 继承）、`numastat` 页落 node（区分私有页/共享页）；覆盖指定 node / 换 node / 降级 / 模板亲和四场景（numastat 未装可用 `/proc/<pid>/numa_maps` 替代）
- [ ] 8.2 实测门口径与保底水位标定压测（声明/未声明混合负载，观察 node 水位、降级率、远程页占比、node 间 CPU 均衡度）
- [ ] 8.3 合入时总开关默认关闭；灰度节点开启 besteffort + 模板亲和观察；strict 由运维按节点启用
