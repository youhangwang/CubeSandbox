// Copyright © 2026 Tencent Inc.
//
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
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

func TestPrewarmFileSyncReadsWholeFile(t *testing.T) {
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
	dur, n, err := prewarmFileSync(path)
	if err != nil {
		t.Fatalf("prewarmFileSync: %v", err)
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

func TestPrewarmFileSyncSparseHoles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sparse")
	// Truncate to 3 MiB: everything after the first byte is a hole.
	if err := os.WriteFile(path, []byte{1}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 3<<20); err != nil {
		t.Fatal(err)
	}
	_, n, err := prewarmFileSync(path)
	if err != nil {
		t.Fatalf("prewarmFileSync: %v", err)
	}
	if n != 3<<20 {
		t.Errorf("read %d bytes, want %d", n, 3<<20)
	}
}

func TestPrewarmFileSyncMissingFile(t *testing.T) {
	if _, _, err := prewarmFileSync(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("missing file must error")
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
	prewarmVolumeOnce(ctx, "test", path)
	if got := atomic.LoadInt64(&keepwarmReads); got != before+1 {
		t.Fatalf("first call must read: reads=%d want %d", got, before+1)
	}
	// A second call within prewarmSkipInterval must be suppressed.
	prewarmVolumeOnce(ctx, "test", path)
	if got := atomic.LoadInt64(&keepwarmReads); got != before+1 {
		t.Errorf("second call within skip interval must not read: reads=%d", got)
	}
	// An expired completion allows the read again.
	keepwarmDone.Store(path, time.Now().Add(-2*prewarmSkipInterval))
	prewarmVolumeOnce(ctx, "test", path)
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
