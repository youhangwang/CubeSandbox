// Copyright © 2026 Tencent Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// First-touch ORDER recording for template hotset profiling.
//
// The hotset consumer (hotset.rs) restores the guest's page-cache warmth,
// but the plain profile carries no ORDER: pages are merged into extents and
// walked in extent order, while the guest faults them in its own order. On
// seek-bound storage that mismatch is the difference between a faster boot
// and a slower one.
//
// This module records the exact first-touch order. It is deliberately an
// INDEPENDENT path from the plain restore: when a `hotset-record` marker
// file exists next to the snapshot state (cubelet drops it only around the
// profiling verification rounds), every guest RAM region is backed by a
// memfd registered with userfaultfd (missing mode) instead of the volume
// file. The handler thread resolves each fault by preading the page from
// the memory volume and UFFDIO_COPYing it in, appending the volume offset
// to a trace file — the exact sequence of first touches, with none of the
// readahead slop a pagemap snapshot carries.
//
// Everything here is best-effort and process-local: the shim hosts one VM,
// the recorder activates at most once per process, any failure degrades to
// the plain volume-backed restore, and the handler thread simply dies with
// the process when the recording VM goes away.

use std::fs::{self, File};
use std::io::Write;
use std::os::fd::{AsRawFd, FromRawFd};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, OnceLock};

use log::{info, warn};
use vm_memory::GuestMemoryRegion;

use crate::GuestRegionMmap;

/// Marker file the profiler drops into the snapshot state dir to ask this
/// process to record first-touch order.
const MARKER_FILE: &str = "hotset-record";
/// Trace files, pid-suffixed so the two profiling rounds never collide.
const TRACE_PREFIX: &str = "hotset-record.trace.";
/// Debug milestones, pid-suffixed: the recorder runs in the shim whose
/// stdio is discarded, so its own decisions land in a file instead.
const DEBUG_PREFIX: &str = "hotset-record.debug.";
const TRACE_MAGIC: &[u8; 4] = b"HSTR";
const TRACE_VERSION: u32 = 1;

const PAGE: u64 = 4096;
/// Events batched per read(2) on the uffd fd.
const UFFD_MSG_BATCH: usize = 64;
/// sizeof(struct uffd_msg): event byte + flags + padding + fault payload.
const UFFD_MSG_SIZE: usize = 24;
/// Trace records buffered before flushing to disk.
const TRACE_FLUSH_RECORDS: usize = 8192;

// userfaultfd syscall numbers (asm-generic unistd: x86_64 323, aarch64 282).
#[cfg(target_arch = "aarch64")]
const SYS_USERFAULTFD: libc::c_long = 282;
#[cfg(not(target_arch = "aarch64"))]
const SYS_USERFAULTFD: libc::c_long = 323;

// userfaultfd ioctl numbers: stable kernel ABI, spelled out here so no
// extra crate is needed. _UFFDIO_REGISTER=0x00, _UFFDIO_COPY=0x03,
// _UFFDIO_API=0x3f; sizes: register=32, copy=40, api=24;
// _IOWR(0xAA, nr, size).
const UFFD_API: u64 = 0xAA;
/// MISSING mode on shmem/memfd mappings must be negotiated at the API
/// handshake, or UFFDIO_REGISTER rejects the range with EINVAL.
const UFFD_FEATURE_MISSING_SHMEM: u64 = 1 << 1;
const UFFDIO_REGISTER_MODE_MISSING: u64 = 1 << 0;
pub(crate) const UFFDIO_API_IOCTL: libc::c_ulong = 0xc018_aa3f;
pub(crate) const UFFDIO_REGISTER_IOCTL: libc::c_ulong = 0xc020_aa00;
pub(crate) const UFFDIO_COPY_IOCTL: libc::c_ulong = 0xc028_aa03;

#[repr(C)]
struct UffdioApi {
    api: u64,
    features: u64,
    ioctls: u64,
}

#[repr(C)]
struct UffdioRange {
    start: u64,
    len: u64,
}

#[repr(C)]
struct UffdioRegister {
    range: UffdioRange,
    mode: u64,
    ioctls: u64,
}

