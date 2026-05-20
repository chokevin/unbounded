// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::future::Future;
use std::marker::PhantomData;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use smallvec::SmallVec;

use crate::bufferpool::free_list::FreeList;
use crate::bufferpool::inflight::{PageSlot, SlotState, StripeFetch};
use crate::bufferpool::stream::{LocalBoxFuture, ReadStream, StreamSrc};
use crate::bufferpool::traits::{BlockStore, BufferPool, Req, Transport};
use crate::bufferpool::types::{
    Backing, Error, INLINE_PAGES, PageRange, PageRef, PageReply, PoolConfig, StripeKey,
};

/// One per shard. Per the design's runtime model, a single `Pool`
/// runs single-threaded inside its NUMA shard; the embedder pins
/// the executor and is expected to construct the pool off-thread
/// before handing it to the pinned executor for service.
pub struct Pool<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    inner: Rc<PoolInner<T, S, R>>,
}

pub(super) struct PoolInner<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    pub(super) cfg: PoolConfig,
    pub(super) backing: Backing,
    pub(super) page_size: usize,
    pub(super) free: FreeList,
    pub(super) inflight: RefCell<HashMap<StripeKey, Rc<RefCell<StripeFetch>>>>,
    pub(super) stream_count: Cell<usize>,
    pub(super) transport: T,
    pub(super) blockstore: S,
    _r: PhantomData<R>,
}

impl<T, S, R> Pool<T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + Send + Sync + 'static,
{
    /// One per NUMA shard. Carves `backing` into pages and calls
    /// `blockstore.register_pages(...)` exactly once. The
    /// `Transport` is expected to have been bound to the same
    /// `backing` out-of-band by the embedder before this call (e.g.
    /// via `mercury::Class::register_backing`); the pool no longer
    /// drives that handshake. No async I/O happens here; on a real
    /// RDMA `Transport` the synchronous `ibv_reg_mr` is the
    /// dominant cost (see the design's "Page registration"
    /// section). Embedders should run `Pool::new` off the pinned
    /// executor thread before handing the constructed `Pool` to the
    /// per-shard executor for service.
    pub fn new(
        cfg: PoolConfig,
        backing: Backing,
        transport: T,
        blockstore: S,
    ) -> Result<Self, Error> {
        if backing.page_size == 0 || !backing.page_size.is_power_of_two() {
            return Err(Error::BadConfig("page_size must be a power of two"));
        }
        if backing.page_count == 0 {
            return Err(Error::BadConfig("page_count must be > 0"));
        }
        if backing.page_count > u32::MAX as usize {
            return Err(Error::BadConfig("page_count exceeds u32 (see design)"));
        }
        if backing.base.is_null() {
            return Err(Error::BadConfig("backing.base is null"));
        }
        if cfg.pages_per_chunk == 0 {
            return Err(Error::BadConfig("pages_per_chunk must be non-zero"));
        }

        blockstore.register_pages(backing.base, backing.page_size, backing.page_count)?;

        let page_count_u32 = backing.page_count as u32;
        let page_size = backing.page_size;
        let inner = Rc::new(PoolInner {
            cfg,
            backing,
            page_size,
            free: FreeList::new(page_count_u32),
            inflight: RefCell::new(HashMap::new()),
            stream_count: Cell::new(0),
            transport,
            blockstore,
            _r: PhantomData,
        });
        Ok(Self { inner })
    }

    /// Test-only accessor for the free list size. Also used by the
    /// out-of-tree DST harness under `tests/`.
    pub fn free_pages(&self) -> usize {
        self.inner.free.available()
    }

    /// Test-only accessor for the inflight map size. Also used by
    /// the out-of-tree DST harness under `tests/`.
    pub fn inflight_entries(&self) -> usize {
        self.inner.inflight.borrow().len()
    }

