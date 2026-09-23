# Template Memory Hotset Profiling & Restore Prewarm

## 1. Summary

The build flow gets a new switch (`CUBE_TEMPLATE_PROFILE_ENABLE`, off by default). When it's on, the build doesn't report success as soon as the snapshot lands on disk: it first boots two throwaway sandboxes from the brand-new template through the normal fast-restore path, records which memory pages the guest actually touches between resume and ready, intersects the two runs, and ships the result as an extent list with the template — only then is the build marked successful.

From then on, every sandbox started from that template checks for a profile at restore time. If one is present, we pre-read exactly those regions into the page cache, so guest first-touch faults hit memory instead of stalling on disk reads. If not, the behavior is byte-for-byte identical to the original. Prewarm is a pure optimization: any failure anywhere just degrades to the original behavior — it can never block a template release or a sandbox create.

## 2. Background: why cold starts are slow

fast-restore `MAP_PRIVATE`-maps the template memory file into the VMM. After that, the guest's first touch of every page is a real disk read:

- **Warm** (file already in page cache): faults hit memory and startup latency is decoupled from the disk.
- **Cold** (after a reboot or heavy memory reclaim, cache is empty): every fault blocks on an NVMe read, and disk I/O becomes the dominant part of cold-start latency.

One structural fact makes this very fixable: every sandbox from the same template maps the **same shared inode** ("share map memory file"), so the page cache is shared across all of them. Warm that one file's hot set once, and every sandbox on the node — no matter how many run concurrently — benefits.

## 3. How it works: profile the startup set, then prewarm exactly that

- **Profiling.** A restored guest loads a page the first time it touches it, so after a verification sandbox runs from resume to ready, the pages marked *present* in the VMM's mapping of the memory file are precisely "the pages a startup loads". We read `/proc/<pid>/{maps,pagemap}`, walk the mapping page by page, and translate virtual addresses back to file offsets (§4.2) — no VMM internals needed.
- **Why two rounds.** We profile two consecutive restores and keep only the pages present in both. The two runs overlap heavily — what a startup loads is highly reproducible — and the intersection drops one-off touches caused by jitter.
- **What the startup set looks like.** The startup hot set is a small fraction of the memory file, and most of it is *not* concentrated at the head. An earlier attempt pre-read a fixed window at the head of the file and covered only a small part of the hot set — the evidence for why profiling is required and a fixed window won't do.
- **Prewarm.** For each extent we issue `posix_fadvise(POSIX_FADV_WILLNEED)` — asynchronous, returns immediately. The hot set is small, so the sequential reads finish in tens of milliseconds, overlapping device restore and guest startup. Under high concurrency the first create warms the cache and the rest are near no-ops.
- **Why it must be precise.** Pre-reading the whole file (GBs) measured *worse* than doing nothing: the prefetch I/O competes with many concurrent fault streams for the NVMe queue, hot pages end up queued behind cold ones. Precision is the whole point.

## 4. Integration Design

### 4.1 Overview: produced at build time, consumed at run time

```
 Template build flow (producer; once per build; behind the build switch, default off)
 ────────────────────────────────────────────────────────
 create snapshot succeeded (memory volume + VM state on disk)
   ├─ switch off (default) → template ready → notify user
   └─ switch on → template state = building (user not notified yet), profiling phase:
        ├─ round 1: boot a throwaway verification sandbox from the template via the normal
        │           create path, restore → ready → read pagemap → destroy
        ├─ round 2: same again (cache already warm from round 1, so it's faster)
        ├─ intersect the two page sets → merge into extents → atomically write hot-pages.json
        │           into the snapshot state dir (next to memory-ranges)
        └─ template state = ready → notify user of build success

 Normal sandbox create (consumer; every restore; independent of the build switch)
 ────────────────────────────────────────────────────────
 after opening the memory file: read hot-pages.json from the snapshot state dir and validate
   ├─ valid → posix_fadvise(WILLNEED) per extent
   └─ missing/invalid → no pre-read; byte-for-byte identical to the original scheme
```

The ground rule: **prewarm is a pure optimization**. Anything going wrong in the profiling path degrades to existing behavior; it never blocks template publishing or sandbox creation.

### 4.2 Producing the profile: two verification restores at the end of a build

**When**: after create-snapshot succeeds (memory volume + VM state on disk) and before the template flips to ready. The user sees "building" for the whole window and hears about success only once profiling is done — profiling time is part of build time.

**Precondition**: the build switch is on (`CUBE_TEMPLATE_PROFILE_ENABLE`, default off). When it's off, the whole phase is skipped: snapshot success → ready immediately, build time and behavior identical to the original, no profile file.

**Steps**:

1. Delete any existing hot-pages.json first — **unconditionally, regardless of the switch**. A profile left over from a previous build of this template must never ride along with a rebuild;
2. Round 1: boot a verification sandbox from the brand-new template through the **normal create path**, restore → ready;
3. Collect: read the sandbox VMM's `/proc/<pid>/maps` to find the memory-file mapping and `/proc/<pid>/pagemap` for present pages; file offset = mapping offset + (vaddr − mapping start);
4. Destroy the sandbox; repeat for round 2;
5. Intersect the two page sets (page granularity), merge neighbors into extents, atomically write the JSON (temp file + rename) into the snapshot state dir;
6. Template state → ready, notify the user.

### 4.3 Profile file format and placement

```json
{
  "version": 1,
  "template_id": "tpl-xxxx",
  "mem_file_size": 2147483648,
  "profiled_at": "2026-09-20T03:00:00Z",
  "extents": [[offset_bytes, len_bytes], ...]
}
```

