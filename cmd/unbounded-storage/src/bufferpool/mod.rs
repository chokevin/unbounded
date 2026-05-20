// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

mod free_list;
mod group;
mod inflight;
mod null;
pub mod pool;
pub mod stream;
mod traits;
mod types;

#[cfg(test)]
mod tests;

pub use group::{PoolGroup, ShardDescriptor, ShardRouter};
pub use null::{NullBlockStore, NullTransport};
pub use pool::Pool;
pub use stream::{PageGuard, ReadStream};
pub use traits::{BlockStore, BufferPool, Req, Transport};
pub use types::{
    Backing, Error, INLINE_PAGES, NodeId, PAGES_PER_CHUNK, PageRange, PageRef, PageReply, PeerId,
    PoolConfig, StripeKey, TraceCtx,
};
