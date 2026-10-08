// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/cgroups/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

var numaTestLock sync.Mutex

// cpusetFilePath resolves a cpuset file for a pool-v2 group on either cgroup
// version: v2 keeps controllers flat under the root, v1 nests them per
// controller (verified on a 4-node v1 host).
func cpusetFilePath(group, file string) string {
	group = strings.TrimPrefix(group, "/")
	if cgroups.Mode() == cgroups.Unified {
		return fmt.Sprintf("%s/%s/%s", handle.RootMountPoint, group, file)
	}
	return fmt.Sprintf("%s/cpuset/%s/%s", handle.RootMountPoint, group, file)
}

// TestPoolV2NumaCpusetEffective asserts the binding invariant the NUMA feature
// relies on: pool-v2 sandbox groups live under numa<n> and their EFFECTIVE
// cpuset equals the owning node (v2 inherits it from the parent group; on v1
// kernels createCgroup backfills it explicitly — design D3).
func TestPoolV2NumaCpusetEffective(t *testing.T) {
	utils.SkipCI(t)
	requireWritableCgroupFilesystem(t)

	ctx := context.Background()
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

	numaNodes := numa.GetAllNumaNodes()
	require.NotEmpty(t, numaNodes, "host exposes numa nodes")
	target := numaNodes[0]

	fullCgID, err := p.Get(ctx, "numa-cpuset-test", true, int32(target.NodeId))
	require.NoErrorf(t, err, "allocate pool-v2 cgroup")
	t.Cleanup(func() {
		p.Put(ctx, *fullCgID)
	})

	unified := cgroups.Mode() == cgroups.Unified
	memsSuffix, cpuSuffix := "cpuset.mems.effective", "cpuset.cpus.effective"
	if !unified {
		memsSuffix, cpuSuffix = "cpuset.mems", "cpuset.cpus"
	}

	readCgroupFile := func(suffix string) string {
		t.Helper()
		path := cpusetFilePath(MakeCgroupPathByID(*fullCgID), suffix)
		data, err := os.ReadFile(path)
		require.NoErrorf(t, err, "read %s", path)
		return strings.TrimSpace(string(data))
	}

	assert.Equal(t, strconv.Itoa(target.NodeId), readCgroupFile(memsSuffix),
		"effective mems must equal the owning node")

	gotCpus := readCgroupFile(cpuSuffix)
	if !unified {
		// v1 backfill writes the node cpulist verbatim.
		assert.Equal(t, target.Cpulist, gotCpus, "v1 backfilled cpus must equal the node cpulist")
		return
	}
	// v2 inheritance: every effective cpu must belong to the node's cores.
	for _, part := range strings.Split(gotCpus, ",") {
		bounds := strings.SplitN(part, "-", 2)
		start, err := strconv.Atoi(bounds[0])
		require.NoErrorf(t, err, "parse cpu range %q", part)
		end := start
		if len(bounds) == 2 {
			end, err = strconv.Atoi(bounds[1])
			require.NoErrorf(t, err, "parse cpu range %q", part)
		}
		for cpu := start; cpu <= end; cpu++ {
			assert.Truef(t, target.Cores[cpu], "effective cpu %d outside node %d", cpu, target.NodeId)
		}
	}
}
