// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
)

// ---------------------------------------------------------------------------
// fakes

type fakeCapacity map[int]struct {
	total uint64
	used  uint64
	err   error
}

func (f fakeCapacity) TotalBytes(node int) (uint64, error) {
	c, ok := f[node]
	if !ok || c.err != nil {
		return 0, fmt.Errorf("node %d capacity unknown", node)
	}
	return c.total, nil
}

func (f fakeCapacity) NonReclaimableBytes(node int) (uint64, error) {
	c, ok := f[node]
	if !ok || c.err != nil {
		return 0, fmt.Errorf("node %d usage unknown", node)
	}
	return c.used, nil
}

func node(id int, cores int) numa.NumaNode {
	c := map[int]bool{}
	for i := 0; i < cores; i++ {
		c[i] = true
	}
	return numa.NumaNode{NodeId: id, Cores: c}
}

// twoNodes: node0/node1, each 128GB with 8GB non-reclaimable baseline.
func twoNodes() ([]numa.NumaNode, fakeCapacity) {
	const gb = 1024 * 1024 * 1024
	nodes := []numa.NumaNode{node(0, 8), node(1, 8)}
	caps := fakeCapacity{
		0: {total: 128 * gb, used: 8 * gb},
		1: {total: 128 * gb, used: 8 * gb},
	}
	return nodes, caps
}

func defaultParams() numaAllocParams {
	return numaAllocParams{
		Policy:           numaPolicyBestEffort,
		ReserveBytes:     4 * 1024 * 1024 * 1024,
		MemRatio:         1.25,
		TemplateAffinity: true,
	}
}

// MakeFullCgID for ledger fixtures: v2 entry on a node.
func v2Entry(node int32, id uint16, memQ, cpuQ int64, sticky string) numaLedgerEntry {
	return numaLedgerEntry{
		CgID:      MakeFullCgID(id, true, node),
		HostMemQ:  memQ,
		HostCpuQ:  cpuQ,
		StickyKey: sticky,
	}
}

// ---------------------------------------------------------------------------
// intent parsing

func TestParseNumaIntent(t *testing.T) {
	const key = "cube.master.instance.numa_node"
	cases := []struct {
		ann      map[string]string
		want     numaIntent
		wantNode int32
	}{
		{nil, numaIntentNone, 0},
		{map[string]string{"other": "1"}, numaIntentNone, 0},
		{map[string]string{key: ""}, numaIntentNone, 0},
		{map[string]string{key: "auto"}, numaIntentAuto, 0},
		{map[string]string{key: " auto "}, numaIntentAuto, 0},
		{map[string]string{key: "1"}, numaIntentNode, 1},
		{map[string]string{key: "abc"}, numaIntentAuto, 0}, // invalid → auto
		{map[string]string{key: "-3"}, numaIntentAuto, 0},  // invalid → auto
	}
	for i, tc := range cases {
		got, node := parseNumaIntent(tc.ann)
		if got != tc.want || node != tc.wantNode {
			t.Errorf("case %d: got (%v,%d), want (%v,%d)", i, got, node, tc.want, tc.wantNode)
		}
	}
}

// ---------------------------------------------------------------------------
// ledger value encoding

func TestNumaLedgerValueRoundTrip(t *testing.T) {
	entry := v2Entry(1, 42, 2048*1024*1024, 2000, "tpl-1")
	decoded, err := parseNumaLedgerValue(encodeNumaLedgerEntry(entry))
	if err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if decoded != entry {
		t.Errorf("round trip mismatch: %+v vs %+v", decoded, entry)
	}

	legacy, err := parseNumaLedgerValue([]byte("12345"))
	if err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if legacy.CgID != 12345 || legacy.HostMemQ != 0 {
		t.Errorf("legacy decode = %+v", legacy)
	}

	if _, err := parseNumaLedgerValue([]byte("not-a-number")); err == nil {
		t.Error("garbage should fail to parse")
	}
}

// ---------------------------------------------------------------------------
// ledger folding and sticky majority

