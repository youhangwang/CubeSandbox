// Copyright © 2026 Tencent Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Create-time page-cache prewarm ("keep warm on create").
//
// A create that fast-restores from a template snapshot pays for the
// template's memory volume inside the create window: the guest's serial
// fault path reads at a fraction of the disk's sequential bandwidth, so
// the restore stalls on I/O that bulk parallel reads could absorb. The
// shim's hotset prewarm covers the recorded hot pages; this hook starts
// reading at the moment the create request arrives, before any VM
// assembly, so the reads overlap with guest boot from the first byte —
// and a request that arrived ahead of the create finds the cache already
// hot.
//
// By default only the recorded profile is read: the exact-order
// first-touch prefix (in recorded order, coalesced into runs) plus the
// extent ranges — the profile is the contract. CUBE_TEMPLATE_KEEPWARM_SCOPE=full
// switches to sweeping the whole volume instead. The read is plain
// pread(2) (ReadAt): fadvise-style hints are no-ops on newer kernels,
// while pread delivery is complete on every kernel. Holes in the sparse
// memory volume cost no disk I/O. Best effort by design: any failure only
// means the guest faults from disk exactly as before.

package cubebox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

// EnvCreateKeepWarm gates the create-time page-cache prewarm. It is on by
// default; set it to 0/false/off/no to fall back to the plain fault path
// (the shim's hotset prewarm is independent and unaffected).
const EnvCreateKeepWarm = "CUBE_TEMPLATE_KEEPWARM_ON_CREATE"

// EnvCreateKeepWarmScope selects what the prewarm reads:
//
//	hotset (default) — the recorded profile only: the exact-order
//	    first-touch prefix (coalesced into runs, in recorded order) plus
//	    the extent ranges. The profile is the contract; nothing outside it
//	    is read.
//	full — the whole memory volume sequentially (covers pages the
//	    profile missed, at the cost of reading regions the guest may
//	    never touch).
const EnvCreateKeepWarmScope = "CUBE_TEMPLATE_KEEPWARM_SCOPE"

const keepWarmScopeHotset = "hotset"
const keepWarmScopeFull = "full"

func createKeepWarmScope() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvCreateKeepWarmScope)), keepWarmScopeFull) {
		return keepWarmScopeFull
	}
	return keepWarmScopeHotset
}

// prewarmSkipInterval suppresses a repeated bulk read of the same volume
// within this window of a completed read: the earlier read is assumed to
// still be resident.
const prewarmSkipInterval = time.Minute

// keepwarmInflight deduplicates concurrent reads of the same volume;
// keepwarmDone remembers the last completed read per volume path. Entries
// linger for the lifetime of the process — two words per template volume.
var (
	keepwarmInflight sync.Map
	keepwarmDone     sync.Map
)

// keepwarmReads counts completed bulk reads; tests assert dedup on it.
var keepwarmReads int64

func createKeepWarmEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvCreateKeepWarm))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// kickCreateKeepWarm starts the background bulk read for a create that
// restores from a template/snapshot. Fire-and-forget: it never blocks the
// create and never fails it. It is a no-op for builds and plain creates.
func (s *service) kickCreateKeepWarm(ctx context.Context, createInfo *workflow.CreateContext) {
	if !createKeepWarmEnabled() || createInfo == nil || !createInfo.IsRetoreSnapshot() {
		return
	}
	snapshotID, ok := createInfo.GetSnapshotTemplateID()
	if !ok || strings.TrimSpace(snapshotID) == "" {
		return
	}
	backend := ""
	if createInfo.ReqInfo != nil {
		backend = strings.TrimSpace(createInfo.ReqInfo.GetAnnotations()[constants.MasterAnnotationStorageBackend])
	}
	go recov.GoWithRecover(func() {
		s.keepWarmSnapshot(ctx, backend, snapshotID)
	})
}

