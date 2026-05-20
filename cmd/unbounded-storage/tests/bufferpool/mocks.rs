// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! DST-aware mocks for the bufferpool's `Transport` and `BlockStore`.
//!
//! Each async method routes its "I/O latency" through [`yield_n`]
//! with a per-call random count drawn from the framework's
//! [`SimState::rng`], and optionally returns a synthetic error
//! governed by [`MockSimConfig::io_fault_rate`]. The simulation knobs
//! (delay bound, fault rate, cache hit rate) live here rather than
//! in the framework so the DST framework remains project-agnostic.
//! Counters are exposed so tests can assert higher-level properties
//! (e.g. single-flight coalescing).

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::rc::Rc;

use rand::Rng;
use smallvec::SmallVec;
use unbounded_storage::bufferpool::{
    BlockStore, Error, INLINE_PAGES, PageRange, PageRef, PageReply, PeerId, Req, StripeKey,
    Transport,
};

use crate::framework::executor::{with_sim, yield_n};

/// Bufferpool-specific simulation knobs that ride alongside the
/// framework's [`SimState`]. Held behind an `Rc` so both mocks plus
/// the workload driver can share a single configuration instance
/// without leaking knowledge into the framework crate.
#[derive(Default)]
pub struct MockSimConfig {
    /// Maximum number of `yield_once` pends an I/O mock will emit
    /// before completing. The actual count per call is drawn from
    /// the framework's PRNG.
    pub max_io_delay: Cell<u32>,
    /// Probability in `[0, 100]` that an I/O mock returns a
    /// synthetic error after its delay. `0` disables faults (the
    /// happy-path regime); positive values exercise the
    /// leader-error / `ParkOutcome::Error` paths in `pool.rs`.
    pub io_fault_rate: Cell<u32>,
}

impl MockSimConfig {
    pub fn new() -> Rc<Self> {
        Rc::new(Self::default())
    }
}

/// Helper: draw a `[0, max_io_delay]` delay from the framework PRNG.
fn draw_delay(cfg: &MockSimConfig) -> u32 {
    let max = cfg.max_io_delay.get();
    if max == 0 {
        0
    } else {
        with_sim(|s| s.rng.gen_range(0..=max))
    }
}

/// Helper: draw a fault decision from the framework PRNG.
fn draw_fault(cfg: &MockSimConfig) -> bool {
    let rate = cfg.io_fault_rate.get();
    rate > 0 && with_sim(|s| s.rng.gen_ratio(rate.min(100), 100))
}

/// Test request type. The pool only inspects `req.key()`.
#[derive(Clone, Debug)]
pub struct TestReq {
    pub key: StripeKey,
}

impl Req for TestReq {
    fn key(&self) -> StripeKey {
        self.key
    }
}

/// Canonical bytes per stripe. The transport copies out of this map
/// on `bulk_get`; tests use it as the oracle for byte verification.
pub type Stripes = Rc<RefCell<HashMap<StripeKey, Vec<u8>>>>;

#[derive(Default)]
pub struct CallCounts {
    pub bulk_get: Cell<u32>,
    pub read_page: Cell<u32>,
    pub write_page: Cell<u32>,
    /// `(key, page_no) -> bulk_get count` for the single-flight
    /// invariant. Keyed by intra-stripe page number, not byte offset.
    pub bulk_get_by_page: RefCell<HashMap<(StripeKey, u64), u32>>,
    /// `(key, page_no) -> max observed in-flight `bulk_get`s`. The
    /// single-flight invariant tolerates sequential re-issues
    /// (slot recycled, then refetched later) but forbids two
    /// `bulk_get`s overlapping for the same logical page.
    pub bulk_get_inflight: RefCell<HashMap<(StripeKey, u64), u32>>,
    pub bulk_get_max_inflight: RefCell<HashMap<(StripeKey, u64), u32>>,
}

