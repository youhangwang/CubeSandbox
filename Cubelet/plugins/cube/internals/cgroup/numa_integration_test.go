// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/containerd/cgroups/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

// TestNumaAffinityMultiNodeIntegration exercises the NUMA affinity ledger on a
// real multi-NUMA-node host (openspec add-numa-affinity task 8.1, mechanism
// level): bin-packing across nodes, hint binding, besteffort/strict degrade
// and a live process actually confined by the allocated cgroup. Skips on
// single-node hosts and in CI.
func TestNumaAffinityMultiNodeIntegration(t *testing.T) {
	utils.SkipCI(t)
	requireWritableCgroupFilesystem(t)

	ctx := context.Background()
	nodes := numa.GetAllNumaNodes()
	if len(nodes) < 2 {
		t.Skipf("requires >= 2 numa nodes, host has %d", len(nodes))
	}

	numaTestLock.Lock()
	defer numaTestLock.Unlock()

	db, err := utils.NewCubeStoreExt(filepath.Join(t.TempDir(), "db"), "meta.db", 10, nil)
	require.NoErrorf(t, err, "create db")
	p := &cgPool{
		initialSize:  10,
		poolV1Handle: getDefaultCgroupHandle(1),
		poolV2Handle: getDefaultCgroupHandle(2),
		db:           db,
	}
	require.NoErrorf(t, p.init(), "init pool")

	const gb = 1 << 30
	const vmMem = int64(100) * gb // committed pressure of one "VM"
	src := sysfsCapacitySource{}
	params := numaAllocParams{
		Policy:       numaPolicyBestEffort,
		ReserveBytes: 4 * gb,
		MemRatio:     1.25,
	}

	var ledger []numaLedgerEntry
	var allocated []uint32
	t.Cleanup(func() {
		for _, id := range allocated {
			p.Put(ctx, id)
		}
	})

	// bindOne runs the planner against the real nodes/sysfs, then allocates a
	// real pool-v2 cgroup on the chosen node and records it in the ledger.
	bindOne := func(intent numaIntent, specNode int32) numaAllocResult {
		res, err := planNumaAllocation(nodes, ledger, src, vmMem, 0, intent, specNode, "", false, params)
		require.NoErrorf(t, err, "plan")
		if !res.Bound {
			return res
		}
		id, err := p.Get(ctx, fmt.Sprintf("numa-integ-%d", len(allocated)), true, res.Node)
		require.NoErrorf(t, err, "allocate on node %d", res.Node)
		require.Equalf(t, res.Node, func() int32 { n, _ := CgroupID2NumaID(*id); return int32(n) }(),
			"allocated cgroup must encode the chosen node")
		ledger = append(ledger, numaLedgerEntry{CgID: *id, HostMemQ: vmMem})
		allocated = append(allocated, *id)
		assertCgroupBoundToNode(t, p, *id, int(res.Node))
		return res
	}

	// A. hint node[2]: binds exactly there on a fresh ledger.
	require.GreaterOrEqual(t, len(nodes), 3, "hint subtest wants node 2")
	hintNode := int32(nodes[2].NodeId)
	res := bindOne(numaIntentNode, hintNode)
	require.True(t, res.Bound, "hint bind")
	require.Equal(t, hintNode, res.Node, "hint must bind the requested node")

	// B. auto bin-packing fills the remaining nodes: with ratio 1.25 the
	// per-node committed budget is ~1.25×132GB ≈ 165GB, so a second 100GB VM
	// never fits beside the first — every node hosts exactly one.
	seen := map[int32]bool{hintNode: true}
	for i := 0; i < len(nodes)-1; i++ {
		res := bindOne(numaIntentAuto, 0)
		require.Truef(t, res.Bound, "auto bind %d: %+v", i, res)
		require.Falsef(t, seen[res.Node], "auto re-picked committed node %d", res.Node)
		seen[res.Node] = true
	}
	assert.Len(t, seen, len(nodes), "bin-packing spread VMs over every node")

	// C. capacity exhausted: besteffort degrades, strict fails typed.
	degradeRes, degradeErr := planNumaAllocation(nodes, ledger, src, vmMem, 0, numaIntentAuto, 0, "", false, params)
	require.NoError(t, degradeErr)
	assert.Falsef(t, degradeRes.Bound, "expected besteffort degrade, got %+v", degradeRes)
	assert.NotEmpty(t, degradeRes.Reason, "degrade must leave a reason")

	strictParams := params
	strictParams.Policy = numaPolicyStrict
	_, strictErr := planNumaAllocation(nodes, ledger, src, vmMem, 0, numaIntentAuto, 0, "", false, strictParams)
	require.Truef(t, errors.Is(strictErr, ErrNumaCapacityInsufficient), "strict error = %v", strictErr)

	// D. live process confinement: a real task moved into an allocated group
	// must be restricted to the owning node's CPUs and memory.
	liveID, err := p.Get(ctx, "numa-integ-live", true, int32(nodes[0].NodeId))
	require.NoErrorf(t, err, "allocate live-test group")
	allocated = append(allocated, *liveID)

	sleep := exec.Command("sleep", "60")
	require.NoError(t, sleep.Start())
	t.Cleanup(func() {
		_ = sleep.Process.Kill()
		_ = sleep.Wait()
	})

	group := MakeCgroupPathByID(*liveID)
	require.NoErrorf(t, p.poolV2Handle.AddProc(group, uint64(sleep.Process.Pid)),
		"add live pid to %s", group)

	status := readProcStatus(t, sleep.Process.Pid)
	assert.Equal(t, strconv.Itoa(nodes[0].NodeId), status["Mems_allowed_list"],
		"live process must be memory-confined to the owning node")
	assertCpusWithinNode(t, status["Cpus_allowed_list"], nodes[0], "live process cpus")
}

