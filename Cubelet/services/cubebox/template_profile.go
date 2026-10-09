// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

// Template memory hotset profiling ("hot-pages.json").
//
// When CUBE_TEMPLATE_PROFILE_ENABLE is set, AppSnapshot runs a profiling
// phase after the snapshot package and catalog are in place and before the
// template is finalized for distribution: two verification sandboxes are
// started from the brand-new template through the normal create path, and
// the memory pages the restored guest touches between resume and ready are
// collected. The intersection of the two rounds drops one-off page faults;
// the result is merged into extents and written next to memory-ranges
// inside the package metadata, where the VMM consumes it at fast-restore
// time to prewarm the page cache (hypervisor/vmm/src/hotset.rs).
//
// Two capture sources are supported. When the recording marker
// (`hotset-record`) is present next to the profile, the verification VMMs
// back guest RAM with a memfd+userfaultfd region (hotset_record.rs) and
// write a first-touch trace file: the exact page sequence the guest
// touched, which becomes the version-2 manifest's ordered prefix. Without
// the marker (or when traces fail), the phase falls back to the original
// /proc/<pid>/pagemap present-bit collection and a version-1 manifest.
//
// The phase is strictly optional and fail-open: any error — including the
// overall budget running out — is returned to the caller, which logs a
// warning and publishes the template without a profile.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

const (
	// HotPagesFileName is the profile file the VMM looks up inside the
	// snapshot state dir (<meta>/snapshot/), next to memory-ranges.
	HotPagesFileName = "hot-pages.json"
	// HotPagesVersionV1 is the extents-only profile schema (pagemap capture).
	HotPagesVersionV1 = 1
	// HotPagesVersionV2 adds the exact-order first-touch prefix recorded
	// through the memfd+userfaultfd recording path.
	HotPagesVersionV2 = 2

	// HotsetRecordMarker asks the verification VMMs to record first-touch
	// order; it exists only for the lifetime of the profiling phase.
	HotsetRecordMarker = "hotset-record"
	// HotsetRecordTracePrefix is the trace file name prefix the recording
	// VMM writes (pid-suffixed) into the snapshot state dir.
	HotsetRecordTracePrefix = "hotset-record.trace."
	// hotsetTraceHeaderSize is the fixed trace header: magic + version +
	// page size.
	hotsetTraceHeaderSize = 12

	// EnvTemplateProfileEnable turns the profiling phase on (default off).
	EnvTemplateProfileEnable = "CUBE_TEMPLATE_PROFILE_ENABLE"
	// EnvTemplateProfileTimeoutSec bounds the whole phase in seconds.
	EnvTemplateProfileTimeoutSec = "CUBE_TEMPLATE_PROFILE_TIMEOUT_SEC"

	// verifyRounds is the number of verification restores; the intersection
	// of their touch sets drops one-off page faults.
	verifyRounds = 2
	// hotPagesMaxTotalDen caps the profile extent total at 1/N of the memory
	// volume — the consumer rejects anything larger.
	hotPagesMaxTotalDen = 2
	// hotPagesMaxExtents caps how many extents a profile may carry. Past
	// this point the profile costs more than it saves, and the phase fails
	// open: the template publishes without a profile.
	hotPagesMaxExtents = 4096
	// hotPagesMaxPrefix caps the exact-order prefix: the strictly-ordered
	// head of the first-touch sequence the consumer replays.
	hotPagesMaxPrefix = 4096
	// extentCoalescePages merges hot pages separated by small gaps so the
	// extent list stays short.
	extentCoalescePages = 16
	// minRoundBudget is the minimum remaining time for another round.
	minRoundBudget = 5 * time.Second
	// defaultProfileBudgetSec is the default total phase budget.
	defaultProfileBudgetSec = 60
)

func templateProfileEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvTemplateProfileEnable))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func templateProfileBudget() time.Duration {
	raw := strings.TrimSpace(os.Getenv(EnvTemplateProfileTimeoutSec))
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(defaultProfileBudgetSec) * time.Second
}

