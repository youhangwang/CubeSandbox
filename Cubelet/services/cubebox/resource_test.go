// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// setRequestResource only performs bookkeeping for the numa annotation; the
// binding decision itself lives in the cgroup plugin. On any Linux host node0
// exists, so "0" is a valid hint and out-of-range values degrade to auto.
func TestSetRequestResourceNumaAnnotation(t *testing.T) {
	s := &service{}

	run := func(ann map[string]string) (*workflow.CreateContext, error) {
		ctx := &workflow.CreateContext{}
		ctx.ReqInfo = &cubebox.RunCubeSandboxRequest{
			Containers:  []*cubebox.ContainerConfig{},
			Annotations: ann,
		}
		err := s.setRequestResource(ctx, ctx.ReqInfo)
		return ctx, err
	}

	// absent → round-robin bookkeeping, no error
	ctx, err := run(nil)
	if err != nil {
		t.Fatalf("absent annotation: %v", err)
	}
	if ctx.NumaNode != 0 {
		t.Errorf("absent: NumaNode = %d, want round-robin 0", ctx.NumaNode)
	}

	// auto → still bookkeeping round-robin (binding decided in cgroup plugin)
	ctx, err = run(map[string]string{constants.MasterAnnotationsNumaNode: "auto"})
	if err != nil {
		t.Fatalf("auto: %v", err)
	}

	// valid hint "0"
	ctx, err = run(map[string]string{constants.MasterAnnotationsNumaNode: "0"})
	if err != nil {
		t.Fatalf("hint 0: %v", err)
	}
	if ctx.NumaNode != 0 {
		t.Errorf("hint 0: NumaNode = %d, want 0", ctx.NumaNode)
	}

	// out-of-range and garbage degrade to auto with a warning, NOT an error
	for _, bad := range []string{"999", "abc", "-1"} {
		ctx, err = run(map[string]string{constants.MasterAnnotationsNumaNode: bad})
		if err != nil {
			t.Errorf("annotation %q should degrade, got error: %v", bad, err)
		}
	}
}
