// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dynamConf "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// NUMA affinity allocation (openspec/changes/add-numa-affinity).
//
// Binding is opt-in: only sandboxes carrying the cube.master.instance.numa_node
// annotation ("auto" or a node id) enter the per-node ledger. The ledger is
// derived from this plugin's sandbox bucket — every pool-v2 allocation stores a
// numaLedgerEntry, so accounting survives restarts and pause releases the
// entry when the cgroup is destroyed. Admission runs two gates:
//
//	committed gate:  Σ HostMemQ(bound VMs on node) + request ≤ nodeMem × ratio
//	measured gate:   nonReclaimable(node) + request + reserve ≤ nodeMem
//
// The measured gate counts only non-reclaimable memory (shared template page
// cache is excluded) so it also sees the footprint of unbound VMs.

const (
	numaPolicyBestEffort = "besteffort"
	numaPolicyStrict     = "strict"

	numaStickyRemapThreshold = 3

	defaultNumaMemRatio     = 1.25
	defaultNumaReserveMemMB = 4096

	// numaCapErrPrefix marks strict-mode failures over the wire; callers match
	// this prefix because a dedicated proto error code is not available yet.
	numaCapErrPrefix = "numa_capacity_insufficient"
)

// ErrNumaCapacityInsufficient is the in-process sentinel for strict-mode
// allocation failures (use errors.Is; the wire error carries
// numaCapErrPrefix as its message prefix).
var ErrNumaCapacityInsufficient = errors.New(numaCapErrPrefix)

type numaIntent int

const (
	numaIntentNone numaIntent = iota
	numaIntentAuto
	numaIntentNode
)

// parseNumaIntent reads the cube.master.instance.numa_node annotation.
// Absent → none; "auto" → ledger picks; numeric → hint node; any other value
// degrades to auto (only capacity is allowed to fail a create).
func parseNumaIntent(annotations map[string]string) (numaIntent, int32) {
	if annotations == nil {
		return numaIntentNone, 0
	}
	raw, ok := annotations["cube.master.instance.numa_node"]
	if !ok {
		return numaIntentNone, 0
	}
	switch v := strings.TrimSpace(raw); {
	case v == "":
		return numaIntentNone, 0
	case v == "auto":
		return numaIntentAuto, 0
	default:
		if node, err := strconv.Atoi(v); err == nil && node >= 0 {
			return numaIntentNode, int32(node)
		}
		return numaIntentAuto, 0
	}
}

// numaLedgerEntry is persisted in the sandbox bucket for every pool-v2
// allocation. Legacy entries (bare decimal cgroup id, pool v1) are still
// parsed so restart recovery and destroy keep working across upgrades.
type numaLedgerEntry struct {
	CgID      uint32 `json:"cgid"`
	HostMemQ  int64  `json:"host_mem_q"` // bytes
	HostCpuQ  int64  `json:"host_cpu_q"` // milli-cores
	StickyKey string `json:"sticky_key,omitempty"`
}

func encodeNumaLedgerEntry(entry numaLedgerEntry) []byte {
	data, err := json.Marshal(entry)
	if err != nil {
		// Marshal of an all-numeric struct cannot fail; fall back to the
		// legacy format rather than failing the create.
		return []byte(strconv.Itoa(int(entry.CgID)))
	}
	return data
}

// parseNumaLedgerValue accepts both the JSON ledger entry and the legacy bare
// decimal cgroup id.
func parseNumaLedgerValue(raw []byte) (numaLedgerEntry, error) {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "{") {
		var entry numaLedgerEntry
		if err := json.Unmarshal([]byte(s), &entry); err != nil {
			return numaLedgerEntry{}, err
		}
		return entry, nil
	}
	id, err := strconv.Atoi(s)
	if err != nil {
		return numaLedgerEntry{}, fmt.Errorf("unknown cgroup db value %q: %w", s, err)
	}
	return numaLedgerEntry{CgID: uint32(id)}, nil
}

// numaCapacitySource abstracts per-node capacity so the selection core can be
// tested without sysfs.
type numaCapacitySource interface {
	TotalBytes(node int) (uint64, error)
	NonReclaimableBytes(node int) (uint64, error)
}

type sysfsCapacitySource struct{}