// runTemplateProfilePhase executes the two-round verification restores and
// writes the merged profile to profilePath. Returned errors are advisory:
// the caller logs them and publishes the template without a profile.
func (s *service) runTemplateProfilePhase(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	createReq *cubebox.RunCubeSandboxRequest,
	backend, templateID, profilePath string,
	memFileSize uint64,
) error {
	deadline := time.Now().Add(templateProfileBudget())
	traceDir := filepath.Dir(profilePath)

	// Ask the verification VMMs to record first-touch order: the marker
	// exists only for this phase and is removed on every path. When it is
	// present the rounds yield exact-order traces (v2 manifest); otherwise
	// the phase falls back to pagemap sets (v1).
	markerPath := filepath.Join(traceDir, HotsetRecordMarker)
	if err := os.WriteFile(markerPath, nil, 0o644); err != nil {
		stepLog.Warnf("template profile: recording marker write failed, pagemap-only capture: %v", err)
		markerPath = ""
	} else {
		defer func() {
			if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
				stepLog.Warnf("template profile: recording marker remove failed: %v", err)
			}
		}()
	}

	rounds := make([]verificationRound, 0, verifyRounds)
	for round := 0; round < verifyRounds; round++ {
		remaining := time.Until(deadline)
		if remaining <= minRoundBudget {
			return fmt.Errorf("budget exhausted after %d/%d rounds", round, verifyRounds)
		}
		r, err := s.profileVerificationRound(ctx, stepLog, createReq, backend, templateID, round, remaining, traceDir, markerPath != "")
		if err != nil {
			return fmt.Errorf("verification round %d: %w", round, err)
		}
		rounds = append(rounds, *r)
	}

	// Exact-order capture: both rounds recorded a first-touch trace, so the
	// manifest carries an ordered prefix (round-0 order filtered to the
	// pages both rounds touched) plus the merged ranges of the stable set.
	if len(rounds[0].tracePages) > 0 && len(rounds[1].tracePages) > 0 {
		return writeOrderedProfile(
			profilePath, templateID, memFileSize,
			rounds[0].tracePages, rounds[1].tracePages,
			stepLog,
		)
	}

	// Version-1 fallback: pagemap present-bit sets.
	pages := intersectPageSets([]map[uint64]struct{}{rounds[0].pages, rounds[1].pages})
	if len(pages) == 0 {
		return errors.New("empty page intersection across rounds")
	}
	extents, totalBytes := mergeHotExtents(pages, extentCoalescePages, memFileSize)
	if err := checkHotProfileCaps(extents, totalBytes, memFileSize); err != nil {
		return err
	}
	if err := writeHotPagesFile(profilePath, templateID, memFileSize, HotPagesVersionV1, extents, nil); err != nil {
		return fmt.Errorf("write profile: %w", err)
	}
	stepLog.Infof("template profile: wrote %s (v1): %d extents, %d pages, %d bytes (%.1f%% of memory)",
		profilePath, len(extents), len(pages), totalBytes,
		float64(totalBytes)/float64(memFileSize)*100)
	return nil
}

// verificationRound is one verification restore's capture result. Exactly
// one of the two sources is populated: tracePages (exact order, from the
// recording VMM) or pages (pagemap present set).
type verificationRound struct {
	pages      map[uint64]struct{}
	tracePages []uint64
}

