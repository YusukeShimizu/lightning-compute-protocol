use std::sync::Arc;

use esplora_client::r#async::DefaultSleeper;
use lightning::chain::chaininterface::BroadcasterInterface;

#[derive(Clone)]
pub struct EsploraBroadcaster {
	client: esplora_client::AsyncClient<DefaultSleeper>,
	runtime: tokio::runtime::Handle,
}

impl EsploraBroadcaster {
	pub fn new(client: esplora_client::AsyncClient<DefaultSleeper>, runtime: tokio::runtime::Handle) -> Self {
		Self { client, runtime }
	}
}

impl BroadcasterInterface for EsploraBroadcaster {
	fn broadcast_transactions(&self, txs: &[&bitcoin::Transaction]) {
		let client = self.client.clone();
		let txs: Vec<bitcoin::Transaction> = txs.iter().map(|tx| (*tx).clone()).collect();
		self.runtime.spawn(async move {
			for tx in txs {
				if let Err(err) = client.broadcast(&tx).await {
					tracing::warn!(error = %err, txid = %tx.compute_txid(), "esplora broadcast failed");
				}
			}
		});
	}
}

pub type DynBroadcaster = Arc<EsploraBroadcaster>;