#[repr(C)]
struct UffdioCopy {
    dst: u64,
    src: u64,
    len: u64,
    mode: u64,
    copy: u64,
}

const _: () = {
    assert!(std::mem::size_of::<UffdioApi>() == 24);
    assert!(std::mem::size_of::<UffdioRegister>() == 32);
    assert!(std::mem::size_of::<UffdioCopy>() == 40);
};

/// One memfd-backed guest RAM region under recording.
struct RecordedRegion {
    /// Host virtual address of the shim's mapping of this region.
    host_addr: u64,
    /// Byte length of the mapping.
    len: u64,
    /// Offset of this region's backing pages inside the memory volume.
    volume_offset: u64,
}

/// State shared between the registering thread and the fault handler. The
/// fault path only ever takes `regions` (held briefly by registrars), so a
/// registering thread can never stall an in-flight guest fault.
struct Recorder {
    uffd_fd: i32,
    volume: File,
    /// Backs every guest RAM region: sized and laid out exactly like the
    /// memory volume, so each region's `snap_offset` maps 1:1.
    memfd: File,
    /// Taken by the handler thread at spawn; the only trace writer.
    trace: Mutex<Option<File>>,
    /// Debug milestones for the on-host troubleshooting file.
    debug: Mutex<Option<File>>,
    regions: Mutex<Vec<RecordedRegion>>,
}

/// dbg appends one line to the debug milestone file (best-effort).
fn dbg(recorder: &Recorder, line: &str) {
    if let Some(mut f) = recorder
        .debug
        .lock()
        .unwrap_or_else(|p| p.into_inner())
        .as_mut()
    {
        let ts = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis())
            .unwrap_or(0);
        let _ = writeln!(f, "[{ts}] {line}");
        let _ = f.flush();
    }
}

static RECORDER: OnceLock<Arc<Recorder>> = OnceLock::new();

/// True when this process is recording first touches. Cheap, lock-free.
pub fn active() -> bool {
    RECORDER.get().is_some()
}

impl Recorder {
    /// True when `addr` falls inside any registered region.
    fn contains(&self, addr: u64) -> bool {
        self.regions
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .iter()
            .any(|r| addr >= r.host_addr && addr < r.host_addr + r.len)
    }
}

/// Activate recording when the snapshot state dir carries the marker file.
/// `volume_path` is the memory volume the handler preads from; the trace is
/// written beside the profile in `snapshot_dir`. Any failure leaves the
/// global state inactive and the caller on the plain restore path.
pub fn activate_if_requested(snapshot_dir: &Path, volume_path: &Path) -> Result<(), String> {
    if RECORDER.get().is_some() {
        return Ok(());
    }
    if !snapshot_dir.join(MARKER_FILE).exists() {
        return Ok(());
    }
    let trace_path: PathBuf = snapshot_dir.join(format!("{TRACE_PREFIX}{}", std::process::id()));
    let debug_path: PathBuf = snapshot_dir.join(format!("{DEBUG_PREFIX}{}", std::process::id()));
    let debug = fs::OpenOptions::new()
        .create(true)
        .write(true)
        .truncate(true)
        .open(&debug_path)
        .map_err(|e| format!("open debug {}: {e}", debug_path.display()))?;
    let volume = File::open(volume_path).map_err(|e| format!("open volume: {e}"))?;
    let mut trace = fs::OpenOptions::new()
        .create(true)
        .write(true)
        .truncate(true)
        .open(&trace_path)
        .map_err(|e| format!("open trace {}: {e}", trace_path.display()))?;
    trace
        .write_all(TRACE_MAGIC)
        .and_then(|_| trace.write_all(&TRACE_VERSION.to_le_bytes()))
        .and_then(|_| trace.write_all(&(PAGE as u32).to_le_bytes()))
        .map_err(|e| format!("write trace header: {e}"))?;

    let fd = unsafe { libc::syscall(SYS_USERFAULTFD, 0) };
    if fd < 0 {
        return Err(format!("userfaultfd syscall failed: {}", io_err()));
    }
    let uffd_fd = fd as i32;
    let mut api = UffdioApi {
        api: UFFD_API,
        // The guest RAM region is a memfd (shmem): MISSING mode registration
        // on shmem is only accepted when the feature is negotiated here.
        features: UFFD_FEATURE_MISSING_SHMEM,
        ioctls: 0,
    };
    if unsafe { libc::ioctl(uffd_fd, UFFDIO_API_IOCTL, &mut api) } != 0 {
        let err = io_err();
        unsafe { libc::close(uffd_fd) };
        return Err(format!("UFFDIO_API failed: {err}"));
    }

    // One memfd mirroring the memory volume: every guest RAM region maps it
    // at its own `snap_offset`, exactly like the plain path maps the volume,
    // so region content and trace offsets stay 1:1 with the volume.
    let volume_size = volume.metadata().map_err(|e| format!("stat volume: {e}"))?.len();
    let memfd_fd = unsafe {
        libc::syscall(
            libc::SYS_memfd_create,
            b"hotset-record\0".as_ptr(),
            libc::MFD_CLOEXEC,
        )
    };
    if memfd_fd < 0 {
        let err = io_err();
        unsafe { libc::close(uffd_fd) };
        return Err(format!("memfd_create failed: {err}"));
    }
    let memfd = unsafe { File::from_raw_fd(memfd_fd as i32) };
    if let Err(e) = memfd.set_len(volume_size) {
        let err = format!("memfd truncate: {e}");
        unsafe { libc::close(uffd_fd) };
        return Err(err);
    }

    let recorder = Arc::new(Recorder {
        uffd_fd,
        volume,
        memfd,
        debug: Mutex::new(Some(debug)),
        trace: Mutex::new(Some(trace)),
        regions: Mutex::new(Vec::new()),
    });
    if RECORDER.set(recorder).is_err() {
        unsafe { libc::close(uffd_fd) };
        return Ok(());
    }
    info!(
        "restore memory hotset recording activated: trace={}",
        trace_path.display()
    );
    Ok(())
}