func (sysfsCapacitySource) TotalBytes(node int) (uint64, error) {
	stats, err := numa.GetNodeMemStats(node)
	if err != nil {
		return 0, err
	}
	return stats.MemTotal, nil
}

func (sysfsCapacitySource) NonReclaimableBytes(node int) (uint64, error) {
	stats, err := numa.GetNodeMemStats(node)
	if err != nil {
		return 0, err
	}
	return stats.NonReclaimable, nil
}

// numaAllocParams carries the normalized config for one allocation decision.
type numaAllocParams struct {
	Policy           string
	ReserveBytes     uint64
	MemRatio         float64
	TemplateAffinity bool
}

func (p numaAllocParams) strict() bool { return p.Policy == numaPolicyStrict }

type numaAllocResult struct {
	Bound    bool
	Node     int32
	Degraded bool
	Reason   string
}

type numaNodeCommit struct {
	Mem int64 // bytes
	Cpu int64 // milli-cores
	Num int   // bound sandbox count (sticky majority)
}

// buildNumaAllocParams normalizes config into allocation params.
func buildNumaAllocParams(bindEnabled bool, policy string, reserveMB uint64, ratio float64, templateAffinity bool) numaAllocParams {
	if policy != numaPolicyStrict {
		policy = numaPolicyBestEffort
	}
	if reserveMB == 0 {
		reserveMB = defaultNumaReserveMemMB
	}
	if ratio <= 0 {
		ratio = defaultNumaMemRatio
	}
	return numaAllocParams{
		Policy:           policy,
		ReserveBytes:     reserveMB * 1024 * 1024,
		MemRatio:         ratio,
		TemplateAffinity: templateAffinity,
	}
}

// foldNumaLedger aggregates pool-v2 ledger entries per node. Legacy (v1)
// entries are skipped: unbound VMs do not enter the ledger.
func foldNumaLedger(entries []numaLedgerEntry) map[int32]*numaNodeCommit {
	committed := map[int32]*numaNodeCommit{}
	for _, entry := range entries {
		if !IsPoolV2ID(entry.CgID) {
			continue
		}
		node, err := CgroupID2NumaID(entry.CgID)
		if err != nil {
			continue
		}
		c, ok := committed[int32(node)]
		if !ok {
			c = &numaNodeCommit{}
			committed[int32(node)] = c
		}
		c.Mem += entry.HostMemQ
		c.Cpu += entry.HostCpuQ
		c.Num++
	}
	return committed
}

// stickyNodeOf returns the node holding the majority of bound VMs for a
// sticky key (template snapshot lineage), or nil when the key is unknown.
func stickyNodeOf(entries []numaLedgerEntry, stickyKey string) *int32 {
	if stickyKey == "" {
		return nil
	}
	counts := map[int32]int{}
	for _, entry := range entries {
		if entry.StickyKey == stickyKey && IsPoolV2ID(entry.CgID) {
			if node, err := CgroupID2NumaID(entry.CgID); err == nil {
				counts[int32(node)]++
			}
		}
	}
	best := int32(-1)
	bestCount := 0
	for node, count := range counts {
		if count > bestCount || (count == bestCount && best >= 0 && node < best) {
			best = node
			bestCount = count
		}
	}
	if best < 0 {
		return nil
	}
	return &best
}