pub struct DstTransport {
    stripes: Stripes,
    counts: Rc<CallCounts>,
    cfg: Rc<MockSimConfig>,
    /// Bound at construction. The `Transport` trait no longer
    /// carries a `register_pages` hook: the embedder pre-registers
    /// the backing out-of-band, so the mock learns the geometry the
    /// same way a real transport does (constructor argument from
    /// whoever owns the `Backing`).
    base: *mut u8,
    page_size: usize,
}

impl DstTransport {
    pub fn new(
        stripes: Stripes,
        counts: Rc<CallCounts>,
        cfg: Rc<MockSimConfig>,
        base: *mut u8,
        page_size: usize,
    ) -> Self {
        Self {
            stripes,
            counts,
            cfg,
            base,
            page_size,
        }
    }
}

impl Transport<TestReq> for DstTransport {
    async fn bulk_get(
        &self,
        _req: &TestReq,
        range: PageRange,
        dst_pages: &[PageRef],
    ) -> Result<SmallVec<[PageReply; INLINE_PAGES]>, Error> {
        // Pull delay and (optional) fault decision up front; this
        // keeps the PRNG draws deterministic across re-orderings of
        // independent tasks.
        let delay = draw_delay(&self.cfg);
        let fault = draw_fault(&self.cfg);

        assert_eq!(
            dst_pages.len(),
            range.len() as usize,
            "DstTransport: dst_pages length must match range length",
        );

        let page_size = self.page_size;
        // Track concurrent in-flight per (stripe, page_no) for the
        // single-flight invariant. With chunked transport, one call
        // may cover several pages; count each page in the range.
        {
            let mut inflight = self.counts.bulk_get_inflight.borrow_mut();
            let mut max = self.counts.bulk_get_max_inflight.borrow_mut();
            for p in range.start_page..range.end_page {
                let entry = inflight.entry((range.stripe, p as u64)).or_insert(0);
                *entry += 1;
                let cur = *entry;
                let m = max.entry((range.stripe, p as u64)).or_insert(0);
                if cur > *m {
                    *m = cur;
                }
            }
        }
        yield_n(delay).await;
        if fault {
            let mut inflight = self.counts.bulk_get_inflight.borrow_mut();
            for p in range.start_page..range.end_page {
                if let Some(e) = inflight.get_mut(&(range.stripe, p as u64)) {
                    *e = e.saturating_sub(1);
                }
            }
            return Err(Error::from("dst: injected transport fault"));
        }

        // Copy stripe bytes into each destination page.
        let stripes = self.stripes.borrow();
        let bytes = stripes
            .get(&range.stripe)
            .expect("DstTransport: stripe not configured");

        let mut replies: SmallVec<[PageReply; INLINE_PAGES]> = SmallVec::new();
        for (i, dst) in dst_pages.iter().enumerate() {
            let page_no = range.start_page as usize + i;
            let start = page_no * page_size;
            let copy_len = dst.len as usize;
            assert!(
                start + copy_len <= bytes.len(),
                "DstTransport: page out of range",
            );
            // SAFETY: dst is a pool-owned page within the registered
            // backing; src is a Vec<u8> owned by `stripes`. Both
            // ranges are valid for the duration of this call.
            unsafe {
                let dst_ptr = self
                    .base
                    .add(dst.page_idx as usize * page_size + dst.offset as usize);
                std::ptr::copy_nonoverlapping(bytes.as_ptr().add(start), dst_ptr, copy_len);
            }
            replies.push(PageReply {
                page_idx: dst.page_idx,
                byte_len: copy_len as u32,
            });
        }

        self.counts.bulk_get.set(self.counts.bulk_get.get() + 1);
        {
            let mut by_page = self.counts.bulk_get_by_page.borrow_mut();
            for p in range.start_page..range.end_page {
                *by_page.entry((range.stripe, p as u64)).or_insert(0) += 1;
            }
        }
        let mut inflight = self.counts.bulk_get_inflight.borrow_mut();
        for p in range.start_page..range.end_page {
            if let Some(e) = inflight.get_mut(&(range.stripe, p as u64)) {
                *e = e.saturating_sub(1);
            }
        }
        Ok(replies)
    }