// profileVerificationRound starts one verification sandbox from the new
// template through the normal create path, waits for it to become ready
// (Create returns), collects the touched page set (exact-order trace when
// recording is on, pagemap otherwise) and destroys the sandbox best-effort
// on every path: Create is not atomic, and a failed create can still leave
// the sandbox behind.
func (s *service) profileVerificationRound(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	createReq *cubebox.RunCubeSandboxRequest,
	backend, templateID string,
	round int,
	budget time.Duration,
	traceDir string, recording bool,
) (*verificationRound, error) {
	verifyReq, err := buildProfileVerifyRequest(createReq, templateID, round)
	if err != nil {
		return nil, err
	}

	// Derived from the incoming ctx, so an aborted build also aborts the
	// round; budget is the round's share of the phase deadline.
	verifyCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	verifyCtx = inheritIncomingMetadata(verifyCtx, ctx)
	verifyCtx = CubeLog.WithRequestTrace(verifyCtx, &CubeLog.RequestTrace{
		Action:       "TemplateProfile",
		RequestID:    verifyReq.GetRequestID(),
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       s.engine.ID(),
		CalleeAction: "TemplateProfileRound",
	})
	verifyCtx = namespaces.WithNamespace(verifyCtx, namespaces.Default)

	roundLog := stepLog.WithFields(CubeLog.Fields{
		"round":     round,
		"sandboxID": verifyReq.GetAnnotations()[constants.MasterAnnotationDesiredSandboxID],
	})

	createRsp, createErr := s.Create(verifyCtx, verifyReq)
	if createErr == nil && createRsp == nil {
		// A nil response would read as ret code 0 (= ErrorCode_OK) below.
		createErr = errors.New("create verification sandbox returned no response")
	}
	sandboxID := createRsp.GetSandboxID()
	if sandboxID == "" {
		sandboxID = verifyReq.GetAnnotations()[constants.MasterAnnotationDesiredSandboxID]
	}
	destroyRoundSandbox := func() {
		// Deliberately on context.Background(): a cancelled build must
		// still reap the sandbox.
		destroyCtx, destroyCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer destroyCancel()
		destroyCtx = inheritIncomingMetadata(destroyCtx, ctx)
		destroyCtx = namespaces.WithNamespace(destroyCtx, namespaces.Default)
		destroyRsp, destroyErr := s.Destroy(destroyCtx, &cubebox.DestroyCubeSandboxRequest{
			RequestID: verifyReq.GetRequestID(),
			SandboxID: sandboxID,
		})
		switch {
		case destroyErr != nil:
			roundLog.Warnf("verification sandbox %s destroy failed: %v", sandboxID, destroyErr)
		case !ret.IsSuccessCode(destroyRsp.GetRet().GetRetCode()):
			roundLog.Warnf("verification sandbox %s destroy failed: %s", sandboxID, destroyRsp.GetRet().GetRetMsg())
		}
	}
	if createErr != nil || !ret.IsSuccessCode(createRsp.GetRet().GetRetCode()) {
		destroyRoundSandbox()
		if createErr != nil {
			return nil, fmt.Errorf("create verification sandbox: %w", createErr)
		}
		return nil, fmt.Errorf("create verification sandbox: %s", createRsp.GetRet().GetRetMsg())
	}
	roundLog.Infof("verification sandbox created: %s", sandboxID)

	// Recording mode: the VMM's guest RAM is a memfd (the volume path is
	// deliberately not mapped, so pagemap collection cannot match), and the
	// trace is only complete once the recording handler exits with the VM.
	// Destroy first, then read the trace. Pagemap mode: collect from the
	// live process first — destroying loses the mapping.
	if recording {
		destroyRoundSandbox()
		tracePages, traceErr := collectVerificationTrace(traceDir, roundLog)
		if traceErr != nil {
			return nil, traceErr
		}
		return &verificationRound{tracePages: tracePages}, nil
	}

	pages, collectErr := s.collectVerificationPageset(verifyCtx, roundLog, backend, templateID, sandboxID)
	destroyRoundSandbox()
	if collectErr != nil {
		return nil, collectErr
	}
	return &verificationRound{pages: pages}, nil
}

