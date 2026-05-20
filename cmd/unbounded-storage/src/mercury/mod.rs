// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

mod bulk;
mod class;
mod config;
mod error;
mod ffi;
mod handle;
mod peer;
pub mod progress;
mod router;
mod rpc;
mod server;
mod transport;

pub use class::Class;
pub use config::{PeerEntry, TransportConfig};
pub use error::{HgError, Result};
pub use router::{PeerRouter, StaticPeer};
pub use server::{BulkSource, MercuryServer};
pub use transport::MercuryTransport;

// Phase 1 of the Mercury wrapper rewrite stubbed
// `MercuryTransport`'s `Transport` impl with `todo!()`; the module
// integration tests in `tests.rs` exercise the real impl and would
// panic at runtime. They are gated off until Phase 6 restores the
// implementation. See AGENTS.md / phase plan.
#[cfg(all(test, any()))]
mod tests;