    /// Test-only: drive a chunk-bounded range fetch directly. Used
    /// by the bufferpool unit tests to assert the splitter calls
    /// `Transport::bulk_get` once per chunk with the expected
    /// `PageRange`. Not part of the public surface; the embedder
    /// goes through `BufferPool::read`.
    #[cfg(test)]
    pub(crate) async fn fetch_range_for_test(
        &self,
        req: &R,
        range: PageRange,
        dst_pages: &[PageRef],
    ) -> Result<(), Error> {
        fetch_range_via_transport(&self.inner, req, range, dst_pages).await
    }
}

impl<T, S, R> BufferPool for Pool<T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + Send + Sync + 'static,
{
    type Req = R;

    async fn read<'p>(
        &'p self,
        req: &'p Self::Req,
        offset: u64,
        len: u64,
    ) -> Result<ReadStream<'p>, Error> {
        let cur = self.inner.stream_count.get();
        if cur >= self.inner.cfg.max_concurrent_streams {
            return Err(Error::StreamLimit);
        }
        self.inner.stream_count.set(cur + 1);

        let key = req.key();
        let fetch = {
            let mut inflight = self.inner.inflight.borrow_mut();
            inflight
                .entry(key)
                .or_insert_with(|| Rc::new(RefCell::new(StripeFetch::new())))
                .clone()
        };
        fetch.borrow_mut().stream_refcount += 1;

        let end = offset.saturating_add(len);
        let src: Rc<dyn StreamSrc + 'p> =
            Rc::new(StreamSrcImpl::new(self.inner.clone(), req, key, fetch));
        Ok(ReadStream::new(src, offset, end, self.inner.page_size))
    }
}

// ---------------------------------------------------------------------------
// StreamSrc: type-erased per-stream view onto the typed PoolInner.
// ---------------------------------------------------------------------------

pub(super) struct StreamSrcImpl<'p, T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + Send + Sync + 'static,
{
    inner: Rc<PoolInner<T, S, R>>,
    req: &'p R,
    key: StripeKey,
    fetch: Rc<RefCell<StripeFetch>>,
    /// Tracks whether `decrement_stream` has already run (it must
    /// happen exactly once per `ReadStream`). Drop ordering of
    /// `Rc<StreamSrcImpl>` is "last `PageGuard` or last
    /// `ReadStream` reference"; the cleanup belongs to the stream,
    /// not to outstanding guards, so we do it eagerly via
    /// `decrement_stream`.
    stream_decremented: Cell<bool>,
}

impl<'p, T, S, R> StreamSrcImpl<'p, T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + Send + Sync + 'static,
{
    pub(super) fn new(
        inner: Rc<PoolInner<T, S, R>>,
        req: &'p R,
        key: StripeKey,
        fetch: Rc<RefCell<StripeFetch>>,
    ) -> Self {
        Self {
            inner,
            req,
            key,
            fetch,
            stream_decremented: Cell::new(false),
        }
    }
}

impl<'p, T, S, R> StreamSrc for StreamSrcImpl<'p, T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + Send + Sync + 'static,
{
    fn page_size(&self) -> usize {
        self.inner.page_size
    }

    fn base(&self) -> *mut u8 {
        self.inner.backing.base
    }

    fn key(&self) -> StripeKey {
        self.key
    }

    fn fetch_page<'a>(&'a self, page_no: u64) -> LocalBoxFuture<'a, Result<u32, Error>> {
        Box::pin(fetch_page(
            self.inner.clone(),
            self.fetch.clone(),
            self.key,
            self.req,
            page_no,
        ))
    }

    fn release_guard(&self, page_no: u64) {
        release_guard(&self.inner, self.key, &self.fetch, page_no);
    }

    fn decrement_stream(&self) {
        if self.stream_decremented.replace(true) {
            return;
        }
        decrement_stream(&self.inner, self.key, &self.fetch);
        let prev = self.inner.stream_count.get();
        self.inner.stream_count.set(prev.saturating_sub(1));
    }
}

// ---------------------------------------------------------------------------
// Cleanup helpers.
// ---------------------------------------------------------------------------