// buildProfileVerifyRequest derives a template-restore create request from
// the template BUILD request: same workload spec, minus the build marker so
// the workflow takes the create-from-template (restore) path.
func buildProfileVerifyRequest(createReq *cubebox.RunCubeSandboxRequest, templateID string, round int) (*cubebox.RunCubeSandboxRequest, error) {
	req, ok := proto.Clone(createReq).(*cubebox.RunCubeSandboxRequest)
	if !ok || req == nil {
		return nil, errors.New("clone create request failed")
	}
	req.RequestID = fmt.Sprintf("%s-prof-%d", createReq.GetRequestID(), round)
	annos := req.GetAnnotations()
	if annos == nil {
		annos = map[string]string{}
	}
	// Restore is keyed on the template id annotation without the
	// appsnapshot build marker (workflow.CreateContext.IsRetoreSnapshot).
	delete(annos, constants.MasterAnnotationsAppSnapshotCreate)
	annos[constants.MasterAnnotationAppSnapshotTemplateID] = templateID
	// Deterministic sandbox id (createid honors the desired id): distinct
	// from user sandboxes and traceable across the two rounds.
	annos[constants.MasterAnnotationDesiredSandboxID] = fmt.Sprintf("%s-prof-%d", templateID, round)
	req.Annotations = annos
	return req, nil
}

// collectVerificationPageset resolves the template memory volume, locates
// the VMM process of sandboxID holding it mapped, and reads the present
// pages out of its pagemap.
func (s *service) collectVerificationPageset(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	backend, templateID, sandboxID string,
) (map[uint64]struct{}, error) {
	entry, err := storage.GetLocalSnapshotFor(ctx, backend, templateID)
	if err != nil || entry == nil {
		return nil, fmt.Errorf("resolve template catalog: %w", err)
	}
	if strings.TrimSpace(entry.MemoryVol) == "" {
		return nil, errors.New("template catalog has no memory volume")
	}
	memoryDevPath, err := storage.ResolveObjectPathFor(ctx, backend, entry.MemoryVol, entry.MemoryKind)
	if err != nil {
		return nil, fmt.Errorf("resolve memory volume %s: %w", entry.MemoryVol, err)
	}

	cb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("load sandbox record: %w", err)
	}
	// Bundle pids (shim.pid / vmm.pid) are included so the lookup also works
	// if the VMM runs out of process; the embedded VMM records the same pid
	// as the endpoint.
	pid := findVMMProcessPid(s.cubeboxMgr.collectSandboxRuntimePIDs(ctx, cb), memoryDevPath)
	if pid <= 0 {
		return nil, fmt.Errorf("no process maps %s for sandbox %s", memoryDevPath, sandboxID)
	}

	pages, err := collectPresentPageset(ctx, pid, memoryDevPath)
	if err != nil {
		return nil, fmt.Errorf("collect pagemap of pid %d: %w", pid, err)
	}
	stepLog.Infof("collected %d present pages from pid %d (%s)", len(pages), pid, memoryDevPath)
	return pages, nil
}

// mapsDevT is the identity of the memory volume in the terms
// /proc/<pid>/maps reports: the major:minor device number (of the device
// itself for block/char devices, of the hosting filesystem for regular
// files) plus the inode for regular files.
type mapsDevT struct {
	major, minor, ino uint64
	needIno           bool
}

// resolveMapsDev stats the memory volume and derives its maps identity.
// ok is false when the path cannot be stated; basename matching is then
// refused.
func resolveMapsDev(memoryDevPath string) (ident mapsDevT, ok bool) {
	var st unix.Stat_t
	if err := unix.Stat(memoryDevPath, &st); err != nil {
		return mapsDevT{}, false
	}
	ident = mapsDevT{
		major: uint64(unix.Major(st.Dev)),
		minor: uint64(unix.Minor(st.Dev)),
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFBLK, unix.S_IFCHR:
		ident.major, ident.minor = uint64(unix.Major(st.Rdev)), uint64(unix.Minor(st.Rdev))
	default:
		ident.needIno = true
		ident.ino = st.Ino
	}
	return ident, true
}

// mapsLineDev parses the "major:minor" hex device field of a maps line.
func mapsLineDev(field string) (major, minor uint64, ok bool) {
	colon := strings.IndexByte(field, ':')
	if colon <= 0 {
		return 0, 0, false
	}
	if major, err := strconv.ParseUint(field[:colon], 16, 64); err == nil {
		if minor, err := strconv.ParseUint(field[colon+1:], 16, 64); err == nil {
			return major, minor, true
		}
	}
	return 0, 0, false
}

