// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func (s *service) setRequestResource(createInfo *workflow.CreateContext, reqInfo *cubebox.RunCubeSandboxRequest) error {
	var cpu int64
	var mem float64
	for _, c := range reqInfo.GetContainers() {
		cpuR, err := resource.ParseQuantity(c.GetResources().GetCpu())
		if err != nil {
			return fmt.Errorf("invalid cpu resource: %v", err)
		}
		cpu += cpuR.Value()

		memR, err := resource.ParseQuantity(c.GetResources().GetMem())
		if err != nil {
			return fmt.Errorf("invalid cpu resource: %v", err)
		}
		mem += float64(memR.Value())
	}
	createInfo.CPU = cpu
	createInfo.Memory = int64(math.Ceil(mem / 1024 / 1024 / 1024))
	createInfo.PCIMode = constants.PCIModePF
	// NumaNode here is bookkeeping only (round-robin or master-specified hint);
	// actual NUMA binding for annotated sandboxes is decided by the cgroup
	// plugin against the per-node ledger (see cgroup/numa_alloc.go).
	useRoundRobinNumaNode := true
	if reqInfo.GetAnnotations() != nil {

		if numaStr, ok := reqInfo.GetAnnotations()[constants.MasterAnnotationsNumaNode]; ok {
			if strings.TrimSpace(numaStr) == "auto" {
				// Binding intent: ledger picks the node later; keep the
				// round-robin value as the bookkeeping default.
				useRoundRobinNumaNode = true
			} else if numaNode, err := strconv.Atoi(strings.TrimSpace(numaStr)); err == nil {
				// Valid ids are 0..GetMaxNumaNodeId() inclusive — the old
				// `>= max` check rejected the top node (a latent bug that
				// never fired because the annotation had no producer).
				if numaNode < 0 || numaNode > numa.GetMaxNumaNodeId() {
					// Malformed value degrades to auto with a warning instead
					// of failing the create (strict only covers capacity).
					CubeLog.Warnf("numa annotation %q out of range, fallback to auto", numaStr)
					useRoundRobinNumaNode = true
				} else {
					createInfo.NumaNode = int32(numaNode)
					useRoundRobinNumaNode = false
				}
			} else {
				CubeLog.Warnf("invalid numa annotation %q, fallback to auto", numaStr)
				useRoundRobinNumaNode = true
			}
		}

		if v, ok := reqInfo.GetAnnotations()[constants.MasterAnnotationsPICMode]; ok {
			createInfo.PCIMode = v
		}
	}

	if useRoundRobinNumaNode {
		createInfo.NumaNode = s.getNextNumaNode()
	}
	return nil
}

func (s *service) getNextNumaNode() int32 {
	return int32(atomic.AddUint32(&s.numaNodeIndex, 1) % uint32(numa.GetNumaNodeCount()))
}
