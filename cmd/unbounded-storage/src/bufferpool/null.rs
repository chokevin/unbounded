// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Placeholder `BlockStore` and `Transport` for embedders that
//! have no local cache tier or peer transport wired up yet. Every
//! `BlockStore::read_page` reports a miss, so the pool always falls
//! through to `Transport::bulk_get`; `write_page` is a no-op. The
//! `NullTransport` reports every requested page as populated
//! without ever touching the destination buffers, and `probe`
//! always returns `Ok(true)`.
//!
//! These exist so the binary can construct a `Pool` per shard
//! before production implementations land. Replace with real
//! io_uring / NVMe-backed and Mercury-backed impls as soon as
//! they're available.

use smallvec::SmallVec;

use crate::bufferpool::traits::{BlockStore, Req, Transport};
use crate::bufferpool::types::{
    Error, INLINE_PAGES, PageRange, PageRef, PageReply, PeerId, StripeKey,
};

#[derive(Default)]
pub struct NullBlockStore;

impl NullBlockStore {
    pub fn new() -> Self {
        Self
    }
}

impl BlockStore for NullBlockStore {
    fn register_pages(
        &self,
        _base: *mut u8,
        _page_size: usize,
        _page_count: usize,
    ) -> Result<(), Error> {
        Ok(())
    }

    async fn read_page(
        &self,
        _key: StripeKey,
        _stripe_off: u64,
        _dst: PageRef,
    ) -> Result<bool, Error> {
        // Always-miss. Pool falls through to `Transport::bulk_get`.
        Ok(false)
    }

    async fn write_page(
        &self,
        _key: StripeKey,
        _stripe_off: u64,
        _page: PageRef,
    ) -> Result<(), Error> {
        // Drop the tee silently. A real blockstore will persist.
        Ok(())
    }
}

/// Placeholder `Transport` that reports every requested page as
/// populated without touching the destination buffers. Useful as a
/// stand-in before a real peer transport (e.g. Mercury) is wired
/// up. Returns one [`PageReply`] per requested page; `probe`
/// always reports presence.
pub struct NullTransport {
    page_size: u32,
}

impl NullTransport {
    pub fn new(page_size: u32) -> Self {
        Self { page_size }
    }
}

impl<R> Transport<R> for NullTransport
where
    R: Req + Send + Sync + 'static,
{
    async fn bulk_get(
        &self,
        _req: &R,
        range: PageRange,
        dst_pages: &[PageRef],
    ) -> Result<SmallVec<[PageReply; INLINE_PAGES]>, Error> {
        debug_assert_eq!(
            dst_pages.len(),
            range.len() as usize,
            "NullTransport: dst_pages length must match range length",
        );
        let mut out: SmallVec<[PageReply; INLINE_PAGES]> = SmallVec::new();
        for (i, dst) in dst_pages.iter().enumerate() {
            out.push(PageReply {
                page_idx: dst.page_idx,
                byte_len: self.page_size,
            });
            let _ = i;
        }
        Ok(out)
    }

    async fn probe(&self, _req: &R, _range: PageRange, _peer: PeerId) -> Result<bool, Error> {
        Ok(true)
    }
}

#[cfg(test)]
mod tests {
    use std::future::Future;
    use std::pin::pin;
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    use super::*;

    fn noop_waker() -> Waker {
        fn raw() -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
        // SAFETY: vtable never dereferences the data pointer.
        unsafe { Waker::from_raw(raw()) }
    }

    fn block_on<F: Future>(f: F) -> F::Output {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut fut = pin!(f);
        loop {
            if let Poll::Ready(v) = fut.as_mut().poll(&mut cx) {
                return v;
            }
        }
    }

    #[test]
    fn read_always_misses_write_is_noop() {
        let s = NullBlockStore::new();
        let key = StripeKey([0; 32]);
        let dst = PageRef {
            page_idx: 0,
            offset: 0,
            len: 0,
        };
        assert!(matches!(block_on(s.read_page(key, 0, dst)), Ok(false)));
        assert!(block_on(s.write_page(key, 0, dst)).is_ok());
    }

    #[test]
    fn register_pages_accepts_anything() {
        let s = NullBlockStore::new();
        assert!(s.register_pages(std::ptr::null_mut(), 4096, 0).is_ok());
    }

    #[derive(Clone)]
    struct K;
    impl Req for K {
        fn key(&self) -> StripeKey {
            StripeKey([0; 32])
        }
    }

    #[test]
    fn transport_bulk_get_returns_one_reply_per_page() {
        let t = NullTransport::new(4096);
        let range = PageRange {
            stripe: StripeKey([1; 32]),
            start_page: 10,
            end_page: 13,
        };
        let dst = vec![
            PageRef {
                page_idx: 100,
                offset: 0,
                len: 4096,
            },
            PageRef {
                page_idx: 101,
                offset: 0,
                len: 4096,
            },
            PageRef {
                page_idx: 102,
                offset: 0,
                len: 4096,
            },
        ];
        let replies = block_on(Transport::<K>::bulk_get(&t, &K, range, &dst)).unwrap();
        assert_eq!(replies.len(), 3);
        assert_eq!(replies[0].page_idx, 100);
        assert_eq!(replies[0].byte_len, 4096);
    }

    #[test]
    fn transport_probe_reports_presence() {
        let t = NullTransport::new(4096);
        let range = PageRange {
            stripe: StripeKey([1; 32]),
            start_page: 0,
            end_page: 1,
        };
        assert!(block_on(Transport::<K>::probe(&t, &K, range, PeerId(0))).unwrap());
    }
}