func TestFoldNumaLedger(t *testing.T) {
	entries := []numaLedgerEntry{
		v2Entry(0, 1, 2*1024*1024*1024, 2000, "tpl-a"),
		v2Entry(0, 2, 3*1024*1024*1024, 1000, ""),
		v2Entry(1, 1, 1*1024*1024*1024, 500, "tpl-a"),
		{CgID: 7}, // legacy v1 entry — must be skipped
	}
	got := foldNumaLedger(entries)
	if len(got) != 2 {
		t.Fatalf("nodes = %d, want 2", len(got))
	}
	if got[0].Mem != 5*1024*1024*1024 || got[0].Cpu != 3000 || got[0].Num != 2 {
		t.Errorf("node0 commit = %+v", got[0])
	}
	if got[1].Mem != 1*1024*1024*1024 || got[1].Num != 1 {
		t.Errorf("node1 commit = %+v", got[1])
	}
}

func TestStickyNodeOf(t *testing.T) {
	entries := []numaLedgerEntry{
		v2Entry(0, 1, 1, 1, "tpl-a"),
		v2Entry(0, 2, 1, 1, "tpl-a"),
		v2Entry(1, 1, 1, 1, "tpl-a"),
		v2Entry(1, 2, 1, 1, "tpl-b"),
		v2Entry(1, 3, 1, 1, "tpl-b"), // tie with node0 for tpl-b? no: 2 vs 0
	}
	if n := stickyNodeOf(entries, "tpl-a"); n == nil || *n != 0 {
		t.Errorf("tpl-a sticky = %v, want node 0", n)
	}
	if n := stickyNodeOf(entries, "tpl-b"); n == nil || *n != 1 {
		t.Errorf("tpl-b sticky = %v, want node 1", n)
	}
	if n := stickyNodeOf(entries, "tpl-unknown"); n != nil {
		t.Errorf("unknown sticky = %v, want nil", n)
	}
	if n := stickyNodeOf(entries, ""); n != nil {
		t.Errorf("empty key sticky = %v, want nil", n)
	}
}

// ---------------------------------------------------------------------------
// allocation scenarios

func TestPlanNumaAllocationAutoPicksLessCommitted(t *testing.T) {
	nodes, caps := twoNodes()
	ledger := []numaLedgerEntry{v2Entry(0, 1, 40*1024*1024*1024, 0, "")}
	res, err := planNumaAllocation(nodes, ledger, caps, 2*1024*1024*1024, 0,
		numaIntentAuto, 0, "", false, defaultParams())
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if !res.Bound || res.Node != 1 {
		t.Errorf("result = %+v, want bound node 1 (node0 more committed)", res)
	}
}

func TestPlanNumaAllocationCommittedGateBlocks(t *testing.T) {
	nodes, caps := twoNodes()
	const gb = 1024 * 1024 * 1024
	// node1 commits up to its 1.25 ratio: 128GB*1.25 = 160GB budget
	ledger := []numaLedgerEntry{v2Entry(1, 1, 159*gb, 0, "")}
	// request would push node1 over the ratio → must land on node0
	res, err := planNumaAllocation(nodes, ledger, caps, 2*gb, 0,
		numaIntentAuto, 0, "", false, defaultParams())
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if !res.Bound || res.Node != 0 {
		t.Errorf("result = %+v, want bound node 0", res)
	}
}

func TestPlanNumaAllocationMeasuredGateBlocks(t *testing.T) {
	nodes, _ := twoNodes()
	const gb = 1024 * 1024 * 1024
	// node1 physically almost full (non-reclaimable 126GB): measured gate
	// 126+2+4 > 128 → node1 unusable even though the ledger is empty.
	caps := fakeCapacity{
		0: {total: 128 * gb, used: 8 * gb},
		1: {total: 128 * gb, used: 126 * gb},
	}
	res, err := planNumaAllocation(nodes, nil, caps, 2*gb, 0,
		numaIntentAuto, 0, "", false, defaultParams())
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if !res.Bound || res.Node != 0 {
		t.Errorf("result = %+v, want bound node 0", res)
	}
}

