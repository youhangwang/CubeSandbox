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
// shim's hotset prewarm covers the recorded hot pages; this hook starts a
// bulk sequential read of the whole memory volume (and the rootfs volume)
// at the moment the create request arrives, before any VM assembly, so the
// reads overlap with guest boot from the first byte — and a request that
// arrived ahead of the create finds the cache already hot.
//
// The read is plain pread(2) (ReadAt): fadvise-style hints are no-ops on
// newer kernels, while pread delivery is complete on every kernel. Holes in
// the sparse memory volume cost no disk I/O. Best effort by design: any
// failure only means the guest faults from disk exactly as before.

package cubebox

import (
	"context"
	"io"
	"os"
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
	if path, err := storage.ResolveObjectPathFor(ctx, backend, entry.MemoryVol, entry.MemoryKind); err == nil {
		prewarmVolumeOnce(ctx, "memory", path)
	} else {
		log.G(ctx).Debugf("create keepwarm: resolve memory volume for %s: %v", snapshotID, err)
	}
	// The rootfs volume is read by guest boot as well. Kernel and agent
	// images are shared node assets and are assumed resident.
	if path, err := storage.ResolveObjectPathFor(ctx, backend, entry.RootfsVol, entry.RootfsKind); err == nil {
		prewarmVolumeOnce(ctx, "rootfs", path)
	} else {
		log.G(ctx).Debugf("create keepwarm: resolve rootfs volume for %s: %v", snapshotID, err)
	}
}

// prewarmVolumeOnce bulk-reads path unless a read of it is already running
// or one completed within prewarmSkipInterval.
func prewarmVolumeOnce(ctx context.Context, label, path string) {
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
	dur, n, err := prewarmFileSync(path)
	keepwarmDone.Store(path, time.Now())
	keepwarmReads++
	if err != nil {
		log.G(ctx).Warnf("create keepwarm: %s volume %s: %d bytes in %s: %v", label, path, n, dur, err)
		return
	}
	log.G(ctx).Infof("create keepwarm: %s volume %s warmed %d bytes in %s", label, path, n, dur)
}

// prewarmFileSync reads the whole file sequentially into the page cache
// with pread(2) (ReadAt). Sparse holes cost no disk I/O. Returns the wall
// time and the bytes pulled through the read path.
func prewarmFileSync(path string) (time.Duration, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	var total int64
	start := time.Now()
	for {
		n, err := f.ReadAt(buf, total)
		total += int64(n)
		if err != nil {
			if err == io.EOF || n == 0 && err == io.ErrUnexpectedEOF {
				break
			}
			return time.Since(start), total, err
		}
		if n == 0 || n < len(buf) {
			break
		}
	}
	return time.Since(start), total, nil
}