- Each extent is **exactly a two-element tuple** `[offset_bytes, len_bytes]`. The consumer parses strictly as `[u64; 2]` — one extra field (even a third element) fails the parse of the whole file, which downgrades to no prewarm with a debug log. The v1 consumer reads **no other fields**;
- The file lives in the template package's snapshot state dir, next to `memory-ranges` (`<package>/metadata/snapshot/hot-pages.json`). That dir is the `source_url` the VMM already knows at restore time, so no extra parameters need to travel. Semantically it's part of the template metadata and follows the template lifecycle: deleted with the template, dropped and rewritten on a rebuild;
- **Distribution is free**: the profile rides with the snapshot package metadata, no new packaging logic. On the XFS backend metadata is a local directory (template packages are node-local; "distribution" just means each node builds its own); on the S3 backend the profile is written before the metadata volume gets sealed (finalize) and travels inside it — after restore mounts the metadata volume, it's visible at the same relative path;
- **Validation rules** (consumer side; any failure → invalid → no prewarm, fall back): recognized version, `mem_file_size` matches the actual file, extents in bounds, total within 50% of memory size (a runaway guard). Keeping stale profiles out is the producer's job: the build deletes them unconditionally before the phase, with `mem_file_size` equality plus the schema version as backstops;

### 4.4 Consuming the profile: one hook on the fast-restore path

The fast-restore branch (memory_manager.rs `new_from_snapshot`, `open_read()` under fast_restore) mmaps straight after opening the memory file. The consumer slots in right there:

```
after opening the memory file:
  read hot-pages.json from the snapshot state dir and validate
    ├─ valid: posix_fadvise(WILLNEED) per extent — async, returns immediately
    └─ missing/invalid: no pre-read; identical to the original logic
```

- fadvise returns before the reads finish, so the prewarm overlaps device restore and guest startup and never sits on the restore critical path;
- **seccomp prerequisite**: the vmm thread allowlist (seccomp_filters.rs) didn't include `fadvise64`. It's now allowed via a named `SYS_FADVISE64` const (x86_64 takes `libc::SYS_fadvise64`; aarch64 libc doesn't export it, so we use the asm-generic syscall number 223);
- under high concurrency, every create fadvises the same shared inode; the kernel merges duplicate reads at the page-cache layer, so the actual I/O is roughly one copy of the hot set;

### 4.5 Fallback and compatibility guarantees (hard requirements)

Production is a **pure consumer** (read-only); generation only exists inside the build flow. The fallback path is the default behavior itself (no pre-read):

| Scenario | Behavior |
|---|---|
| Existing templates / templates built with the switch off (default) / profiling phase failed / profile missing on a newly distributed node | No profile, **identical to the original path**; the production path never compensates by generating either |
| Profile missing/corrupt/failing validation | Same as above, debug log |
| Verification restore failed / phase timed out (60s cap) | Skip profiling, **template still marked successful** (no profile), warn log — profiling failure never blocks publishing |
| Stale profile from a previous build | Deleted **unconditionally** by the build before the phase, switch on or off (§4.2) |
| Verification sandbox cleanup failed | Build flow retries destroy; leftovers fall to the regular sandbox reaper |
| fadvise failed (any extent) | warn + skip that extent, restore continues |
| Collection failed (pagemap read / disk write) | Same as phase failure: template still ready, no profile |
| Consumer switch disabled / build switch off | Consumer reads nothing / producer generates nothing; both fully back to the original path |
| UMA / legacy config / version rollback | No profile file → no behavior difference; to old versions it's an invisible side file |

The invariant: **template publishing and sandbox creation must never fail or measurably slow down because of profiling** (consumer validation is a pure in-memory check; producer failures are always fail-open). No new API, no frontend changes; template state reuses the existing "building → ready" semantics, just flipped a bit later.

### 4.6 Configuration switches

| Variable | Default | Meaning |
|---|---|---|
| `CUBE_VMM_RESTORE_HOTSET_DISABLE` | unset | `1`/`true`: the consumer reads no profile (restore path fully back to the original path) |
| `CUBE_TEMPLATE_PROFILE_ENABLE` | unset (**no generation by default**) | `1`/`true`: run two-round profiling at the end of a build; unset: build flow identical to the original path (no profiling phase, no extra time) |
| `CUBE_TEMPLATE_PROFILE_TIMEOUT_SEC` | 60 | Timeout for the whole profiling phase (both rounds); timeout handled per §4.5 (only effective when the build switch is on) |

## 5. Design Decisions

1. **Two rounds plus an intersection is the finalized shape** (profiling from a snapshot resume inside the build pipeline): the intersection drops one-off touches.
2. **Profile what a real restore touches, nothing more.** The touch set at snapshot time is a "boot from zero + customization" superset — the heavier the customization, the dirtier it gets. The two verification restores also double as a restore sanity check.
3. **Producer and consumer are fully decoupled.** Generation lives in the build flow (controller-side /proc collection, zero shim changes); consumption lives in shim/VMM (JSON + seccomp, ~150 lines). Production sandboxes gain zero new write paths.
4. **Generation is off by default, on explicitly.** Prewarm trades build time, and whether that trade is worth it differs by deployment and template. Default off keeps existing build flows untouched; turn it on per batch. The consumer doesn't care about this switch — a profile present means prewarm.
5. **One copy of page cache.** The prewarm I/O lands wherever the issuing thread runs, and cross-node access is memory speed anyway — no per-node cache copies for locality.
6. **Prewarm is a cache operation, not a copy.** No sandbox files are written, no CoW; reflink/shared pages keep sharing the same cache page until written.
7. **Losing the cache only degrades, never worsens.** If reclaim evicts the pages between prewarm and create, we fall back to per-fault reads — the baseline.