/// Region backing swap: when recording is active the region is backed by
/// the recording memfd (laid out like the memory volume) instead of the
/// volume file itself. Returns `saved` unchanged when recording is off, so
/// the caller keeps its plain path.
pub fn take_memfd_region_backing(saved: Option<File>) -> Option<File> {
    let Some(recorder) = RECORDER.get() else {
        return saved;
    };
    match recorder.memfd.try_clone() {
        // Region mappings own their dup; the recorder keeps the original.
        Ok(dup) => Some(dup),
        Err(e) => {
            warn!("hotset record: memfd dup failed: {e}");
            saved
        }
    }
}

/// Register a just-built guest RAM region with the uffd fd and start the
/// handler thread on the first registration. `volume_offset` is where this
/// region's backing pages live inside the memory volume.
pub fn register_region(region: &GuestRegionMmap, volume_offset: u64) {
    let Some(recorder) = RECORDER.get() else {
        return;
    };
    let host_addr = region.as_ptr() as u64;
    let len = region.len() as u64;
    if host_addr == 0 || len == 0 {
        return;
    }
    // 4K fault granularity: shmem THP would turn first touches into 2M
    // events and blur the trace. Fresh mapping, no faults can race this.
    unsafe {
        libc::madvise(
            host_addr as *mut libc::c_void,
            len as usize,
            libc::MADV_NOHUGEPAGE,
        );
    }
    let mut reg = UffdioRegister {
        range: UffdioRange {
            start: host_addr,
            len,
        },
        mode: UFFDIO_REGISTER_MODE_MISSING,
        ioctls: 0,
    };
    if unsafe { libc::ioctl(recorder.uffd_fd, UFFDIO_REGISTER_IOCTL, &mut reg) } != 0 {
        dbg(
            recorder,
            &format!("REGISTER FAILED at {host_addr:#x} len {len}: {}", io_err()),
        );
        warn!(
            "hotset record: UFFDIO_REGISTER failed at {host_addr:#x}: {}",
            io_err()
        );
        return;
    }
    dbg(
        recorder,
        &format!(
            "REGISTER ok at {host_addr:#x} len {len} vol_off {volume_offset}"
        ),
    );
    recorder
        .regions
        .lock()
        .unwrap_or_else(|p| p.into_inner())
        .push(RecordedRegion {
            host_addr,
            len,
            volume_offset,
        });
    // One handler thread serves every registered range for the lifetime of
    // the process; the recording VM is single-shot, so no cancel plumbing.
    static HANDLER: OnceLock<()> = OnceLock::new();
    HANDLER.get_or_init(|| {
        let trace = recorder
            .trace
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .take();
        let Some(trace) = trace else {
            warn!("hotset record: handler not started, trace already taken");
            return;
        };
        // Private dups of the uffd and volume fds: VM assembly churns open
        // fd numbers, and a closed-and-reused number would silently point
        // the handler at some other stream (observed: ttrpc frames). A dup
        // keeps the open file description alive regardless.
        let uffd_fd = unsafe { libc::dup(recorder.uffd_fd) };
        let volume_fd = unsafe { libc::dup(recorder.volume.as_raw_fd()) };
        if uffd_fd < 0 || volume_fd < 0 {
            warn!("hotset record: fd dup failed");
            unsafe {
                if uffd_fd >= 0 {
                    libc::close(uffd_fd);
                }
                if volume_fd >= 0 {
                    libc::close(volume_fd);
                }
            }
            return;
        }
        let handler = Handler {
            recorder: Arc::clone(recorder),
            trace,
            buf: Vec::with_capacity(TRACE_FLUSH_RECORDS * 8),
            msgs: vec![0u8; UFFD_MSG_BATCH * 32],
            layout: None,
            faults: 0,
        };
        let (u, v) = (uffd_fd, volume_fd);
        dbg(
            &recorder,
            &format!(
                "handler fds: uffd={u} -> {}  volume={v} -> {}",
                std::fs::read_link(format!("/proc/self/fd/{u}"))
                    .map(|p| p.display().to_string())
                    .unwrap_or_else(|e| format!("gone({e})")),
                std::fs::read_link(format!("/proc/self/fd/{v}"))
                    .map(|p| p.display().to_string())
                    .unwrap_or_else(|e| format!("gone({e})")),
            ),
        );
        if let Err(e) = std::thread::Builder::new()
            .name("hotset-record".to_string())
            .spawn(move || handler.run())
        {
            warn!("hotset record: handler spawn failed: {e}");
        }
    });
}