// keepWarmSnapshot resolves the snapshot's volumes through the local
// catalog and bulk-reads them into the page cache, memory first.
func (s *service) keepWarmSnapshot(ctx context.Context, backend, snapshotID string) {
	entry, err := storage.GetLocalSnapshotFor(ctx, backend, snapshotID)
	if err != nil {
		log.G(ctx).Debugf("create keepwarm: catalog lookup for %s: %v", snapshotID, err)
		return
	}
	memPath, err := storage.ResolveObjectPathFor(ctx, backend, entry.MemoryVol, entry.MemoryKind)
	if err != nil {
		log.G(ctx).Debugf("create keepwarm: resolve memory volume for %s: %v", snapshotID, err)
		memPath = ""
	}
	if memPath != "" {
		if createKeepWarmScope() == keepWarmScopeFull {
			prewarmVolumeOnce(ctx, "memory", memPath, nil)
		} else if list, perr := keepWarmReadList(filepath.Join(snapshotStateDir(entry.MetaDir), HotPagesFileName)); perr != nil {
			log.G(ctx).Debugf("create keepwarm: profile for %s: %v (memory volume skipped)", snapshotID, perr)
		} else if len(list) == 0 {
			log.G(ctx).Debugf("create keepwarm: empty profile for %s (memory volume skipped)", snapshotID)
		} else {
			prewarmVolumeOnce(ctx, "memory", memPath, list)
		}
	}
	// The rootfs volume is read by guest boot as well. Kernel and agent
	// images are shared node assets and are assumed resident.
	if path, err := storage.ResolveObjectPathFor(ctx, backend, entry.RootfsVol, entry.RootfsKind); err == nil {
		prewarmVolumeOnce(ctx, "rootfs", path, nil)
	} else {
		log.G(ctx).Debugf("create keepwarm: resolve rootfs volume for %s: %v", snapshotID, err)
	}
}

// prewarmVolumeOnce bulk-reads path unless a read of it is already running
// or one completed within prewarmSkipInterval. When ranges is nil the whole
// file is read; otherwise only the listed [offset, length] chunks, in list
// order.
func prewarmVolumeOnce(ctx context.Context, label, path string, ranges [][2]uint64) {
	if _, busy := keepwarmInflight.LoadOrStore(path, struct{}{}); busy {
		log.G(ctx).Debugf("create keepwarm: %s volume %s already warming", label, path)
		return
	}
	defer keepwarmInflight.Delete(path)
	if t, ok := keepwarmDone.Load(path); ok {
		if time.Since(t.(time.Time)) < prewarmSkipInterval {
			log.G(ctx).Debugf("create keepwarm: %s volume %s warmed recently", label, path)
			return
		}
	}
	dur, n, err := prewarmFile(path, ranges)
	keepwarmDone.Store(path, time.Now())
	keepwarmReads++
	if err != nil {
		log.G(ctx).Warnf("create keepwarm: %s volume %s: %d bytes in %s: %v", label, path, n, dur, err)
		return
	}
	log.G(ctx).Infof("create keepwarm: %s volume %s warmed %d bytes in %s", label, path, n, dur)
}

// prewarmFile reads path into the page cache: the whole file when ranges is
// nil, else only the listed chunks in order. Returns the wall time and the
// bytes pulled through the read path.
func prewarmFile(path string, ranges [][2]uint64) (time.Duration, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	var total int64
	start := time.Now()
	readAt := func(off, length int64) error {
		pos := off
		for left := length; left > 0; {
			want := int64(len(buf))
			if left < want {
				want = left
			}
			n, err := f.ReadAt(buf[:want], pos)
			total += int64(n)
			pos += int64(n)
			left -= int64(n)
			if err != nil {
				if err == io.EOF && pos-off == length {
					return nil
				}
				return err
			}
			if n == 0 {
				return io.EOF
			}
		}
		return nil
	}
	if ranges == nil {
		st, err := f.Stat()
		if err != nil {
			return time.Since(start), total, err
		}
		if err := readAt(0, st.Size()); err != nil && err != io.EOF {
			return time.Since(start), total, err
		}
		return time.Since(start), total, nil
	}
	for _, r := range ranges {
		if err := readAt(int64(r[0]), int64(r[1])); err != nil && err != io.EOF {
			return time.Since(start), total, err
		}
	}
	return time.Since(start), total, nil
}

// keepWarmReadList loads the recorded profile and builds the read plan: the
// exact-order first-touch prefix first, consecutive pages coalesced into
// runs, then the extent ranges. Byte totals are bounded by the profile
// caps the writer already enforced.
func keepWarmReadList(profilePath string) ([][2]uint64, error) {
	payload, err := os.ReadFile(profilePath)
	if err != nil {
		return nil, err
	}
	var prof hotPagesFile
	if err := json.Unmarshal(payload, &prof); err != nil {
		return nil, fmt.Errorf("parse %s: %w", profilePath, err)
	}
	page := uint64(os.Getpagesize())
	coalesce := uint64(1 << 20)
	var list [][2]uint64
	var runOff, runLen uint64
	flush := func() {
		if runLen > 0 {
			list = append(list, [2]uint64{runOff, runLen})
			runLen = 0
		}
	}
	for _, off := range prof.PrefixPages {
		if runLen > 0 && off == runOff+runLen && runLen+page <= coalesce {
			runLen += page
			continue
		}
		flush()
		runOff, runLen = off, page
	}
	flush()
	list = append(list, prof.Extents...)
	return list, nil
}