fn release_guard<T, S, R>(
    inner: &Rc<PoolInner<T, S, R>>,
    key: StripeKey,
    fetch: &Rc<RefCell<StripeFetch>>,
    page_no: u64,
) where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    let slot = match fetch.borrow().pages.get(&page_no).cloned() {
        Some(s) => s,
        None => return,
    };
    let prev = slot.consumer_holds.get();
    slot.consumer_holds.set(prev.saturating_sub(1));
    recycle_if_terminal(inner, key, fetch, &slot, page_no);
}

// ---------------------------------------------------------------------------
// ConsumerHold: RAII for `PageSlot::consumer_holds`.
// ---------------------------------------------------------------------------

/// RAII bump of `slot.consumer_holds`. Construction increments;
/// drop decrements and runs `recycle_if_terminal`. Used by parked
/// subscribers and the leader during I/O+tee to keep the slot
/// pinned across `.await` points. On success the hold is
/// transferred to the caller via [`ConsumerHold::forget`] and
/// balanced later by [`StreamSrc::release_guard`] when the
/// resulting `PageGuard` drops.
struct ConsumerHold<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    inner: Rc<PoolInner<T, S, R>>,
    fetch: Rc<RefCell<StripeFetch>>,
    slot: Rc<PageSlot>,
    key: StripeKey,
    page_no: u64,
    active: bool,
}

impl<T, S, R> ConsumerHold<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    fn new(
        inner: Rc<PoolInner<T, S, R>>,
        fetch: Rc<RefCell<StripeFetch>>,
        slot: Rc<PageSlot>,
        key: StripeKey,
        page_no: u64,
    ) -> Self {
        slot.consumer_holds.set(slot.consumer_holds.get() + 1);
        Self {
            inner,
            fetch,
            slot,
            key,
            page_no,
            active: true,
        }
    }

    /// Transfer ownership of the bump out of this guard. The caller
    /// is responsible for balancing it (typically via the
    /// `PageGuard` returned to the stream consumer).
    fn forget(mut self) {
        self.active = false;
    }
}

impl<T, S, R> Drop for ConsumerHold<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    fn drop(&mut self) {
        if !self.active {
            return;
        }
        let prev = self.slot.consumer_holds.get();
        self.slot.consumer_holds.set(prev.saturating_sub(1));
        recycle_if_terminal(&self.inner, self.key, &self.fetch, &self.slot, self.page_no);
    }
}

/// Returns `slot`'s page to the free list and removes the slot
/// (and possibly the entire `StripeFetch`) if it is in a terminal
/// state and no one is holding it. Shared between [`release_guard`]
/// and [`ConsumerHold::drop`] so the cleanup invariant lives in
/// one place.
fn recycle_if_terminal<T, S, R>(
    inner: &Rc<PoolInner<T, S, R>>,
    key: StripeKey,
    fetch: &Rc<RefCell<StripeFetch>>,
    slot: &Rc<PageSlot>,
    page_no: u64,
) where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    if !(slot.is_recyclable() && slot_is_terminal(slot)) {
        return;
    }
    let release_pi = {
        let mut f = fetch.borrow_mut();
        // Re-check under the borrow to guard against a concurrent
        // (re-entrant) update; in single-threaded use this is just
        // defensive but it is cheap.
        match f.pages.get(&page_no) {
            Some(s) if Rc::ptr_eq(s, slot) && s.is_recyclable() && slot_is_terminal(s) => {
                f.pages.remove(&page_no).and_then(|s| s.page_idx.get())
            }
            _ => None,
        }
    };
    if let Some(pi) = release_pi {
        inner.free.release(pi);
    }
    let should_remove = {
        let f = fetch.borrow();
        f.stream_refcount == 0 && f.pages.is_empty()
    };
    if should_remove {
        inner.inflight.borrow_mut().remove(&key);
    }
}

fn decrement_stream<T, S, R>(
    inner: &Rc<PoolInner<T, S, R>>,
    key: StripeKey,
    fetch: &Rc<RefCell<StripeFetch>>,
) where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    let to_release: Vec<u32> = {
        let mut f = fetch.borrow_mut();
        f.stream_refcount = f.stream_refcount.saturating_sub(1);
        let mut released = Vec::new();
        if f.stream_refcount == 0 {
            f.pages.retain(|_, slot| {
                if slot.is_recyclable() && slot_is_terminal(slot) {
                    if let Some(pi) = slot.page_idx.get() {
                        released.push(pi);
                    }
                    false
                } else {
                    true
                }
            });
        }
        released
    };

    for pi in to_release {
        inner.free.release(pi);
    }

    let should_remove = {
        let f = fetch.borrow();
        f.stream_refcount == 0 && f.pages.is_empty()
    };
    if should_remove {
        inner.inflight.borrow_mut().remove(&key);
    }
}