// findVMMProcessPid picks the candidate pid whose /proc/<pid>/maps maps the
// template memory volume — i.e. the process hosting the restored VM. Exact
// path matches win; basename matches are a fallback, accepted only when the
// line's device number (and inode for regular files) agrees with the
// volume's stat identity.
func findVMMProcessPid(candidates []int, memoryDevPath string) int {
	base := filepath.Base(memoryDevPath)
	ident, identKnown := resolveMapsDev(memoryDevPath)
	fallback := 0
	for _, pid := range candidates {
		if pid <= 1 || pid == os.Getpid() {
			continue
		}
		content, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
		if err != nil {
			continue
		}
		exact, loose := mapsContainPath(string(content), memoryDevPath, base, ident, identKnown)
		if exact {
			return pid
		}
		if loose && fallback == 0 {
			fallback = pid
		}
	}
	return fallback
}

// mapsLineMatches reports whether one parsed maps line ("start-end perms
// offset dev inode path") refers to the memory volume. Exact path equality
// always matches; a basename-only match is accepted only when the volume's
// stat identity is known and the line's device number (plus inode for
// regular files) agrees with it. Shared by pid discovery and pagemap
// collection so both accept the same lines.
func mapsLineMatches(fields []string, memoryDevPath, baseName string, ident mapsDevT, identKnown bool) bool {
	path := fields[5]
	if path == memoryDevPath {
		return true
	}
	if filepath.Base(path) != baseName || !identKnown {
		return false
	}
	major, minor, ok := mapsLineDev(fields[3])
	if !ok || major != ident.major || minor != ident.minor {
		return false
	}
	if ident.needIno && fields[4] != strconv.FormatUint(ident.ino, 10) {
		return false
	}
	return true
}

// mapsContainPath reports whether any maps line references memoryDevPath,
// separately for exact path equality and verified basename-only matches (the
// latter tolerates dm/loop path renames between resolve time and mmap time,
// but requires device-number agreement with the resolved volume).
func mapsContainPath(mapsData, memoryDevPath, baseName string, ident mapsDevT, identKnown bool) (exact, loose bool) {
	for _, line := range strings.Split(mapsData, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 6 {
			continue
		}
		if fields[5] == memoryDevPath {
			return true, true
		}
		if mapsLineMatches(fields, memoryDevPath, baseName, ident, identKnown) {
			loose = true
		}
	}
	return false, loose
}

// procMapLine is one entry of /proc/<pid>/maps backed by the memory volume.
type procMapLine struct {
	startAddr  uint64
	endAddr    uint64
	fileOffset uint64
}

// parseMapsLine parses "start-end perms offset dev inode path" fields and
// keeps only readable mappings of memoryDevPath, matched like pid discovery
// (mapsContainPath): basename matches tolerate dm/loop path renames and are
// accepted only with device-number (and inode) agreement.
func parseMapsLine(line, memoryDevPath string, ident mapsDevT, identKnown bool) (procMapLine, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 6 {
		return procMapLine{}, false
	}
	if !mapsLineMatches(fields, memoryDevPath, filepath.Base(memoryDevPath), ident, identKnown) {
		return procMapLine{}, false
	}
	if !strings.HasPrefix(fields[1], "r") {
		return procMapLine{}, false
	}
	dash := strings.IndexByte(fields[0], '-')
	if dash <= 0 {
		return procMapLine{}, false
	}
	start, err := strconv.ParseUint(fields[0][:dash], 16, 64)
	if err != nil {
		return procMapLine{}, false
	}
	end, err := strconv.ParseUint(fields[0][dash+1:], 16, 64)
	if err != nil {
		return procMapLine{}, false
	}
	offset, err := strconv.ParseUint(fields[2], 16, 64)
	if err != nil {
		return procMapLine{}, false
	}
	return procMapLine{startAddr: start, endAddr: end, fileOffset: offset}, true
}

