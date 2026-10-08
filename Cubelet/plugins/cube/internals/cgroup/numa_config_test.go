// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	dynamConf "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
)

// allocateNumaNode with no dynamic config must degrade instead of panicking.
func TestAllocateNumaNodeNilConfig(t *testing.T) {
	l := &CgPlugin{}
	res, err := l.allocateNumaNode(numaIntentAuto, 0, "", 1024, 1000)
	if err != nil {
		t.Fatalf("nil config should degrade, not error: %v", err)
	}
	if res.Bound || !res.Degraded {
		t.Errorf("nil config result = %+v, want degraded", res)
	}
}

// allocateNumaNode reads the real dynamic config: exercise the switch + strict
// policy plumbing with a temp config file. Gates run against the host's real
// sysfs (any Linux has node0), so an auto request on a healthy host binds.
func TestAllocateNumaNodeWithConfig(t *testing.T) {
	dir := t.TempDir()
	yaml := filepath.Join(dir, "dyn.yaml")
	content := `common:
  numa_bind_enabled: true
  numa_alloc_policy: "strict"
  numa_reserve_mem_mb: 64
  numa_mem_ratio: 1.25
  numa_template_affinity: false
`
	if err := os.WriteFile(yaml, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := dynamConf.Init(yaml, false); err != nil {
		t.Fatalf("config init: %v", err)
	}
	// restore the zero-value config so other tests in the package are unaffected
	defer dynamConf.Init("", true)

	l := &CgPlugin{}
	res, err := l.allocateNumaNode(numaIntentAuto, 0, "", 64*1024*1024, 1000)
	if err != nil {
		t.Fatalf("strict auto alloc: %v", err)
	}
	// The scored selection prefers the node with the lowest committed/total
	// ratio; totals differ per node so only "some node bound" is asserted.
	if !res.Bound || res.Node < 0 || res.Node >= int32(numa.GetNumaNodeCount()) {
		t.Errorf("result = %+v, want bound to a valid node", res)
	}

	// strict + impossible request → typed error
	_, err = l.allocateNumaNode(numaIntentAuto, 0, "", 1<<50, 0)
	if !errors.Is(err, ErrNumaCapacityInsufficient) {
		t.Errorf("impossible request error = %v, want ErrNumaCapacityInsufficient", err)
	}
}