/// The fault service loop. Owns the trace file: it is the only writer after
/// activation, so no locking is needed on the fault path beyond the shared
/// region table (held briefly by registrars).
struct Handler {
    recorder: Arc<Recorder>,
    trace: File,
    buf: Vec<u8>,
    msgs: Vec<u8>,
    /// Message layout (stride, address offset) — differs by kernel version
    /// (24/8 pre pagefault-flags, 32/16 after) and is auto-detected from the
    /// first event: the fault address must land inside a registered region,
    /// while the wrong candidate reads the (zero) flags field.
    layout: Option<(usize, usize)>,
    faults: u64,
}

impl Handler {
    fn run(mut self) {
        info!("hotset record: handler started");
        dbg(&self.recorder, "handler started");
        loop {
            let n = unsafe {
                libc::read(
                    self.recorder.uffd_fd,
                    self.msgs.as_mut_ptr() as *mut libc::c_void,
                    self.msgs.len(),
                )
            };
            if n < 0 && errno_is_retry() {
                continue;
            }
            if n <= 0 {
                info!("hotset record: handler exiting ({n})");
                dbg(&self.recorder, &format!("handler exiting ({n})"));
                break;
            }
            let bytes = n as usize;
            if self.layout.is_none() {
                // Detect once from the batch head. The upstream layout is
                // (stride 24, address @8); this vendor kernel delivers
                // (stride 32, address @16) and its first byte is NOT the
                // upstream event value — so the candidates are matched by
                // region membership of the address, not by the event byte.
                let detected = self.detect_layout(bytes);
                let head: String = self.msgs[..40.min(bytes)]
                    .iter()
                    .map(|b| format!("{b:02x}"))
                    .collect();
                dbg(
                    &self.recorder,
                    &format!("first batch {bytes}B head={head} layout={detected:?}"),
                );
                self.layout = Some(detected);
            }
            let (stride, addr_at) = self.layout.unwrap();
            // Collect first: handle_fault needs &mut self for the trace
            // buffer, and the msgs slice borrows self immutably. The event
            // byte is vendor-specific (0x12 here vs upstream 1) — records
            // are validated by address containment inside handle_fault, so
            // unrelated records simply miss and are dropped.
            let mut faults = Vec::with_capacity(bytes / stride + 1);
            let mut pos = 0usize;
            while pos + stride <= bytes {
                let chunk = &self.msgs[pos..pos + stride];
                faults.push(u64::from_le_bytes(
                    chunk[addr_at..addr_at + 8].try_into().unwrap(),
                ));
                pos += stride;
            }
            for addr in faults {
                self.handle_fault(addr);
            }
            self.faults += 1;
            if self.faults % 65536 == 0 {
                let (batches, buf_len) = (self.faults, self.buf.len());
                dbg(&self.recorder, &format!("progress: {batches} batches, buf {buf_len}B"));
            }
            // Flush per batch: the trace consumer (cubelet) reads after the
            // VM is destroyed, and this keeps the tail loss at one batch.
            self.flush();
        }
    }