// collectPresentPageset reads /proc/<pid>/maps + pagemap and returns the set
// of present page numbers (in memory-file page units) among the process's
// mappings of memoryDevPath.
func collectPresentPageset(ctx context.Context, pid int, memoryDevPath string) (map[uint64]struct{}, error) {
	mapsData, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return nil, fmt.Errorf("read maps: %w", err)
	}
	pagemap, err := os.Open(fmt.Sprintf("/proc/%d/pagemap", pid)) // NOCC:Path Traversal()
	if err != nil {
		return nil, fmt.Errorf("open pagemap: %w", err)
	}
	defer pagemap.Close()
	return parsePresentPageset(ctx, string(mapsData), pagemap, memoryDevPath, uint64(os.Getpagesize()))
}

// parsePresentPageset walks the maps entries matching memoryDevPath and reads
// the pagemap present bits for their vaddr ranges. Split from
// collectPresentPageset for fixture-driven unit tests.
func parsePresentPageset(ctx context.Context, mapsData string, pagemap io.ReadSeeker, memoryDevPath string, pageSize uint64) (map[uint64]struct{}, error) {
	ident, identKnown := resolveMapsDev(memoryDevPath)
	pages := map[uint64]struct{}{}
	for _, line := range strings.Split(mapsData, "\n") {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("pagemap collection cancelled: %w", err)
		}
		m, ok := parseMapsLine(line, memoryDevPath, ident, identKnown)
		if !ok {
			continue
		}
		startPage := m.startAddr / pageSize
		endPage := m.endAddr / pageSize
		if endPage <= startPage {
			continue
		}
		count := endPage - startPage
		raw := make([]byte, count*8)
		if _, err := pagemap.Seek(int64(startPage*8), 0); err != nil {
			return nil, fmt.Errorf("seek pagemap: %w", err)
		}
		if _, err := io.ReadFull(pagemap, raw); err != nil {
			return nil, fmt.Errorf("read pagemap: %w", err)
		}
		for i := 0; i < len(raw); i += 8 {
			// Bit 63: page present. Swapped-out pages are not resident and
			// must not enter the hotset.
			if binary.LittleEndian.Uint64(raw[i:])&(1<<63) == 0 {
				continue
			}
			// VMM mappings are page-aligned: mapping page i covers memory
			// file offset fileOffset + (i/8)*pageSize.
			pages[m.fileOffset/pageSize+uint64(i/8)] = struct{}{}
		}
	}
	return pages, nil
}

// hotsetTraceHeader is the fixed header of a first-touch trace file
// (hypervisor/vmm/src/hotset_record.rs): magic "HSTR", little-endian u32
// version, little-endian u32 page size — then u64 little-endian volume
// offsets in exact first-touch order.
type hotsetTraceHeader struct {
	Magic    [4]byte
	Version  uint32
	PageSize uint32
}

// collectVerificationTrace reads and removes every trace file the recording
// VMM left in the snapshot state dir for this round, returning the ordered
// page numbers (in memory-file page units).
func collectVerificationTrace(traceDir string, stepLog *log.CubeWrapperLogEntry) ([]uint64, error) {
	matches, err := filepath.Glob(filepath.Join(traceDir, HotsetRecordTracePrefix+"*"))
	if err != nil {
		return nil, fmt.Errorf("glob traces: %w", err)
	}
	if len(matches) == 0 {
		return nil, errors.New("no recording trace found")
	}
	var pages []uint64
	for _, path := range matches {
		data, err := os.ReadFile(path) // NOCC:Path Traversal()
		if err != nil {
			return nil, fmt.Errorf("read trace %s: %w", path, err)
		}
		parsed, err := parseHotsetTrace(data)
		if err != nil {
			return nil, fmt.Errorf("parse trace %s: %w", path, err)
		}
		pages = append(pages, parsed...)
		if err := os.Remove(path); err != nil {
			stepLog.Warnf("template profile: trace %s remove failed: %v", path, err)
		}
	}
	stepLog.Infof("collected %d first-touch pages from %d recording trace(s)", len(pages), len(matches))
	return pages, nil
}