func TestPlanNumaAllocationHintNode(t *testing.T) {
	nodes, caps := twoNodes()
	const gb = 1024 * 1024 * 1024

	// hint passes → exactly that node
	res, err := planNumaAllocation(nodes, nil, caps, 2*gb, 0,
		numaIntentNode, 1, "", false, defaultParams())
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if !res.Bound || res.Node != 1 {
		t.Errorf("hint pass = %+v, want node 1", res)
	}

	// hint node full → falls to the other node (换 node)
	ledger := []numaLedgerEntry{v2Entry(1, 1, 159*gb, 0, "")}
	res, err = planNumaAllocation(nodes, ledger, caps, 2*gb, 0,
		numaIntentNode, 1, "", false, defaultParams())
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if !res.Bound || res.Node != 0 {
		t.Errorf("hint fallback = %+v, want node 0", res)
	}

	// hint node not on this host → falls to any fitting node
	res, err = planNumaAllocation(nodes, nil, caps, 2*gb, 0,
		numaIntentNode, 7, "", false, defaultParams())
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if !res.Bound {
		t.Errorf("absent hint = %+v, want bound", res)
	}
}

func TestPlanNumaAllocationAllFull(t *testing.T) {
	nodes, caps := twoNodes()
	const gb = 1024 * 1024 * 1024
	full := []numaLedgerEntry{
		v2Entry(0, 1, 159*gb, 0, ""),
		v2Entry(1, 1, 159*gb, 0, ""),
	}

	// besteffort: degrade, no error
	res, err := planNumaAllocation(nodes, full, caps, 2*gb, 0,
		numaIntentAuto, 0, "", false, defaultParams())
	if err != nil {
		t.Fatalf("besteffort alloc: %v", err)
	}
	if res.Bound || !res.Degraded || res.Reason == "" {
		t.Errorf("besteffort result = %+v, want degraded with reason", res)
	}

	// strict: typed error
	strict := defaultParams()
	strict.Policy = numaPolicyStrict
	_, err = planNumaAllocation(nodes, full, caps, 2*gb, 0,
		numaIntentAuto, 0, "", false, strict)
	if !errors.Is(err, ErrNumaCapacityInsufficient) {
		t.Errorf("strict error = %v, want ErrNumaCapacityInsufficient", err)
	}

	// oversized VM (bigger than any node) → same endpoints
	huge := int64(200) * gb
	res, err = planNumaAllocation(nodes, nil, caps, huge, 0,
		numaIntentAuto, 0, "", false, defaultParams())
	if err != nil {
		t.Fatalf("oversized alloc: %v", err)
	}
	if res.Bound || !res.Degraded {
		t.Errorf("oversized result = %+v, want degraded", res)
	}
}

func TestPlanNumaAllocationSticky(t *testing.T) {
	nodes, caps := twoNodes()
	const gb = 1024 * 1024 * 1024
	ledger := []numaLedgerEntry{
		v2Entry(0, 1, 2*gb, 0, "tpl-a"),
		v2Entry(0, 2, 2*gb, 0, "tpl-a"),
	}

	// sticky majority respected when it fits
	res, err := planNumaAllocation(nodes, ledger, caps, 2*gb, 0,
		numaIntentAuto, 0, "tpl-a", false, defaultParams())
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if !res.Bound || res.Node != 0 {
		t.Errorf("sticky = %+v, want node 0", res)
	}

	// sticky node full → other node (换 node), still bound
	full := append(ledger, v2Entry(0, 3, 155*gb, 0, "tpl-a"))
	res, err = planNumaAllocation(nodes, full, caps, 2*gb, 0,
		numaIntentAuto, 0, "tpl-a", false, defaultParams())
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if !res.Bound || res.Node != 1 {
		t.Errorf("sticky full = %+v, want node 1", res)
	}

	// override ignores sticky entirely
	res, err = planNumaAllocation(nodes, ledger, caps, 2*gb, 0,
		numaIntentAuto, 0, "tpl-a", true, defaultParams())
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if !res.Bound {
		t.Errorf("override = %+v, want bound", res)
	}
}

func TestBuildNumaAllocParamsDefaults(t *testing.T) {
	p := buildNumaAllocParams(true, "weird", 0, 0, false)
	if p.Policy != numaPolicyBestEffort || p.ReserveBytes != defaultNumaReserveMemMB*1024*1024 || p.MemRatio != defaultNumaMemRatio {
		t.Errorf("defaults = %+v", p)
	}
	p = buildNumaAllocParams(true, numaPolicyStrict, 8, 2.0, true)
	if p.Policy != numaPolicyStrict || p.ReserveBytes != 8*1024*1024 || p.MemRatio != 2.0 || !p.TemplateAffinity {
		t.Errorf("explicit = %+v", p)
	}
}