fn slot_is_terminal(slot: &PageSlot) -> bool {
    matches!(*slot.state.borrow(), SlotState::Ready | SlotState::Error(_))
}

// ---------------------------------------------------------------------------
// Single-flight fetch.
// ---------------------------------------------------------------------------

/// Drives one logical page's I/O through the single-flight state
/// machine. Cooperative leadership: the first poller to find the
/// slot in `Idle` becomes the leader and runs the I/O; later
/// pollers park in `Loading`. If the leader's future drops before
/// completing, the [`LeaderGuard`] flips state back to `Idle`,
/// wakes parked subscribers, and the next one takes over.
async fn fetch_page<T, S, R>(
    inner: Rc<PoolInner<T, S, R>>,
    fetch: Rc<RefCell<StripeFetch>>,
    key: StripeKey,
    req: &R,
    page_no: u64,
) -> Result<u32, Error>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + Send + Sync + 'static,
{
    let slot: Rc<PageSlot> = {
        let mut f = fetch.borrow_mut();
        f.pages
            .entry(page_no)
            .or_insert_with(|| Rc::new(PageSlot::new(page_no)))
            .clone()
    };

    loop {
        let action = {
            let mut st = slot.state.borrow_mut();
            match &mut *st {
                SlotState::Ready => Action::Ready,
                SlotState::Error(e) => Action::Error(e.clone()),
                SlotState::Loading(_) => Action::Park,
                SlotState::Idle => {
                    *st = SlotState::Loading(Vec::new());
                    Action::Lead
                }
            }
        };

        match action {
            Action::Ready => {
                let pi = slot
                    .page_idx
                    .get()
                    .expect("page_idx must be set when slot is Ready");
                // Bump and immediately transfer to the caller; the
                // returned `PageGuard` balances it via
                // `StreamSrc::release_guard`.
                ConsumerHold::new(inner.clone(), fetch.clone(), slot.clone(), key, page_no)
                    .forget();
                return Ok(pi);
            }
            Action::Error(e) => return Err(e),
            Action::Park => {
                // `ParkOnSlot` pre-bumps `consumer_holds` so the
                // leader's `PageGuard` drop cannot recycle the slot
                // between wake and re-poll. The hold is transferred
                // to us on `Ready`, dropped on `Error`/`Retry`.
                let park =
                    ParkOnSlot::new(inner.clone(), fetch.clone(), slot.clone(), key, page_no);
                match park.await {
                    ParkOutcome::Ready(hold) => {
                        let pi = slot
                            .page_idx
                            .get()
                            .expect("page_idx must be set when slot is Ready");
                        hold.forget();
                        return Ok(pi);
                    }
                    ParkOutcome::Error(e) => return Err(e),
                    ParkOutcome::Retry => continue,
                }
            }
            Action::Lead => {
                // Leader-owned `ConsumerHold` covers the entire I/O
                // phase plus the tee. Drop order on cancel: locals
                // declared first drop last, so `leader_guard` flips
                // state and wakes parked subscribers before
                // `leader_hold` drops and possibly recycles.
                let leader_hold =
                    ConsumerHold::new(inner.clone(), fetch.clone(), slot.clone(), key, page_no);
                let mut leader_guard = LeaderGuard {
                    slot: &slot,
                    completed: false,
                };

                if slot.page_idx.get().is_none() {
                    let pi = inner.free.alloc().await;
                    slot.page_idx.set(Some(pi));
                }
                let pi = slot.page_idx.get().expect("page_idx set above");
                let dst = PageRef {
                    page_idx: pi,
                    offset: 0,
                    len: inner.page_size as u32,
                };
                let stripe_off = page_no
                    .checked_mul(inner.page_size as u64)
                    .ok_or(Error::OffsetOutOfRange)?;

                // Phase 1: get bytes into the page (blocking the
                // leader; parked subscribers wait). On error,
                // transition to `Error` and propagate.
                let fetch_result: Result<bool, Error> = async {
                    let hit = inner.blockstore.read_page(key, stripe_off, dst).await?;
                    if !hit {
                        let start_page: u32 =
                            page_no.try_into().map_err(|_| Error::OffsetOutOfRange)?;
                        let range = PageRange {
                            stripe: key,
                            start_page,
                            end_page: start_page + 1,
                        };
                        fetch_range_via_transport(&inner, req, range, std::slice::from_ref(&dst))
                            .await?;
                    }
                    Ok(hit)
                }
                .await;

                let hit = match fetch_result {
                    Ok(h) => h,
                    Err(e) => {
                        let wakers = take_loading_wakers(&slot);
                        *slot.state.borrow_mut() = SlotState::Error(e.clone());
                        leader_guard.completed = true;
                        drop(leader_guard);
                        for w in wakers {
                            w.wake();
                        }
                        // `leader_hold` drops at the `return`,
                        // releasing the bump and (if no parked
                        // subscribers are still holding their own
                        // bumps) recycling the page via
                        // `recycle_if_terminal`.
                        return Err(e);
                    }
                };

                // Phase 2: mark Ready and wake parked subscribers
                // BEFORE running the tee, so non-leader subscribers
                // can consume bytes concurrently with the leader's
                // `write_page` (matches designs/bufferpool.md
                // "Pull-through with tee"). The page stays pinned
                // across the tee via `tee_pending` plus the
                // leader's `ConsumerHold`.
                let need_tee = !hit;
                if need_tee {
                    slot.tee_pending.set(true);
                }
                let wakers = take_loading_wakers(&slot);
                *slot.state.borrow_mut() = SlotState::Ready;
                leader_guard.completed = true;
                drop(leader_guard);
                for w in wakers {
                    w.wake();
                }

                // Phase 3: leader drives the tee. If the leader's
                // future is dropped here, `TeePendingGuard` clears
                // `tee_pending` first, then `leader_hold` drops and
                // runs the same recycle path as `release_guard`.
                // `write_page` errors are best-effort for v1 (see
                // designs/bufferpool.md TODO(partial-failure)).
                if need_tee {
                    let _tee_pending_guard = TeePendingGuard { slot: &slot };
                    let _ = inner.blockstore.write_page(key, stripe_off, dst).await;
                }

                leader_hold.forget();
                return Ok(pi);
            }
        }
    }
}