// parseHotsetTrace validates the trace header and decodes the ordered page
// offsets. Split from collectVerificationTrace for unit tests.
func parseHotsetTrace(data []byte) ([]uint64, error) {
	if len(data) < hotsetTraceHeaderSize {
		return nil, errors.New("trace shorter than header")
	}
	var header hotsetTraceHeader
	copy(header.Magic[:], data[:4])
	if string(header.Magic[:]) != "HSTR" {
		return nil, errors.New("bad trace magic")
	}
	if header.Version = binary.LittleEndian.Uint32(data[4:8]); header.Version != 1 {
		return nil, fmt.Errorf("unsupported trace version %d", header.Version)
	}
	if header.PageSize = binary.LittleEndian.Uint32(data[8:12]); header.PageSize == 0 {
		return nil, errors.New("zero trace page size")
	}
	body := data[hotsetTraceHeaderSize:]
	if len(body)%8 != 0 {
		return nil, errors.New("trace body is not u64-aligned")
	}
	pages := make([]uint64, 0, len(body)/8)
	for i := 0; i < len(body); i += 8 {
		pages = append(pages, binary.LittleEndian.Uint64(body[i:])/uint64(header.PageSize))
	}
	return pages, nil
}

// writeOrderedProfile builds the version-2 manifest from two first-touch
// traces: the prefix replays round 0's exact touch order filtered to pages
// both rounds touched (stable, ordered head), and the merged ranges carry
// the remaining stable pages. Ranges exclude prefix pages so the consumer's
// two phases never double-read.
func writeOrderedProfile(
	path, templateID string, memFileSize uint64,
	trace0, trace1 []uint64,
	stepLog *log.CubeWrapperLogEntry,
) error {
	if len(trace0) == 0 || len(trace1) == 0 {
		return errors.New("ordered profile requires non-empty traces")
	}
	stable0 := make(map[uint64]struct{}, len(trace0))
	for _, page := range trace0 {
		stable0[page] = struct{}{}
	}
	stable1 := make(map[uint64]struct{}, len(trace1))
	for _, page := range trace1 {
		stable1[page] = struct{}{}
	}

	// Prefix: round 0's order, kept only where round 1 agrees, capped.
	var prefix []uint64
	prefixSet := make(map[uint64]struct{}, hotPagesMaxPrefix)
	for _, page := range trace0 {
		if len(prefix) >= hotPagesMaxPrefix {
			break
		}
		if _, ok := stable1[page]; !ok {
			continue
		}
		if _, ok := prefixSet[page]; ok {
			continue
		}
		prefixSet[page] = struct{}{}
		prefix = append(prefix, page)
	}
	// Ranges: the rest of the stable set, prefix pages excluded.
	stable := intersectPageSets([]map[uint64]struct{}{stable0, stable1})
	for page := range prefixSet {
		delete(stable, page)
	}
	if len(stable) == 0 && len(prefix) == 0 {
		return errors.New("empty ordered profile intersection")
	}
	extents, totalBytes := mergeHotExtents(stable, extentCoalescePages, memFileSize)
	if len(prefix) > hotPagesMaxPrefix || len(extents) > hotPagesMaxExtents {
		return fmt.Errorf("ordered profile over caps: %d prefix pages, %d extents",
			len(prefix), len(extents))
	}
	prefixBytes := uint64(len(prefix)) * uint64(os.Getpagesize())
	if (totalBytes+prefixBytes)*hotPagesMaxTotalDen > memFileSize {
		return fmt.Errorf("profile total %d bytes exceeds 1/%d of memory size %d",
			totalBytes+prefixBytes, hotPagesMaxTotalDen, memFileSize)
	}
	if err := writeHotPagesFile(path, templateID, memFileSize, HotPagesVersionV2, extents, prefix); err != nil {
		return fmt.Errorf("write profile: %w", err)
	}
	stepLog.Infof("template profile: wrote %s (v2): %d prefix pages, %d extents, %d bytes (%.1f%% of memory)",
		path, len(prefix), len(extents), totalBytes+prefixBytes,
		float64(totalBytes+prefixBytes)/float64(memFileSize)*100)
	return nil
}