// planNumaAllocation picks the node for one declared sandbox. It never fails
// for besteffort — an unallocatable request yields Bound=false with a reason —
// and returns ErrNumaCapacityInsufficient for strict.
func planNumaAllocation(
	nodes []numa.NumaNode,
	ledger []numaLedgerEntry,
	src numaCapacitySource,
	memQ, cpuQ int64,
	intent numaIntent,
	specNode int32,
	stickyKey string,
	stickyOverride bool, // sticky failed too often; ignore it this round
	params numaAllocParams,
) (numaAllocResult, error) {
	committed := foldNumaLedger(ledger)

	passes := func(node numa.NumaNode) (bool, string) {
		total, err := src.TotalBytes(node.NodeId)
		if err != nil || total == 0 {
			return false, fmt.Sprintf("node %d capacity unknown", node.NodeId)
		}
		measured, err := src.NonReclaimableBytes(node.NodeId)
		if err != nil {
			return false, fmt.Sprintf("node %d usage unknown", node.NodeId)
		}
		mem := committed[int32(node.NodeId)]
		var committedMem int64
		if mem != nil {
			committedMem = mem.Mem
		}
		if float64(committedMem+memQ) > float64(total)*params.MemRatio {
			return false, fmt.Sprintf("node %d committed gate: %d+%d > %.2f×%d",
				node.NodeId, committedMem, memQ, params.MemRatio, total)
		}
		if uint64(measured)+uint64(memQ)+params.ReserveBytes > total {
			return false, fmt.Sprintf("node %d measured gate: %d+%d+%d > %d",
				node.NodeId, measured, memQ, params.ReserveBytes, total)
		}
		return true, ""
	}

	trySticky := func() (numaAllocResult, bool) {
		if !params.TemplateAffinity || stickyOverride {
			return numaAllocResult{}, false
		}
		sticky := stickyNodeOf(ledger, stickyKey)
		if sticky == nil {
			return numaAllocResult{}, false
		}
		for _, node := range nodes {
			if int32(node.NodeId) != *sticky {
				continue
			}
			if ok, reason := passes(node); ok {
				return numaAllocResult{Bound: true, Node: *sticky}, true
			} else {
				return numaAllocResult{Degraded: true, Reason: reason}, true
			}
		}
		return numaAllocResult{}, false
	}

	// Hint node first: bind it when it fits, otherwise fall through to the
	// scored selection over the remaining nodes.
	var reasons []string
	hintEvaluated := false
	if intent == numaIntentNode {
		for _, node := range nodes {
			if int32(node.NodeId) != specNode {
				continue
			}
			hintEvaluated = true
			if ok, reason := passes(node); ok {
				return numaAllocResult{Bound: true, Node: specNode}, nil
			} else {
				reasons = append(reasons, reason)
			}
			break
		}
		if !hintEvaluated {
			reasons = append(reasons, fmt.Sprintf("hint node %d not present on this host", specNode))
		}
	}

	if res, handled := trySticky(); handled {
		if res.Bound {
			return res, nil
		}
		if params.strict() {
			return numaAllocResult{}, fmt.Errorf("%w: sticky node: %s", ErrNumaCapacityInsufficient, res.Reason)
		}
		// Sticky node full: keep looking at the other nodes (换 node), the
		// caller decides when the sticky mapping should be rebuilt.
	}

	type candidate struct {
		node     numa.NumaNode
		memRatio float64
		cpuRatio float64
	}
	var candidates []candidate
	for _, node := range nodes {
		if intent == numaIntentNode && int32(node.NodeId) == specNode {
			// Already evaluated in the hint pass; its reason is recorded.
			continue
		}
		if ok, reason := passes(node); ok {
			mem := committed[int32(node.NodeId)]
			var cm, cc int64
			if mem != nil {
				cm, cc = mem.Mem, mem.Cpu
			}
			cores := float64(len(node.Cores))
			if cores == 0 {
				cores = 1
			}
			candidates = append(candidates, candidate{
				node:     node,
				memRatio: float64(cm+memQ) / float64(srcTotalOrOne(src, node.NodeId)),
				cpuRatio: float64(cc+cpuQ) / (cores * 1000),
			})
		} else {
			reasons = append(reasons, reason)
		}
	}

	if len(candidates) == 0 {
		reason := "no numa node fits"
		if len(reasons) > 0 {
			reason += ": " + strings.Join(reasons, "; ")
		}
		if params.strict() {
			return numaAllocResult{}, fmt.Errorf("%w: %s", ErrNumaCapacityInsufficient, reason)
		}
		return numaAllocResult{Degraded: true, Reason: reason}, nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].memRatio != candidates[j].memRatio {
			return candidates[i].memRatio < candidates[j].memRatio
		}
		if candidates[i].cpuRatio != candidates[j].cpuRatio {
			return candidates[i].cpuRatio < candidates[j].cpuRatio
		}
		return candidates[i].node.NodeId < candidates[j].node.NodeId
	})
	return numaAllocResult{Bound: true, Node: int32(candidates[0].node.NodeId)}, nil
}

func srcTotalOrOne(src numaCapacitySource, node int) uint64 {
	total, err := src.TotalBytes(node)
	if err != nil || total == 0 {
		return 1
	}
	return total
}