    fn detect_layout(&self, bytes: usize) -> (usize, usize) {
        // This vendor kernel's first byte is not the upstream event value,
        // so candidates are matched purely by address containment.
        for candidate in [(32usize, 16usize), (24usize, 8usize)] {
            let (stride, addr_at) = candidate;
            if bytes >= stride {
                let addr = u64::from_le_bytes(
                    self.msgs[addr_at..addr_at + 8].try_into().unwrap(),
                );
                if addr != 0 && self.recorder.contains(addr) {
                    return candidate;
                }
            }
        }
        (32, 16)
    }

    /// Resolve one fault: pread the page from the memory volume, UFFDIO_COPY
    /// it into the mapping, and append the volume offset to the trace. The
    /// faulting vcpu is parked until the copy lands, so this must never skip
    /// a fault.
    fn handle_fault(&mut self, addr: u64) {
        let page_addr = addr & !(PAGE - 1);
        let regions = self
            .recorder
            .regions
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let Some(region) = regions
            .iter()
            .find(|r| page_addr >= r.host_addr && page_addr < r.host_addr + r.len)
        else {
            if self.faults < 3 {
                dbg(&self.recorder, &format!("FAULT MISS addr={page_addr:#x}"));
            }
            return;
        };
        let vol_off = region.volume_offset + (page_addr - region.host_addr);
        drop(regions);

        let mut page = [0u8; PAGE as usize];
        if !pread_full(self.recorder.volume.as_raw_fd(), &mut page, vol_off) {
            dbg(
                &self.recorder,
                &format!("PREAD FAIL vol_off={vol_off}: {}", io_err()),
            );
            return;
        }
        let mut copy = UffdioCopy {
            dst: page_addr,
            src: page.as_ptr() as u64,
            len: PAGE,
            mode: 0,
            copy: 0,
        };
        if unsafe { libc::ioctl(self.recorder.uffd_fd, UFFDIO_COPY_IOCTL, &mut copy) } != 0 {
            let err = io_err();
            if self.faults < 3 {
                dbg(
                    &self.recorder,
                    &format!("COPY FAIL dst={page_addr:#x}: {err}"),
                );
            }
            warn!(
                "hotset record: UFFDIO_COPY failed at {page_addr:#x}: {}",
                err
            );
            return;
        }
        if self.faults < 3 {
            dbg(&self.recorder, &format!("COPY OK dst={page_addr:#x}"));
        }
        self.buf.extend_from_slice(&vol_off.to_le_bytes());
    }

    fn flush(&mut self) {
        if self.buf.is_empty() {
            return;
        }
        if let Err(e) = self.trace.write_all(&self.buf) {
            warn!("hotset record: trace write failed: {e}");
            return;
        }
        self.buf.clear();
    }
}

fn pread_full(fd: i32, buf: &mut [u8], offset: u64) -> bool {
    let mut filled = 0usize;
    while filled < buf.len() {
        let n = unsafe {
            libc::pread(
                fd,
                buf[filled..].as_mut_ptr() as *mut libc::c_void,
                buf.len() - filled,
                (offset as usize + filled) as libc::off_t,
            )
        };
        if n < 0 {
            if errno_is_retry() {
                continue;
            }
            return false;
        }
        if n == 0 {
            return false;
        }
        filled += n as usize;
    }
    true
}