// intersectPageSets keeps only pages present in every round.
func intersectPageSets(sets []map[uint64]struct{}) map[uint64]struct{} {
	if len(sets) == 0 {
		return nil
	}
	result := sets[0]
	for _, other := range sets[1:] {
		for page := range result {
			if _, ok := other[page]; !ok {
				delete(result, page)
			}
		}
	}
	return result
}

// mergeHotExtents converts a present-page set into [offset, len] byte
// extents, merging runs separated by at most coalesce pages. Extents are
// clamped to memFileSize; the caller rejects over-cap profiles.
func mergeHotExtents(pages map[uint64]struct{}, coalesce uint64, memFileSize uint64) ([][2]uint64, uint64) {
	if len(pages) == 0 {
		return nil, 0
	}
	nums := make([]uint64, 0, len(pages))
	for page := range pages {
		nums = append(nums, page)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })

	pageSize := uint64(os.Getpagesize())
	extents := make([][2]uint64, 0, len(pages))
	start, end := nums[0], nums[0]+1 // [start, end) in pages
	flush := func() {
		offset := start * pageSize
		length := (end - start) * pageSize
		if offset >= memFileSize {
			return
		}
		if offset+length > memFileSize {
			length = memFileSize - offset
		}
		if length == 0 {
			return
		}
		extents = append(extents, [2]uint64{offset, length})
	}
	for _, page := range nums[1:] {
		if page-end <= coalesce {
			if page >= end {
				end = page + 1
			}
			continue
		}
		flush()
		start, end = page, page+1
	}
	flush()

	total := uint64(0)
	for _, e := range extents {
		total += e[1]
	}
	return extents, total
}

// checkHotProfileCaps rejects profiles the consumer should not carry: empty
// ones, totals over 1/hotPagesMaxTotalDen of the memory volume, or extent
// counts over hotPagesMaxExtents.
func checkHotProfileCaps(extents [][2]uint64, totalBytes, memFileSize uint64) error {
	if totalBytes == 0 {
		return errors.New("merged profile is empty")
	}
	if totalBytes*hotPagesMaxTotalDen > memFileSize {
		return fmt.Errorf("profile total %d bytes exceeds 1/%d of memory size %d",
			totalBytes, hotPagesMaxTotalDen, memFileSize)
	}
	if len(extents) > hotPagesMaxExtents {
		return fmt.Errorf("profile has %d extents, over the %d cap",
			len(extents), hotPagesMaxExtents)
	}
	return nil
}

// hotPagesFile is the profile schema consumed by hypervisor/vmm hotset.rs.
// PrefixPages is set only on version-2 profiles (exact-order first touches).
type hotPagesFile struct {
	Version     int         `json:"version"`
	TemplateID  string      `json:"template_id"`
	MemFileSize uint64      `json:"mem_file_size"`
	ProfiledAt  string      `json:"profiled_at"`
	Extents     [][2]uint64 `json:"extents"`
	PrefixPages []uint64    `json:"prefix_pages,omitempty"`
}

// writeHotPagesFile atomically writes the profile (temp file + rename) so a
// consumer never observes a partial file.
func writeHotPagesFile(path, templateID string, memFileSize uint64, version int, extents [][2]uint64, prefixPages []uint64) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}
	payload, err := json.Marshal(hotPagesFile{
		Version:     version,
		TemplateID:  templateID,
		MemFileSize: memFileSize,
		ProfiledAt:  time.Now().UTC().Format(time.RFC3339),
		Extents:     extents,
		PrefixPages: prefixPages,
	})
	if err != nil {
		return fmt.Errorf("marshal profile: %w", err)
	}
	tmp, err := os.CreateTemp(dir, HotPagesFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp profile: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write profile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close profile: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename profile: %w", err)
	}
	return nil
}
