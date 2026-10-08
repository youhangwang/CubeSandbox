// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package numa

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NodeMemStats is the per-node memory picture the NUMA ledger's measured gate
// runs on. Reclaimable page cache (FilePages) is deliberately excluded: the
// kernel drops clean file pages on demand, so counting them as pressure would
// over-brake admission on nodes holding large shared template images.
type NodeMemStats struct {
	NodeID int

	// All values in bytes.
	MemTotal       uint64
	AnonPages      uint64
	Shmem          uint64
	SUnreclaim     uint64
	Dirty          uint64
	PageTables     uint64
	KernelStack    uint64
	Unevictable    uint64
	FilePages      uint64
	NonReclaimable uint64
}

const nodeStatsCacheTTL = time.Second

type nodeStatsCacheEntry struct {
	stats  *NodeMemStats
	err    error
	loaded time.Time
}

var (
	statsMu       sync.Mutex
	statsCache    = map[int]nodeStatsCacheEntry{}
	statsNowFunc  = time.Now
	statsReadFunc = readNodeMeminfo
)

// GetNodeMemStats returns the memory stats for one NUMA node, cached for
// nodeStatsCacheTTL. Callers must treat an error as "gate not passable".
func GetNodeMemStats(nodeID int) (*NodeMemStats, error) {
	statsMu.Lock()
	defer statsMu.Unlock()

	if entry, ok := statsCache[nodeID]; ok && statsNowFunc().Sub(entry.loaded) < nodeStatsCacheTTL {
		return entry.stats, entry.err
	}

	stats, err := statsReadFunc(nodeID)
	statsCache[nodeID] = nodeStatsCacheEntry{stats: stats, err: err, loaded: statsNowFunc()}
	return stats, err
}

func readNodeMeminfo(nodeID int) (*NodeMemStats, error) {
	path := fmt.Sprintf("/sys/devices/system/node/node%d/meminfo", nodeID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	stats, err := parseNodeMeminfo(nodeID, data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return stats, nil
}

// parseNodeMeminfo parses /sys/devices/system/node/nodeN/meminfo, whose lines
// look like "Node 0 AnonPages:      123 kB". Unknown fields are skipped so the
// parser tolerates kernel differences.
func parseNodeMeminfo(nodeID int, data []byte) (*NodeMemStats, error) {
	stats := &NodeMemStats{NodeID: nodeID}
	found := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		// "Node", "<id>", "<Name>:", "<value>", "kB"
		if len(fields) < 4 || fields[0] != "Node" {
			continue
		}
		if id, err := strconv.Atoi(fields[1]); err != nil || id != nodeID {
			continue
		}
		name := strings.TrimSuffix(fields[2], ":")
		valueKb, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			continue
		}
		bytes := valueKb * 1024
		switch name {
		case "MemTotal":
			stats.MemTotal = bytes
		case "AnonPages":
			stats.AnonPages = bytes
		case "Shmem":
			stats.Shmem = bytes
		case "SUnreclaim":
			stats.SUnreclaim = bytes
		case "Dirty":
			stats.Dirty = bytes
		case "PageTables":
			stats.PageTables = bytes
		case "KernelStack":
			stats.KernelStack = bytes
		case "Unevictable":
			stats.Unevictable = bytes
		case "FilePages":
			stats.FilePages = bytes
		default:
			continue
		}
		found++
	}
	if stats.MemTotal == 0 {
		return nil, fmt.Errorf("MemTotal missing or zero (%d known fields)", found)
	}
	stats.NonReclaimable = stats.AnonPages + stats.Shmem + stats.SUnreclaim +
		stats.Dirty + stats.PageTables + stats.KernelStack
	return stats, nil
}
