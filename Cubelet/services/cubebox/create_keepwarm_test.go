// Copyright © 2026 Tencent Inc.
//
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

func TestCreateKeepWarmEnabled(t *testing.T) {
	orig := os.Getenv(EnvCreateKeepWarm)
	defer os.Setenv(EnvCreateKeepWarm, orig)
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"", true},
		{"1", true},
		{"true", true},
		{"0", false},
		{"false", false},
		{" OFF ", false},
	} {
		os.Setenv(EnvCreateKeepWarm, tc.raw)
		if got := createKeepWarmEnabled(); got != tc.want {
			t.Errorf("env %q: enabled=%v want %v", tc.raw, got, tc.want)
		}
	}
}

func TestPrewarmFileReadsWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vol")
	// 2.5 MiB: forces several ReadAt rounds, tail shorter than one buffer.
	data := make([]byte, (5<<20)/2)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	dur, n, err := prewarmFile(path, nil)
	if err != nil {
		t.Fatalf("prewarmFile: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("read %d bytes, want %d", n, len(data))
	}
	if dur <= 0 {
		t.Errorf("duration %v must be positive", dur)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file vanished: %v", err)
	}
}

func TestPrewarmFileReadsOnlyRequestedRanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vol")
	data := make([]byte, 3<<20)
	for i := range data {
		data[i] = byte(i / 256)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Read only the first 128 KiB and a 256 KiB window in the middle.
	ranges := [][2]uint64{{0, 128 << 10}, {1 << 20, 256 << 10}}
	_, n, err := prewarmFile(path, ranges)
	if err != nil {
		t.Fatalf("prewarmFile: %v", err)
	}
	if want := int64(128<<10 + 256<<10); n != want {
		t.Errorf("read %d bytes, want %d", n, want)
	}
}

func TestPrewarmFileSparseHoles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sparse")
	// Truncate to 3 MiB: everything after the first byte is a hole.
	if err := os.WriteFile(path, []byte{1}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 3<<20); err != nil {
		t.Fatal(err)
	}
	_, n, err := prewarmFile(path, nil)
	if err != nil {
		t.Fatalf("prewarmFile: %v", err)
	}
	if n != 3<<20 {
		t.Errorf("read %d bytes, want %d", n, 3<<20)
	}
}

func TestPrewarmFileMissingFile(t *testing.T) {
	if _, _, err := prewarmFile(filepath.Join(t.TempDir(), "absent"), nil); err == nil {
		t.Error("missing file must error")
	}
}

func TestKeepWarmReadListOrderedPrefixThenExtents(t *testing.T) {
	dir := t.TempDir()
	page := uint64(os.Getpagesize())
	meg := uint64(1 << 20)
	// prefix: 300 consecutive pages starting at 1 MiB (coalesces into one
	// run), then a page at 8 MiB; extents add [16 MiB, 4 KiB).
	prof := hotPagesFile{
		Version:     2,
		MemFileSize: 32 << 20,
		PrefixPages: []uint64{},
		Extents:     [][2]uint64{{16 << 20, 4096}},
	}
	for i := 0; i < 300; i++ {
		prof.PrefixPages = append(prof.PrefixPages, meg+page*uint64(i))
	}
	prof.PrefixPages = append(prof.PrefixPages, 8<<20)
	payload, err := json.Marshal(prof)
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(dir, HotPagesFileName)
	if err := os.WriteFile(profilePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := keepWarmReadList(profilePath)
	if err != nil {
		t.Fatalf("keepWarmReadList: %v", err)
	}
	// Expect: the 300-page run coalesces up to the 1 MiB cap (256 pages),
	// the remaining 44 pages form the next run, then the lone 8 MiB page,
	// then the extent.
	want := [][2]uint64{{meg, 256 * page}, {meg + meg, 44 * page}, {8 << 20, page}, {16 << 20, 4096}}
	if len(list) != len(want) {
		t.Fatalf("got %d chunks %v, want %d", len(list), list, len(want))
	}
	for i, c := range want {
		if list[i] != c {
			t.Errorf("chunk %d = %v, want %v", i, list[i], c)
		}
	}
}

func TestKeepWarmReadListRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HotPagesFileName)
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := keepWarmReadList(path); err == nil {
		t.Error("garbage profile must error")
	}
	if _, err := keepWarmReadList(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("missing profile must error")
	}
}

func TestPrewarmVolumeOnceDeduplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vol")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	before := atomic.LoadInt64(&keepwarmReads)
	prewarmVolumeOnce(ctx, "test", path, nil)
	if got := atomic.LoadInt64(&keepwarmReads); got != before+1 {
		t.Fatalf("first call must read: reads=%d want %d", got, before+1)
	}
	// A second call within prewarmSkipInterval must be suppressed.
	prewarmVolumeOnce(ctx, "test", path, nil)
	if got := atomic.LoadInt64(&keepwarmReads); got != before+1 {
		t.Errorf("second call within skip interval must not read: reads=%d", got)
	}
	// An expired completion allows the read again.
	keepwarmDone.Store(path, time.Now().Add(-2*prewarmSkipInterval))
	prewarmVolumeOnce(ctx, "test", path, nil)
	if got := atomic.LoadInt64(&keepwarmReads); got != before+2 {
		t.Errorf("call after skip interval must read: reads=%d want %d", got, before+2)
	}
}

func TestKickCreateKeepWarmNoopsWithoutSnapshot(t *testing.T) {
	// Plain create (no template annotation): must not panic and must not
	// read anything.
	orig := os.Getenv(EnvCreateKeepWarm)
	os.Setenv(EnvCreateKeepWarm, "1")
	defer os.Setenv(EnvCreateKeepWarm, orig)
	s := &service{}
	before := atomic.LoadInt64(&keepwarmReads)
	s.kickCreateKeepWarm(context.Background(), &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations:  map[string]string{},
		},
	})
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt64(&keepwarmReads); got != before {
		t.Errorf("plain create must not prewarm: reads=%d", got)
	}
}

func TestKickCreateKeepWarmUnknownSnapshotFailsOpen(t *testing.T) {
	// Restore create with a snapshot id the catalog does not know: the
	// lookup fails, the hook logs and returns — create unaffected.
	orig := os.Getenv(EnvCreateKeepWarm)
	os.Setenv(EnvCreateKeepWarm, "1")
	defer os.Setenv(EnvCreateKeepWarm, orig)
	s := &service{}
	before := atomic.LoadInt64(&keepwarmReads)
	s.kickCreateKeepWarm(context.Background(), &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-does-not-exist",
			},
		},
	})
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt64(&keepwarmReads); got != before {
		t.Errorf("unknown snapshot must not read anything: reads=%d", got)
	}
}