func readProcStatus(t *testing.T, pid int) map[string]string {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	require.NoErrorf(t, err, "read /proc/%d/status", pid)
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

func assertCpusWithinNode(t *testing.T, allowed string, node numa.NumaNode, msg string) {
	t.Helper()
	for _, part := range strings.Split(allowed, ",") {
		bounds := strings.SplitN(part, "-", 2)
		start, err := strconv.Atoi(bounds[0])
		require.NoErrorf(t, err, "parse %q", part)
		end := start
		if len(bounds) == 2 {
			end, err = strconv.Atoi(bounds[1])
			require.NoErrorf(t, err, "parse %q", part)
		}
		for cpu := start; cpu <= end; cpu++ {
			assert.Truef(t, node.Cores[cpu], "%s: cpu %d outside node %d", msg, cpu, node.NodeId)
		}
	}
}

// assertCgroupBoundToNode reads the cpuset files of an allocated group and
// asserts they equal the owning node (v2: .effective inheritance; v1: the
// explicit backfill values).
func assertCgroupBoundToNode(t *testing.T, p *cgPool, fullCgID uint32, nodeID int) {
	t.Helper()
	unified := cgroups.Mode() == cgroups.Unified
	group := MakeCgroupPathByID(fullCgID)
	read := func(suffix string) string {
		t.Helper()
		path := cpusetFilePath(group, suffix)
		data, err := os.ReadFile(path)
		require.NoErrorf(t, err, "read %s", path)
		return strings.TrimSpace(string(data))
	}
	mems, cpus := "cpuset.mems.effective", "cpuset.cpus.effective"
	if !unified {
		mems, cpus = "cpuset.mems", "cpuset.cpus"
	}
	assert.Equal(t, strconv.Itoa(nodeID), read(mems), "%s mems", group)
	if !unified {
		node := numa.GetAllNumaNodes()[nodeID]
		assert.Equal(t, node.Cpulist, read(cpus), "%s cpus (v1 backfill)", group)
	}
}