    async fn probe(&self, _req: &TestReq, _range: PageRange, _peer: PeerId) -> Result<bool, Error> {
        Ok(true)
    }
}

// SAFETY: DST tests are single-threaded and the executor is pinned;
// the production `Transport` trait requires `Send + Sync + 'static`,
// so we manually attest the mock satisfies it under the DST runtime
// model. See AGENTS.md "DST mock" note about `!Send` types in the
// single-threaded executor.
unsafe impl Send for DstTransport {}
unsafe impl Sync for DstTransport {}

/// Blockstore mock with a configurable hit rate. On a miss
/// (probability `1 - hit_rate/100`) returns `Ok(false)` and the
/// pool falls through to `Transport::bulk_get`. On a hit, copies
/// the canonical stripe bytes into the destination page and
/// returns `Ok(true)`, exercising the fast-path branch in
/// `Pool::fetch_page` that *skips* `bulk_get` and the tee.
pub struct DstBlockStore {
    counts: Rc<CallCounts>,
    stripes: Stripes,
    cfg: Rc<MockSimConfig>,
    base: Cell<Option<*mut u8>>,
    page_size: Cell<usize>,
    /// `0` = miss-only (the original v1 behavior); `100` = always
    /// hit; intermediate values inject hits probabilistically.
    hit_rate: Cell<u32>,
}

impl DstBlockStore {
    pub fn new(counts: Rc<CallCounts>, stripes: Stripes, cfg: Rc<MockSimConfig>) -> Self {
        Self {
            counts,
            stripes,
            cfg,
            base: Cell::new(None),
            page_size: Cell::new(0),
            hit_rate: Cell::new(0),
        }
    }

    pub fn set_hit_rate(&self, pct: u32) {
        self.hit_rate.set(pct.min(100));
    }
}

impl BlockStore for DstBlockStore {
    fn register_pages(
        &self,
        base: *mut u8,
        page_size: usize,
        _page_count: usize,
    ) -> Result<(), Error> {
        self.base.set(Some(base));
        self.page_size.set(page_size);
        Ok(())
    }

    async fn read_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, Error> {
        let delay = draw_delay(&self.cfg);
        let hit = self.hit_rate.get() > 0
            && with_sim(|s| s.rng.gen_ratio(self.hit_rate.get().min(100), 100));
        yield_n(delay).await;
        self.counts.read_page.set(self.counts.read_page.get() + 1);
        if !hit {
            return Ok(false);
        }

        // Hit: copy oracle bytes for this page into `dst`. Mirrors
        // what a real on-disk cache would do.
        let page_size = self.page_size.get();
        let stripes = self.stripes.borrow();
        let bytes = stripes
            .get(&key)
            .expect("DstBlockStore: stripe not configured");
        let start = stripe_off as usize;
        let end = start + page_size;
        assert!(end <= bytes.len(), "DstBlockStore: stripe_off out of range");
        let base = self.base.get().expect("register_pages must run first");
        // SAFETY: dst is a pool-owned page within the registered
        // backing; oracle bytes outlive this call.
        unsafe {
            let dst_ptr = base.add(dst.page_idx as usize * page_size + dst.offset as usize);
            std::ptr::copy_nonoverlapping(bytes.as_ptr().add(start), dst_ptr, page_size);
        }
        Ok(true)
    }

    async fn write_page(
        &self,
        _key: StripeKey,
        _stripe_off: u64,
        _page: PageRef,
    ) -> Result<(), Error> {
        let delay = draw_delay(&self.cfg);
        yield_n(delay).await;
        self.counts.write_page.set(self.counts.write_page.get() + 1);
        Ok(())
    }
}

// SAFETY: see the equivalent impls on `DstTransport` above. The DST
// runtime is single-threaded; `BlockStore`'s blanket `Arc<T>` impl
// needs `T: Sync`, and various producer code paths in the pool
// require `Send + Sync`, so we manually attest the mock.
unsafe impl Send for DstBlockStore {}
unsafe impl Sync for DstBlockStore {}