enum Action {
    Ready,
    Error(Error),
    Park,
    Lead,
}

struct LeaderGuard<'a> {
    slot: &'a Rc<PageSlot>,
    completed: bool,
}

impl<'a> Drop for LeaderGuard<'a> {
    fn drop(&mut self) {
        if self.completed {
            return;
        }
        // Leader future was dropped before reaching Ready (during
        // read_page or bulk_get). Reset to Idle so the next
        // subscriber takes over leadership; preserve `page_idx` for
        // reuse. v1 drain relaxation: see mod.rs.
        let wakers = take_loading_wakers(self.slot);
        *self.slot.state.borrow_mut() = SlotState::Idle;
        for w in wakers {
            w.wake();
        }
    }
}

/// Clears `slot.tee_pending` on drop. Drop order in [`fetch_page`]
/// ensures this runs before the leader's `ConsumerHold` drops, so
/// any subsequent recycle sees `tee_pending == false`.
struct TeePendingGuard<'a> {
    slot: &'a Rc<PageSlot>,
}

impl<'a> Drop for TeePendingGuard<'a> {
    fn drop(&mut self) {
        self.slot.tee_pending.set(false);
    }
}

/// Drains the parked-waker list out of a slot in `Loading`. Returns
/// an empty `Vec` if the slot has already moved on.
fn take_loading_wakers(slot: &Rc<PageSlot>) -> Vec<std::task::Waker> {
    let mut st = slot.state.borrow_mut();
    match &mut *st {
        SlotState::Loading(w) => std::mem::take(w),
        _ => Vec::new(),
    }
}

