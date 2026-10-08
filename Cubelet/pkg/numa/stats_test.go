// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package numa

import (
	"testing"
)

func TestParseNodeMeminfo(t *testing.T) {
	fixture := `Node 0 MemTotal:       131580608 kB
Node 0 MemFree:       90123456 kB
Node 0 MemUsed:       41457152 kB
Node 0 AnonPages:      1234567 kB
Node 0 Shmem:            22222 kB
Node 0 FilePages:       444444 kB
Node 0 SUnreclaim:       33333 kB
Node 0 Dirty:             1111 kB
Node 0 PageTables:        5555 kB
Node 0 KernelStack:        666 kB
Node 0 Unevictable:        777 kB
Node 1 MemTotal:       132115952 kB
Node 1 AnonPages:      9999999 kB
`
	stats, err := parseNodeMeminfo(0, []byte(fixture))
	if err != nil {
		t.Fatalf("parse node0: %v", err)
	}
	kb := func(v uint64) uint64 { return v / 1024 }
	if kb(stats.MemTotal) != 131580608 {
		t.Errorf("MemTotal = %d kB, want 131580608", kb(stats.MemTotal))
	}
	if kb(stats.AnonPages) != 1234567 {
		t.Errorf("AnonPages = %d kB, want 1234567", kb(stats.AnonPages))
	}
	if kb(stats.FilePages) != 444444 {
		t.Errorf("FilePages = %d kB, want 444444", kb(stats.FilePages))
	}
	// non-reclaimable = AnonPages + Shmem + SUnreclaim + Dirty + PageTables + KernelStack
	wantNR := uint64(1234567+22222+33333+1111+5555+666) * 1024
	if stats.NonReclaimable != wantNR {
		t.Errorf("NonReclaimable = %d, want %d", stats.NonReclaimable, wantNR)
	}
	if stats.FilePages != 0 && stats.NonReclaimable >= stats.MemTotal {
		// sanity: non-reclaimable must stay below total on this fixture
		t.Errorf("NonReclaimable %d exceeds MemTotal %d", stats.NonReclaimable, stats.MemTotal)
	}

	// foreign node lines must not leak into node1's stats
	stats1, err := parseNodeMeminfo(1, []byte(fixture))
	if err != nil {
		t.Fatalf("parse node1: %v", err)
	}
	if kb(stats1.AnonPages) != 9999999 {
		t.Errorf("node1 AnonPages = %d kB, want 9999999", kb(stats1.AnonPages))
	}
	if kb(stats1.FilePages) != 0 {
		t.Errorf("node1 FilePages should be 0 (no node1 line), got %d kB", kb(stats1.FilePages))
	}

	// garbage without MemTotal is an error, not a zero-capacity pass-through
	if _, err := parseNodeMeminfo(0, []byte("hello world\n")); err == nil {
		t.Error("parse garbage should fail")
	}
}
