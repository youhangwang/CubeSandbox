# Sandbox NUMA 亲和接入说明

> 对应 OpenSpec change：`openspec/changes/add-numa-affinity/`（proposal / specs / design / tasks）。
> 本文档面向调用方与运维：怎么声明、行为是什么、怎么配置、有哪些边界。

## 1. 用户怎么声明

在创建 sandbox 的请求 annotations 里设置 **`cube.master.instance.numa_node`**：

| 取值 | 语义 | 行为 |
|---|---|---|
| （不设） | 不参与 NUMA | 现有流程原样（v1 cgroup 池），不进 NUMA 账本 |
| `"auto"` | 自动选 node | cubelet 账本（账面门 + 实测门）挑一个最优 node 并绑定 |
| `"0"` / `"1"` / … | 指定 node（hint） | 该 node 双门通过 → 绑定它；不通过但有其他 node 通过 → 换绑；全满 → 降级链 |
| 非法值（`"abc"`、`"-1"`、超界数字） | 按 `auto` 处理 | 警告日志留痕，不影响创建（strict 只针对容量不足） |

**宿主相对性**：node 编号是**单台宿主机内部**的编号，跨宿主机没有全局含义。指定 `"1"` 只在
"这台 VM 最终落在的宿主机"上有意义；需要钉死宿主机请配合宿主级亲和一起使用。

## 2. 绑定后发生什么

声明 VM 的 shim+VMM 进程树被放进 v2 池的 `numa<N>` cgroup 组：

- `cpuset.mems` = 该 node → 内存页（含缺页触发的页缓存分配）只能来自该 node
- `cpuset.cpus` = 该 node 核集 → 全部线程（含 vCPU）出不了该 node
- 一期不做 per-vCPU 1:1 pinning（node 内精调留二期）

验收口径（多 NUMA 真机）：cgroup `.effective` 文件、vCPU `Cpus_allowed_list`、
`/proc/<pid>/numa_maps` 页落 node（区分私有页/共享页）。

**共享页缓存的诚实说明**：VM 恢复自模板时，未写过的 base 页是全体同模板 VM 共享的页缓存，
住在**首碰者**的 node 上。开启模板亲和（见下）可以让同模板 VM 粘住同一 node，使共享缓存与
绑定一致；未开启时，绑定 VM 读其他 node 首碰的共享缓存页属于预期行为（私有页/CoW 页始终本地）。

## 3. 运维配置（dynamicconf `common` 段）

```yaml
common:
  numa_bind_enabled: false          # 总开关，默认关。关时 annotation 被完全忽略
  numa_alloc_policy: "besteffort"   # besteffort(默认)：装不下照常部署不绑定（留痕）
                                    # strict：装不下创建失败（numa_capacity_insufficient），master 不换机重试
  numa_reserve_mem_mb: 4096         # 实测门保底水位：node 不可回收内存 + 请求 + 水位 ≤ node 物理内存
  numa_mem_ratio: 1.25              # 账面门比例，对齐全局配额比 5/4
  numa_template_affinity: false     # 同模板（快照 lineage）声明 VM 粘住同一 node
```

建议灰度路径：默认关 → 多 NUMA 机器验证 → 灰度节点开 `besteffort` + `template_affinity` →
按需对特定节点开 `strict`。回滚 = 关总开关。

**未声明 VM 恒走 v1 池**，cgroup 位置与行为零变化（总开关只影响声明 VM 的池选择）。

## 4. 透传链路与谁可以写这个 annotation

- **master → cubelet**：现成。master 的 `ConstructCubeletReq` 转发所有 `cube.master.` 前缀
  annotation（reject 名单只含快照类键），**master 侧零改动**。
- **内部调用方 / master 策略**：直接在请求 annotations 里带 `cube.master.instance.numa_node` 即可。
- **终端用户（经 CubeAPI）**：CubeAPI 目前按白名单拼装 annotations；开放用户自指定需在
  `create_sandbox` 的 metadata→annotation 提升中加一条 NUMA 键规则（仿 `HOSTDIR_MOUNT_KEY`
  先例，白名单式、不可覆盖平台 annotation）。是否开放、暴露 `auto` 还是完整 node 编号，为
  产品决定（建议对外仅 `auto`）。

## 5. 已知边界

- **pause→resume**：恢复是新 VM，重新过账本，不保证回到原 node（模板亲和开启时倾向粘回）。
- **模板亲和的键**：模板快照 id；增量派生模板共享祖先 base 的统一粘滞（lineage 根）是后续精化。
- **busy 宿主机**：宿主被未声明 VM 物理吃满时，实测门双 node 全不过 → 声明 VM 全部
  besteffort 不绑定（安全优先，属预期；降级率是灰度期一等指标）。
- **cgroup v1 宿主机**：v2 池子组 cpuset 由代码显式回填（已实证 v1 子组不自动继承），行为一致。
- 绑定 VM 的 `memory.max` 是否写入遵循全局 `disable_host_cgroup` 开关，NUMA 特性不改变它。