struct ParkOnSlot<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    inner: Rc<PoolInner<T, S, R>>,
    fetch: Rc<RefCell<StripeFetch>>,
    slot: Rc<PageSlot>,
    key: StripeKey,
    page_no: u64,
    /// `Some` once we have registered our waker and pre-bumped
    /// `consumer_holds`. Taken out and either transferred to the
    /// caller on `Ready` or dropped on `Error`/`Retry`/cancellation.
    hold: Option<ConsumerHold<T, S, R>>,
}

/// Outcome of awaiting [`ParkOnSlot`].
enum ParkOutcome<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    /// Slot reached `Ready`. The caller must `forget()` the hold so
    /// the bump survives to be balanced by the consumer's
    /// `PageGuard`.
    Ready(ConsumerHold<T, S, R>),
    Error(Error),
    /// Slot left `Loading` via leader-future drop. The caller
    /// re-enters the state machine.
    Retry,
}

impl<T, S, R> ParkOnSlot<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    fn new(
        inner: Rc<PoolInner<T, S, R>>,
        fetch: Rc<RefCell<StripeFetch>>,
        slot: Rc<PageSlot>,
        key: StripeKey,
        page_no: u64,
    ) -> Self {
        Self {
            inner,
            fetch,
            slot,
            key,
            page_no,
            hold: None,
        }
    }
}

impl<T, S, R> Future for ParkOnSlot<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    type Output = ParkOutcome<T, S, R>;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        // `ParkOnSlot` holds only `Rc`/`Option`/`Cell` fields; it
        // is `Unpin`.
        let this = self.get_mut();

        // First poll: register our waker and pre-bump
        // `consumer_holds`. `Action::Park` observed `Loading`
        // synchronously, but the leader may have completed before
        // we get here; bump unconditionally so the outcome paths
        // below always have a hold to consume, then fall through.
        if this.hold.is_none() {
            {
                let mut st = this.slot.state.borrow_mut();
                if let SlotState::Loading(wakers) = &mut *st {
                    wakers.push(cx.waker().clone());
                }
            }
            this.hold = Some(ConsumerHold::new(
                this.inner.clone(),
                this.fetch.clone(),
                this.slot.clone(),
                this.key,
                this.page_no,
            ));
        }

        let outcome = {
            let st = this.slot.state.borrow();
            match &*st {
                SlotState::Loading(_) => return Poll::Pending,
                SlotState::Ready => Outcome::Ready,
                SlotState::Error(e) => Outcome::Error(e.clone()),
                SlotState::Idle => Outcome::Retry,
            }
        };
        match outcome {
            Outcome::Ready => {
                let hold = this.hold.take().expect("hold registered above");
                Poll::Ready(ParkOutcome::Ready(hold))
            }
            Outcome::Error(e) => {
                let _ = this.hold.take();
                Poll::Ready(ParkOutcome::Error(e))
            }
            Outcome::Retry => {
                let _ = this.hold.take();
                Poll::Ready(ParkOutcome::Retry)
            }
        }
    }
}

/// Internal outcome enum used inside `ParkOnSlot::poll` to keep
/// the `slot.state` borrow scope minimal.
enum Outcome {
    Ready,
    Error(Error),
    Retry,
}

// ---------------------------------------------------------------------------
// Chunk splitter + range-fetch helper.
// ---------------------------------------------------------------------------