fn errno_is_retry() -> bool {
    std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted
}

fn io_err() -> std::io::Error {
    std::io::Error::last_os_error()
}

#[cfg(test)]
mod tests {
    use super::*;
    use vm_memory::{FileOffset, GuestAddress, MmapRegion};

    /// Drive the real recording path without KVM: build a volume file whose
    /// pages carry their own index, activate via the marker, back one region
    /// with the recording memfd, touch it out of order, and assert the trace
    /// replays the touch order (offsets resolved through the handler's
    /// pread+UFFDIO_COPY service).
    ///
    /// Ignored by default: creating a userfaultfd is denied to unprivileged
    /// users on hosts with vm.unprivileged_userfaultfd=0 (CI/dev boxes);
    /// run with --ignored as root (as production shims run) or on a host
    /// that permits it.
    #[test]
    #[ignore = "userfaultfd requires root or vm.unprivileged_userfaultfd=1"]
    fn test_recorder_traces_first_touch_order() {
        const REGION_LEN: u64 = 16 * PAGE; // 16 pages
        let dir = std::env::temp_dir().join(format!(
            "hotset-record-{}",
            std::process::id()
        ));
        fs::create_dir_all(&dir).unwrap();
        let volume_path = dir.join("volume");
        // Volume page i starts with byte i.
        let mut volume = vec![0u8; REGION_LEN as usize];
        for i in 0..16 {
            volume[i * PAGE as usize] = i as u8;
        }
        fs::write(&volume_path, &volume).unwrap();
        fs::write(dir.join(MARKER_FILE), b"").unwrap();

        activate_if_requested(&dir, &volume_path).expect("activate recording");
        assert!(active());

        let saved = File::open(&volume_path).unwrap();
        let memfd = take_memfd_region_backing(Some(saved)).expect("recording memfd");
        let region = Arc::new(
            GuestRegionMmap::new(
                MmapRegion::build(
                    Some(FileOffset::new(memfd, 0)),
                    REGION_LEN as usize,
                    libc::PROT_READ | libc::PROT_WRITE,
                    libc::MAP_SHARED | libc::MAP_NORESERVE,
                )
                .unwrap(),
                GuestAddress(0),
            )
            .unwrap(),
        );
        register_region(&region, 0);

        // Touch pages out of order from a plain thread — every first touch
        // must be serviced by the handler and recorded in call order.
        let base = region.as_ptr() as u64;
        for &page in &[5u64, 2, 9, 0, 15] {
            unsafe {
                let p = (base + page * PAGE) as *mut u8;
                let v = std::ptr::read_volatile(p);
                std::ptr::write_volatile(p, v.wrapping_add(1));
            }
        }

        // Wait for the trace to carry the five offsets in touch order.
        let trace_path = dir.join(format!("{TRACE_PREFIX}{}", std::process::id()));
        let want: Vec<u64> = [5u64, 2, 9, 0, 15].iter().map(|p| p * PAGE).collect();
        let mut got: Vec<u64> = Vec::new();
        for _ in 0..100 {
            if let Ok(data) = fs::read(&trace_path) {
                if data.len() >= 12 {
                    got = data[12..]
                        .chunks_exact(8)
                        .map(|b| u64::from_le_bytes(b.try_into().unwrap()))
                        .collect();
                    if got.len() >= want.len() {
                        break;
                    }
                }
            }
            std::thread::sleep(std::time::Duration::from_millis(20));
        }
        assert_eq!(got[..want.len()], want[..], "trace must match touch order");

        // The serviced pages carry the volume content: the touch read the
        // volume byte (page index) and wrote index+1 back, and that write
        // landed on the memfd page the handler filled.
        for &page in &[5u64, 2, 9, 0, 15] {
            let p = (base + page * PAGE) as *const u8;
            let v = unsafe { std::ptr::read_volatile(p) };
            assert_eq!(v, page as u8 + 1, "page {page} content must come from the volume");
        }

        let _ = fs::remove_dir_all(&dir);
    }
}