// readNumaLedger folds the sandbox bucket into ledger entries. Entries that
// no longer parse are skipped — Destroy removes them anyway.
func (l *CgPlugin) readNumaLedger() []numaLedgerEntry {
	if l.db == nil {
		return nil
	}
	all, err := l.db.ReadAll(bucket)
	if err != nil {
		return nil
	}
	entries := make([]numaLedgerEntry, 0, len(all))
	for _, raw := range all {
		if entry, err := parseNumaLedgerValue(raw); err == nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

// allocateNumaNode runs the ledger + both gates for one declared sandbox.
// besteffort degrades (Bound=false, reason set); strict returns
// ErrNumaCapacityInsufficient.
func (l *CgPlugin) allocateNumaNode(intent numaIntent, specNode int32, stickyKey string, memQ, cpuQ int64) (numaAllocResult, error) {
	cfg := dynamConf.GetCommon()
	if cfg == nil {
		// Uninitialized dynamic config: treat as "feature unavailable" rather
		// than guessing defaults.
		return numaAllocResult{Degraded: true, Reason: "numa config unavailable"}, nil
	}
	params := buildNumaAllocParams(cfg.NumaBindEnabled, cfg.NumaAllocPolicy,
		cfg.NumaReserveMemMB, cfg.NumaMemRatio, cfg.NumaTemplateAffinity)

	entries := l.readNumaLedger()

	// Creates run concurrently across workflow steps; guard the counters.
	l.numaStickyMissMu.Lock()
	defer l.numaStickyMissMu.Unlock()
	if l.numaStickyMiss == nil {
		l.numaStickyMiss = map[string]int{}
	}

	stickyOverride := false
	if params.TemplateAffinity && stickyKey != "" {
		if l.numaStickyMiss[stickyKey] >= numaStickyRemapThreshold {
			// Sticky node keeps failing — drop it for this round so the
			// scored selection can re-home the lineage (spec: remap).
			stickyOverride = true
			delete(l.numaStickyMiss, stickyKey)
			CubeLog.Warnf("numa sticky remap for key %s after %d consecutive misses", stickyKey, numaStickyRemapThreshold)
		}
	}

	res, err := planNumaAllocation(numa.GetAllNumaNodes(), entries, sysfsCapacitySource{},
		memQ, cpuQ, intent, specNode, stickyKey, stickyOverride, params)

	if params.TemplateAffinity && stickyKey != "" && !stickyOverride {
		sticky := stickyNodeOf(entries, stickyKey)
		boundSticky := res.Bound && sticky != nil && res.Node == *sticky
		if sticky == nil {
			// First VM of the lineage — nothing to count.
		} else if boundSticky {
			delete(l.numaStickyMiss, stickyKey)
		} else {
			l.numaStickyMiss[stickyKey]++
		}
	}
	return res, err
}

var (
	numaReconcileMu   sync.Mutex
	numaReconcileLast time.Time
)

// reconcileNumaLedger compares pool-v2 cgroups on disk against the ledger and
// logs the differences (leaked cgroups without a ledger entry, or stale
// ledger entries whose cgroup vanished). Throttled to once a minute; invoked
// from CollectMetric.
func (l *CgPlugin) reconcileNumaLedger(ctx context.Context) {
	numaReconcileMu.Lock()
	if time.Since(numaReconcileLast) < time.Minute {
		numaReconcileMu.Unlock()
		return
	}
	numaReconcileLast = time.Now()
	numaReconcileMu.Unlock()

	existing, err := l.pool.GetAllGroupsExists()
	if err != nil {
		CubeLog.Warnf("numa ledger reconcile: list groups failed: %v", err)
		return
	}
	onDisk := map[uint32]struct{}{}
	for _, id := range existing {
		if IsPoolV2ID(id) {
			onDisk[id] = struct{}{}
		}
	}
	inLedger := map[uint32]struct{}{}
	for _, entry := range l.readNumaLedger() {
		if IsPoolV2ID(entry.CgID) {
			inLedger[entry.CgID] = struct{}{}
		}
	}
	for id := range onDisk {
		if _, ok := inLedger[id]; !ok {
			CubeLog.Warnf("numa ledger reconcile: cgroup %d exists on disk but not in ledger", id)
		}
	}
	for id := range inLedger {
		if _, ok := onDisk[id]; !ok {
			CubeLog.Warnf("numa ledger reconcile: ledger entry %d has no cgroup on disk", id)
		}
	}
}