/// Split `range` into chunk-aligned sub-ranges of at most
/// `pages_per_chunk` pages each. The first chunk may be shorter
/// than `pages_per_chunk` if `range.start_page` does not land on a
/// chunk boundary; subsequent chunks align on multiples of
/// `pages_per_chunk` until the last (possibly short) chunk reaches
/// `range.end_page`.
///
/// Panics if `pages_per_chunk == 0`; callers in this crate go
/// through `PoolConfig` which rejects that at construction.
pub(crate) fn split_into_chunks(range: PageRange, pages_per_chunk: u32) -> Vec<PageRange> {
    assert!(pages_per_chunk > 0, "pages_per_chunk must be non-zero");
    let mut out = Vec::new();
    if range.is_empty() {
        return out;
    }
    let mut cur = range.start_page;
    while cur < range.end_page {
        // End of the current chunk: either the next chunk boundary
        // or the end of the requested range, whichever comes first.
        let next_boundary = (cur / pages_per_chunk + 1) * pages_per_chunk;
        let end = next_boundary.min(range.end_page);
        out.push(PageRange {
            stripe: range.stripe,
            start_page: cur,
            end_page: end,
        });
        cur = end;
    }
    out
}

/// Fetch `range` from the transport in chunk-aligned pieces. The
/// caller supplies one `PageRef` per page in `range`, ordered by
/// ascending `page_no`. Partial replies are treated as failures
/// for v1 (see `designs/bufferpool.md` TODO(partial-failure)).
pub(crate) async fn fetch_range_via_transport<T, S, R>(
    inner: &Rc<PoolInner<T, S, R>>,
    req: &R,
    range: PageRange,
    dst_pages: &[PageRef],
) -> Result<(), Error>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req + Send + Sync + 'static,
{
    assert_eq!(
        dst_pages.len(),
        range.len() as usize,
        "fetch_range_via_transport: dst_pages length must match range length",
    );
    let chunks = split_into_chunks(range, inner.cfg.pages_per_chunk);
    let mut offset_in_range: u32 = 0;
    for chunk in chunks {
        let chunk_len = chunk.len() as usize;
        let slice_start = offset_in_range as usize;
        let slice_end = slice_start + chunk_len;
        let chunk_dst = &dst_pages[slice_start..slice_end];
        let replies: SmallVec<[PageReply; INLINE_PAGES]> =
            inner.transport.bulk_get(req, chunk, chunk_dst).await?;
        if replies.len() < chunk_len {
            return Err(Error::Io(0));
        }
        offset_in_range += chunk.len();
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::types::StripeKey;

    fn stripe() -> StripeKey {
        StripeKey([7u8; 32])
    }

    #[test]
    fn splitter_single_chunk_when_aligned_and_small() {
        let r = PageRange {
            stripe: stripe(),
            start_page: 0,
            end_page: 5,
        };
        let chunks = split_into_chunks(r, 512);
        assert_eq!(chunks.len(), 1);
        assert_eq!(chunks[0], r);
    }

    #[test]
    fn splitter_spans_two_chunks_across_boundary() {
        let r = PageRange {
            stripe: stripe(),
            start_page: 500,
            end_page: 530,
        };
        let chunks = split_into_chunks(r, 512);
        assert_eq!(chunks.len(), 2);
        assert_eq!(chunks[0].start_page, 500);
        assert_eq!(chunks[0].end_page, 512);
        assert_eq!(chunks[1].start_page, 512);
        assert_eq!(chunks[1].end_page, 530);
    }

    #[test]
    fn splitter_spans_three_chunks() {
        let r = PageRange {
            stripe: stripe(),
            start_page: 500,
            end_page: 1100,
        };
        let chunks = split_into_chunks(r, 512);
        assert_eq!(chunks.len(), 3);
        assert_eq!((chunks[0].start_page, chunks[0].end_page), (500, 512));
        assert_eq!((chunks[1].start_page, chunks[1].end_page), (512, 1024));
        assert_eq!((chunks[2].start_page, chunks[2].end_page), (1024, 1100));
    }

    #[test]
    fn splitter_empty_range_yields_nothing() {
        let r = PageRange {
            stripe: stripe(),
            start_page: 10,
            end_page: 10,
        };
        assert!(split_into_chunks(r, 4).is_empty());
    }

    #[test]
    #[should_panic(expected = "pages_per_chunk must be non-zero")]
    fn splitter_panics_on_zero_chunk_size() {
        let r = PageRange {
            stripe: stripe(),
            start_page: 0,
            end_page: 1,
        };
        let _ = split_into_chunks(r, 0);
    }
}
