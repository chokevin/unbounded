// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! `bufferpool::Transport<R>` implementation over Mercury.
//!
//! Phase 1 of the Mercury wrapper rewrite (see `designs/`): the
//! bufferpool side of the transport boundary is the new chunk-based
//! `bulk_get(range, &[PageRef]) -> SmallVec<[PageReply; _]>` /
//! `probe(range, peer) -> bool` shape. The Mercury internals that
//! actually implement these are rewritten in a later phase; for now
//! the body is a stub that panics if hit so the crate still compiles
//! against the new trait surface.

use std::sync::Arc;

use serde::Serialize;
use smallvec::SmallVec;

use crate::bufferpool::{
    Error as PoolError, INLINE_PAGES, PageRange, PageRef, PageReply, PeerId, Req, Transport,
};
use crate::mercury::class::{Class, ClassInner};
use crate::mercury::router::PeerRouter;

/// Mercury-backed transport. One per NUMA shard. `R` is the
/// bufferpool request type; `P` selects the peer per request.
pub struct MercuryTransport<R, P>
where
    P: PeerRouter<R>,
    R: Req + Serialize + Send + Sync + 'static,
{
    #[allow(dead_code)]
    class: Arc<ClassInner>,
    #[allow(dead_code)]
    router: P,
    /// Bound at construction; used to flatten `PageRef` into a byte
    /// offset for `bulk_get`. Must match the `page_size` the
    /// embedder passed when allocating the backing it registered
    /// with `class`.
    #[allow(dead_code)]
    page_size: usize,
    _r: std::marker::PhantomData<fn() -> R>,
}

impl<R, P> MercuryTransport<R, P>
where
    P: PeerRouter<R>,
    R: Req + Serialize + Send + Sync + 'static,
{
    /// Build a transport bound to `class` and `page_size`. The
    /// caller is responsible for having already registered the
    /// pool's backing with `class` (via `Class::register_backing`).
    /// `page_size` must equal the `Backing::page_size` of the pool
    /// that will own this transport.
    pub fn new(class: &Class, router: P, page_size: usize) -> Self {
        assert!(
            page_size > 0 && page_size.is_power_of_two(),
            "MercuryTransport::new: page_size must be a positive power of two",
        );
        Self {
            class: class.inner().clone(),
            router,
            page_size,
            _r: std::marker::PhantomData,
        }
    }
}

impl<R, P> Transport<R> for MercuryTransport<R, P>
where
    P: PeerRouter<R> + Send + Sync + 'static,
    R: Req + Serialize + Send + Sync + 'static,
{
    async fn bulk_get(
        &self,
        _req: &R,
        _range: PageRange,
        _dst_pages: &[PageRef],
    ) -> Result<SmallVec<[PageReply; INLINE_PAGES]>, PoolError> {
        todo!("rewritten in phase 6")
    }

    async fn probe(&self, _req: &R, _range: PageRange, _peer: PeerId) -> Result<bool, PoolError> {
        todo!("rewritten in phase 6")
    }
}
