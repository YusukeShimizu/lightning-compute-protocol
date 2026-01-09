use std::collections::HashMap;
use std::net::SocketAddr;
use std::path::Path;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, Context, Result};
use bitcoin::constants::ChainHash;
use bitcoin::hex::{DisplayHex, FromHex};
use bitcoin::secp256k1::PublicKey;
use lightning::chain::Confirm;
use lightning::chain::Watch;
use lightning::chain::chainmonitor::ChainMonitor;
use lightning::events::{Event, EventsProvider, ReplayEvent};
use lightning::ln::channelmanager::{ChainParameters, ChannelManagerReadArgs, SimpleArcChannelManager};
use lightning::ln::peer_handler::{IgnoringMessageHandler, MessageHandler, PeerManager};
use lightning::ln::types::ChannelId;
use lightning::routing::gossip::{NetworkGraph, P2PGossipSync};
use lightning::routing::router::DefaultRouter;
use lightning::routing::scoring::{
	ProbabilisticScorer, ProbabilisticScoringDecayParameters, ProbabilisticScoringFeeParameters,
};
use lightning::routing::utxo::UtxoLookup;
use lightning::sign::EntropySource;
use lightning::sign::{KeysManager, NodeSigner, Recipient, SignerProvider};
use lightning::util::config::UserConfig;
use lightning::util::persist::{read_channel_monitors, KVStoreSync, CHANNEL_MANAGER_PERSISTENCE_KEY};
use lightning::util::ser::{ReadableArgs, Writeable};
use lightning_persister::fs_store::FilesystemStore;
use lightning_rapid_gossip_sync::RapidGossipSync;
use lightning_transaction_sync::EsploraSyncClient;
use lightning::types::payment::{PaymentHash, PaymentPreimage};
use tokio::sync::{broadcast, RwLock as TokioRwLock};

use crate::broadcaster::EsploraBroadcaster;
use crate::config::NodeConfig;
use crate::custom_message::{PeerStatusEvent, RawCustomMessageEvent, RawCustomMessageHandler};
use crate::fee_estimator::FixedFeeEstimator;
use crate::ldk_logger::TracingLogger;
use crate::seed;
use crate::store;
use crate::wallet::{OnchainWallet, EsploraClient};

type Logger = Arc<TracingLogger>;
type KvStore = Arc<FilesystemStore>;
type FeeEstimator = Arc<FixedFeeEstimator>;
type Broadcaster = Arc<EsploraBroadcaster>;

type GossipUtxoLookup = Arc<dyn UtxoLookup + Send + Sync>;

type NetworkGraphT = Arc<NetworkGraph<Logger>>;
type ScorerT = Arc<std::sync::RwLock<ProbabilisticScorer<NetworkGraphT, Logger>>>;
type RouterT = Arc<
	DefaultRouter<
		NetworkGraphT,
		Logger,
		Arc<KeysManager>,
		ScorerT,
		ProbabilisticScoringFeeParameters,
		ProbabilisticScorer<NetworkGraphT, Logger>,
	>,
>;

type MessageRouterT = Arc<lightning::onion_message::messenger::DefaultMessageRouter<NetworkGraphT, Logger, Arc<KeysManager>>>;

type TxSync = Arc<EsploraSyncClient<Logger>>;
type ChainMonitorT = ChainMonitor<
	<KeysManager as SignerProvider>::EcdsaSigner,
	TxSync,
	Broadcaster,
	FeeEstimator,
	Logger,
	KvStore,
	Arc<KeysManager>,
>;
type ChainMonitorArc = Arc<ChainMonitorT>;

pub type ChannelManager = SimpleArcChannelManager<ChainMonitorT, EsploraBroadcaster, FixedFeeEstimator, TracingLogger>;
type ChannelManagerArc = Arc<ChannelManager>;

type GossipSync = Arc<P2PGossipSync<NetworkGraphT, GossipUtxoLookup, Logger>>;
type RapidGossipSyncT = RapidGossipSync<NetworkGraphT, Logger>;

pub type PeerManagerT = PeerManager<
	lightning_net_tokio::SocketDescriptor,
	ChannelManagerArc,
	GossipSync,
	Arc<IgnoringMessageHandler>,
	Logger,
	Arc<RawCustomMessageHandler>,
	Arc<KeysManager>,
	ChainMonitorArc,
>;

const PEER_STORE_FILE: &str = "peers.json";
const INVOICE_STORE_FILE: &str = "invoices.json";
const OUTBOUND_PAYMENT_STORE_FILE: &str = "payments_outbound.json";
const NETWORK_GRAPH_FILE: &str = "network_graph.bin";

#[derive(Clone, Debug)]
pub struct FundingTxEvent {
	pub temporary_channel_id: ChannelId,
	pub funding_txid: bitcoin::Txid,
}

#[derive(Clone, Debug)]
pub struct InvoiceUpdate {
	pub payment_hash: PaymentHash,
	pub state: InvoiceState,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum InvoiceState {
	Settled,
}

#[derive(Clone, Debug)]
pub enum OutboundPaymentUpdate {
	Succeeded {
		payment_hash: PaymentHash,
		payment_preimage: PaymentPreimage,
	},
	Failed {
		payment_hash: PaymentHash,
		failure_message: String,
	},
}

#[derive(serde::Serialize, serde::Deserialize)]
struct PeerStoreDisk {
	peers: Vec<PeerStoreDiskEntry>,
}

#[derive(serde::Serialize, serde::Deserialize)]
struct PeerStoreDiskEntry {
	pubkey_hex: String,
	addr: String,
}

#[derive(Clone, Debug)]
pub struct InvoiceRecord {
	pub created_unix: u64,
	pub expiry_unix: u64,
	pub settled_unix: Option<u64>,
}

#[derive(serde::Serialize, serde::Deserialize)]
struct InvoiceStoreDisk {
	invoices: Vec<InvoiceStoreDiskEntry>,
}

#[derive(serde::Serialize, serde::Deserialize)]
struct InvoiceStoreDiskEntry {
	payment_hash_hex: String,
	created_unix: u64,
	expiry_unix: u64,
	settled_unix: Option<u64>,
}

#[derive(Clone, Debug)]
pub struct OutboundPaymentRecord {
	pub status: OutboundPaymentStatus,
	pub updated_unix: u64,
	pub failure_message: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum OutboundPaymentStatus {
	Pending,
	Succeeded,
	Failed,
}

#[derive(serde::Serialize, serde::Deserialize)]
struct OutboundPaymentStoreDisk {
	payments: Vec<OutboundPaymentStoreDiskEntry>,
}

#[derive(serde::Serialize, serde::Deserialize)]
struct OutboundPaymentStoreDiskEntry {
	payment_hash_hex: String,
	status: String,
	updated_unix: u64,
	failure_message: Option<String>,
}

pub struct Node {
	pub config: NodeConfig,
	pub node_pubkey: PublicKey,

	logger: Logger,
	pub esplora: EsploraClient,
	pub network_graph: NetworkGraphT,
	pub peer_manager: Arc<PeerManagerT>,
	pub channel_manager: ChannelManagerArc,
	pub chain_monitor: ChainMonitorArc,
	pub tx_sync: TxSync,
	pub fee_estimator: FeeEstimator,
	pub wallet: Arc<OnchainWallet>,

	pub peer_events_tx: broadcast::Sender<PeerStatusEvent>,
	pub custom_messages_tx: broadcast::Sender<RawCustomMessageEvent>,
	pub funding_tx_events_tx: broadcast::Sender<FundingTxEvent>,
	pub invoice_updates_tx: broadcast::Sender<InvoiceUpdate>,
	pub outbound_payment_updates_tx: broadcast::Sender<OutboundPaymentUpdate>,

	custom_handler: Arc<RawCustomMessageHandler>,
	kv_store: KvStore,

	peer_store: TokioRwLock<HashMap<PublicKey, String>>,
	invoice_store: TokioRwLock<HashMap<PaymentHash, InvoiceRecord>>,
	outbound_payments: TokioRwLock<HashMap<PaymentHash, OutboundPaymentRecord>>,

	chain_sync_last_unix: TokioRwLock<Option<u64>>,
	gossip_sync_last_unix: TokioRwLock<Option<u64>>,
}

impl Node {
	pub async fn start(config: NodeConfig) -> Result<Arc<Self>> {
		config.validate()?;
		std::fs::create_dir_all(&config.data_dir).context("create data_dir")?;

		let now = SystemTime::now()
			.duration_since(UNIX_EPOCH)
			.context("system clock")?;
		let current_timestamp = u32::try_from(now.as_secs().min(u64::from(u32::MAX)))
			.unwrap_or(u32::MAX);
		let starting_time_secs = now.as_secs();
		let starting_time_nanos = now.subsec_nanos();

		let seed = seed::load_or_generate_seed(&config.data_dir)?;
		let keys_manager = Arc::new(KeysManager::new(&seed, starting_time_secs, starting_time_nanos, true));
		let node_pubkey = keys_manager
			.get_node_id(Recipient::Node)
			.map_err(|_| anyhow!("KeysManager did not provide a node pubkey"))?;

		let logger: Logger = Arc::new(TracingLogger::default());

		let bitcoin_network = config.network.to_bitcoin();
		let xprv = bitcoin::bip32::Xpriv::new_master(bitcoin_network, &seed)?;
		let wallet = Arc::new(OnchainWallet::load_or_create(&config.data_dir.join("wallet"), bitcoin_network, &xprv)?);

		let ldk_dir = config.data_dir.join("ldk");
		std::fs::create_dir_all(&ldk_dir).context("create ldk_dir")?;
		let kv_store: KvStore = Arc::new(FilesystemStore::new(ldk_dir));

		let esplora = esplora_client::Builder::new(&config.esplora_base_url)
			.build_async()
			.context("create esplora client")?;
		let broadcaster: Broadcaster = Arc::new(EsploraBroadcaster::new(esplora.clone(), tokio::runtime::Handle::current()));
		let fee_estimator: FeeEstimator = Arc::new(FixedFeeEstimator::default());
		let tx_sync: TxSync = Arc::new(EsploraSyncClient::from_client(esplora.clone(), Arc::clone(&logger)));

		let chain_monitor: ChainMonitorArc = Arc::new(ChainMonitor::new(
			Some(Arc::clone(&tx_sync)),
			Arc::clone(&broadcaster),
			Arc::clone(&logger),
			Arc::clone(&fee_estimator),
			Arc::clone(&kv_store),
			Arc::clone(&keys_manager),
			keys_manager.get_peer_storage_key(),
		));

		let mut channel_monitors = read_channel_monitors(
			Arc::clone(&kv_store),
			Arc::clone(&keys_manager),
			Arc::clone(&keys_manager),
		)
		.context("read channel monitors")?;

		let network_graph: NetworkGraphT = load_network_graph(&config.data_dir, bitcoin_network, Arc::clone(&logger))
			.unwrap_or_else(|err| {
				tracing::warn!(error = %err, "failed to load network graph; starting fresh");
				Arc::new(NetworkGraph::new(bitcoin_network, Arc::clone(&logger)))
			});
		let initial_gossip_sync_unix = network_graph
			.get_last_rapid_gossip_sync_timestamp()
			.map(u64::from);
		let scorer: ScorerT = Arc::new(std::sync::RwLock::new(ProbabilisticScorer::new(
			ProbabilisticScoringDecayParameters::default(),
			Arc::clone(&network_graph),
			Arc::clone(&logger),
		)));
		let router: RouterT = Arc::new(DefaultRouter::new(
			Arc::clone(&network_graph),
			Arc::clone(&logger),
			Arc::clone(&keys_manager),
			Arc::clone(&scorer),
			ProbabilisticScoringFeeParameters::default(),
		));
		let message_router: MessageRouterT = Arc::new(lightning::onion_message::messenger::DefaultMessageRouter::new(
			Arc::clone(&network_graph),
			Arc::clone(&keys_manager),
		));

		let best_block = match (esplora.get_height().await, esplora.get_tip_hash().await) {
			(Ok(height), Ok(hash)) => lightning::chain::BestBlock::new(hash, height),
			_ => {
				let genesis = bitcoin::blockdata::constants::genesis_block(bitcoin_network);
				lightning::chain::BestBlock::new(genesis.block_hash(), 0)
			},
		};
		let chain_params = ChainParameters { network: bitcoin_network, best_block };
		let user_config = UserConfig::default();

		let channel_manager: ChannelManagerArc = match kv_store
			.read("", "", CHANNEL_MANAGER_PERSISTENCE_KEY)
		{
			Ok(bytes) => {
				let monitor_refs = channel_monitors.iter().map(|(_, m)| m).collect::<Vec<_>>();
				let read_args = ChannelManagerReadArgs::new(
					Arc::clone(&keys_manager),
					Arc::clone(&keys_manager),
					Arc::clone(&keys_manager),
					Arc::clone(&fee_estimator),
					Arc::clone(&chain_monitor),
					Arc::clone(&broadcaster),
					Arc::clone(&router),
					Arc::clone(&message_router),
					Arc::clone(&logger),
					user_config.clone(),
					monitor_refs,
				);
				let mut cursor = std::io::Cursor::new(bytes);
				let (_block_hash, channel_manager) =
					<(bitcoin::BlockHash, Arc<ChannelManager>)>::read(&mut cursor, read_args)
						.map_err(|err| anyhow!("read channel manager: {err:?}"))?;
				channel_manager
			},
			Err(err) if err.kind() == lightning::io::ErrorKind::NotFound => Arc::new(ChannelManager::new(
				Arc::clone(&fee_estimator),
				Arc::clone(&chain_monitor),
				Arc::clone(&broadcaster),
				Arc::clone(&router),
				Arc::clone(&message_router),
				Arc::clone(&logger),
				Arc::clone(&keys_manager),
				Arc::clone(&keys_manager),
				Arc::clone(&keys_manager),
				user_config.clone(),
				chain_params,
				current_timestamp,
			)),
			Err(err) => return Err(err).context("read channel manager from kv_store"),
		};

		for (_block_hash, monitor) in channel_monitors.drain(..) {
			let channel_id = monitor.channel_id();
			let _ = chain_monitor.watch_channel(channel_id, monitor);
		}

		let gossip_sync: GossipSync = Arc::new(P2PGossipSync::new(
			Arc::clone(&network_graph),
			None::<GossipUtxoLookup>,
			Arc::clone(&logger),
		));

		let (peer_events_tx, _) = broadcast::channel(1024);
		let (custom_messages_tx, _) = broadcast::channel(1024);
		let (funding_tx_events_tx, _) = broadcast::channel(1024);
		let (invoice_updates_tx, _) = broadcast::channel(1024);
		let (outbound_payment_updates_tx, _) = broadcast::channel(1024);
		let custom_handler = Arc::new(RawCustomMessageHandler::new(
			peer_events_tx.clone(),
			custom_messages_tx.clone(),
		));

		let onion_handler = Arc::new(IgnoringMessageHandler {});

		let message_handler = MessageHandler {
			chan_handler: Arc::clone(&channel_manager),
			route_handler: Arc::clone(&gossip_sync),
			onion_message_handler: onion_handler,
			custom_message_handler: Arc::clone(&custom_handler),
			send_only_message_handler: Arc::clone(&chain_monitor),
		};

		let peer_manager = Arc::new(PeerManager::new(
			message_handler,
			current_timestamp,
			&keys_manager.get_secure_random_bytes(),
			Arc::clone(&logger),
			Arc::clone(&keys_manager),
		));

		let peer_store = TokioRwLock::new(load_peer_store(&config.data_dir)?);
		let invoice_store = TokioRwLock::new(load_invoice_store(&config.data_dir)?);
		let outbound_payments = TokioRwLock::new(load_outbound_payments(&config.data_dir)?);

		let node = Arc::new(Self {
			config,
			node_pubkey,
			logger: Arc::clone(&logger),
			esplora: esplora.clone(),
			network_graph: Arc::clone(&network_graph),
			peer_manager,
			channel_manager,
			chain_monitor,
			tx_sync,
			fee_estimator,
			wallet,
			peer_events_tx,
			custom_messages_tx,
			funding_tx_events_tx,
			invoice_updates_tx,
			outbound_payment_updates_tx,
			custom_handler,
			kv_store,
			peer_store,
			invoice_store,
			outbound_payments,
			chain_sync_last_unix: TokioRwLock::new(None),
			gossip_sync_last_unix: TokioRwLock::new(initial_gossip_sync_unix),
		});

		node.spawn_p2p_listeners().await?;
		node.spawn_ldk_background_tasks();
		node.spawn_chain_sync_task();
		node.spawn_rgs_sync_task();
		node.spawn_peer_reconnect_task();

		Ok(node)
	}

	async fn spawn_p2p_listeners(self: &Arc<Self>) -> Result<()> {
		for listen_addr in &self.config.p2p_listen_addrs {
			let listener = tokio::net::TcpListener::bind(listen_addr)
				.await
				.with_context(|| format!("bind p2p listener {listen_addr}"))?;
			let peer_manager = Arc::clone(&self.peer_manager);
			tokio::spawn(async move {
				loop {
					let (stream, _) = match listener.accept().await {
						Ok(v) => v,
						Err(err) => {
							tracing::warn!(error = %err, "p2p accept failed");
							continue;
						},
					};
					let std_stream = match stream.into_std() {
						Ok(s) => s,
						Err(err) => {
							tracing::warn!(error = %err, "failed to convert p2p stream to std");
							continue;
						},
					};
					tokio::spawn(lightning_net_tokio::setup_inbound(
						Arc::clone(&peer_manager),
						std_stream,
					));
				}
			});
		}
		Ok(())
	}

	fn spawn_ldk_background_tasks(self: &Arc<Self>) {
		let node = Arc::clone(self);
		tokio::spawn(async move {
			let mut last_tick = SystemTime::now();
			loop {
				let handler = |event: Event| node.handle_ldk_event(event);
				node.channel_manager.process_pending_events(&handler);
				node.peer_manager.process_events();

				if node.channel_manager.get_and_clear_needs_persistence() {
					if let Err(err) = persist_channel_manager(&node.kv_store, &node.channel_manager) {
						tracing::warn!(error = %err, "failed to persist channel manager");
					}
				}

				let now = SystemTime::now();
				if now
					.duration_since(last_tick)
					.unwrap_or(Duration::from_secs(0))
					.as_secs()
					>= 1
				{
					node.channel_manager.timer_tick_occurred();
					node.peer_manager.timer_tick_occurred();
					last_tick = now;
				}

				tokio::time::sleep(Duration::from_millis(250)).await;
			}
		});
	}

	fn spawn_chain_sync_task(self: &Arc<Self>) {
		let node = Arc::clone(self);
		tokio::spawn(async move {
			loop {
				if let Err(err) = node.sync_chain_once().await {
					tracing::warn!(error = %err, "chain sync failed");
				}
				tokio::time::sleep(Duration::from_secs(5)).await;
			}
		});
	}

	fn spawn_rgs_sync_task(self: &Arc<Self>) {
		let Some(base_url) = self.config.rgs_base_url.clone().filter(|s| !s.trim().is_empty()) else {
			return;
		};
		let base_url = base_url.trim_end_matches('/').to_string();
		let client = match reqwest::Client::builder()
			.user_agent(self.config.ldk_user_agent())
			.timeout(Duration::from_secs(120))
			.build()
		{
			Ok(c) => c,
			Err(err) => {
				tracing::warn!(error = %err, "failed to build RGS http client; skipping RGS");
				return;
			},
		};

		let node = Arc::clone(self);
		let rapid_sync = Arc::new(RapidGossipSync::new(
			Arc::clone(&node.network_graph),
			Arc::clone(&node.logger),
		));

		tokio::spawn(async move {
			let interval = Duration::from_secs(60 * 60);
			loop {
				if let Err(err) = node.sync_rgs_once(&rapid_sync, &client, &base_url).await {
					tracing::warn!(error = %err, "RGS sync failed");
				}
				tokio::time::sleep(interval).await;
			}
		});
	}

	async fn sync_rgs_once(
		&self,
		rapid_sync: &RapidGossipSyncT,
		client: &reqwest::Client,
		base_url: &str,
	) -> Result<()> {
		let last_seen = self
			.network_graph
			.get_last_rapid_gossip_sync_timestamp()
			.unwrap_or(0);
		let url = format!("{base_url}/snapshot/{last_seen}");
		let resp = client.get(&url).send().await.context("fetch RGS snapshot")?;
		let status = resp.status();
		if !status.is_success() {
			return Err(anyhow!("RGS snapshot request failed: {status} (url={url})"));
		}
		let body = resp.bytes().await.context("read RGS response body")?;
		let new_ts = rapid_sync
			.update_network_graph(&body)
			.map_err(|err| anyhow!("apply RGS data failed: {err:?}"))?;
		persist_network_graph(&self.config.data_dir, &self.network_graph)
			.context("persist network graph")?;
		*self.gossip_sync_last_unix.write().await = Some(now_unix());
		tracing::info!(last_seen, new_ts, bytes = body.len(), "RGS sync complete");
		Ok(())
	}

	fn spawn_peer_reconnect_task(self: &Arc<Self>) {
		let node = Arc::clone(self);
		tokio::spawn(async move {
			loop {
				let snapshot = { node.peer_store.read().await.clone() };
				for (peer_pubkey, addr) in snapshot {
					if node.peer_manager.peer_by_node_id(&peer_pubkey).is_some() {
						continue;
					}
					let _ = node.connect_peer(peer_pubkey, &addr).await;
				}
				tokio::time::sleep(Duration::from_secs(5)).await;
			}
		});
	}

	fn handle_ldk_event(self: &Arc<Self>, event: Event) -> Result<(), ReplayEvent> {
		match event {
			Event::FundingGenerationReady {
				temporary_channel_id,
				counterparty_node_id,
				channel_value_satoshis,
				output_script,
				..
			} => {
				let node = Arc::clone(self);
				tokio::spawn(async move {
					if let Err(err) = node
						.handle_funding_generation(
							temporary_channel_id,
							counterparty_node_id,
							channel_value_satoshis,
							output_script,
						)
						.await
					{
						tracing::warn!(
							error = %err,
							channel_id = %temporary_channel_id,
							peer = %counterparty_node_id,
							"funding tx generation failed"
						);
					}
				});
			},
			Event::PaymentClaimable { payment_hash, purpose, .. } => {
				if let Some(preimage) = purpose.preimage() {
					self.channel_manager.claim_funds(preimage);
					self.peer_manager.process_events();
				} else {
					tracing::warn!(%payment_hash, "claimable payment missing preimage; ignoring");
				}
			},
			Event::PaymentClaimed { payment_hash, .. } => {
				let _ = self
					.invoice_updates_tx
					.send(InvoiceUpdate { payment_hash, state: InvoiceState::Settled });
				let node = Arc::clone(self);
				tokio::spawn(async move {
					if let Err(err) = node.mark_invoice_settled(payment_hash).await {
						tracing::warn!(error = %err, %payment_hash, "failed to mark invoice settled");
					}
				});
			},
			Event::PaymentSent { payment_hash, payment_preimage, .. } => {
				let _ = self.outbound_payment_updates_tx.send(OutboundPaymentUpdate::Succeeded {
					payment_hash,
					payment_preimage,
				});
				let node = Arc::clone(self);
				tokio::spawn(async move {
					if let Err(err) = node.mark_outbound_payment_succeeded(payment_hash).await {
						tracing::warn!(error = %err, %payment_hash, "failed to mark outbound payment succeeded");
					}
				});
			},
			Event::PaymentFailed { payment_hash, reason, .. } => {
				let Some(payment_hash) = payment_hash else {
					return Ok(());
				};
				let failure_message = match reason {
					Some(r) => format!("{r:?}"),
					None => "unknown".to_string(),
				};
				let _ = self.outbound_payment_updates_tx.send(OutboundPaymentUpdate::Failed {
					payment_hash,
					failure_message: failure_message.clone(),
				});
				let node = Arc::clone(self);
				tokio::spawn(async move {
					if let Err(err) = node.mark_outbound_payment_failed(payment_hash, failure_message).await {
						tracing::warn!(error = %err, %payment_hash, "failed to mark outbound payment failed");
					}
				});
			},
			Event::ChannelPending { channel_id, counterparty_node_id, .. } => {
				tracing::info!(%channel_id, peer = %counterparty_node_id, "channel pending");
			},
			Event::ChannelReady { channel_id, counterparty_node_id, .. } => {
				tracing::info!(%channel_id, peer = %counterparty_node_id, "channel ready");
			},
			Event::ChannelClosed { channel_id, counterparty_node_id, reason, .. } => {
				tracing::info!(%channel_id, peer = ?counterparty_node_id, ?reason, "channel closed");
			},
			_ => {
				tracing::debug!(event = ?event, "ldk event");
			},
		}
		Ok(())
	}

	async fn sync_chain_once(&self) -> Result<()> {
		self.wallet.sync(&self.esplora).await?;
		let confirmables = vec![
			&*self.channel_manager as &(dyn Confirm + Sync + Send),
			&*self.chain_monitor as &(dyn Confirm + Sync + Send),
		];
		self.tx_sync.sync(confirmables).await?;
		*self.chain_sync_last_unix.write().await = Some(now_unix());
		Ok(())
	}

	async fn handle_funding_generation(
		self: &Arc<Self>,
		temporary_channel_id: ChannelId,
		counterparty_node_id: PublicKey,
		channel_value_satoshis: u64,
		output_script: bitcoin::ScriptBuf,
	) -> Result<()> {
		let fee_rate_sat_per_vbyte =
			sat_per_kw_to_sat_per_vbyte(self.fee_estimator.sat_per_1000_weight);

		for attempt in 0..5u32 {
			self.wallet.sync(&self.esplora).await?;
			let tx = match self
				.wallet
				.create_tx_to_script(
					output_script.clone(),
					channel_value_satoshis,
					fee_rate_sat_per_vbyte,
					false,
				)
				.await
			{
				Ok(tx) => tx,
				Err(err) => {
					if attempt >= 4 {
						return Err(err).context("build funding tx");
					}
					tokio::time::sleep(Duration::from_millis(200)).await;
					continue;
				},
			};

			match self.channel_manager.funding_transaction_generated(
				temporary_channel_id,
				counterparty_node_id,
				tx.clone(),
			) {
				Ok(()) => {
					self.peer_manager.process_events();
					let _ = self.funding_tx_events_tx.send(FundingTxEvent {
						temporary_channel_id,
						funding_txid: tx.compute_txid(),
					});
					return Ok(());
				},
				Err(err) => {
					if attempt >= 4 {
						return Err(anyhow!("funding_transaction_generated failed: {err:?}"));
					}
					tokio::time::sleep(Duration::from_millis(200)).await;
					continue;
				},
			}
		}

		Err(anyhow!("funding generation failed after retries"))
	}

	pub fn open_channel(
		&self,
		peer_pubkey: PublicKey,
		local_funding_amount_sat: u64,
		announce: bool,
	) -> Result<ChannelId> {
		let user_channel_id = rand::random::<u128>();
		let mut config = UserConfig::default();
		config.channel_handshake_config.announce_for_forwarding = announce;
		let channel_id = self.channel_manager.create_channel(
			peer_pubkey,
			local_funding_amount_sat,
			0,
			user_channel_id,
			None,
			Some(config),
		)
		.map_err(|err| anyhow!("create_channel failed: {err:?}"))?;
		self.peer_manager.process_events();
		Ok(channel_id)
	}

	pub async fn record_invoice_created(
		&self,
		payment_hash: PaymentHash,
		expiry_unix: u64,
	) -> Result<()> {
		let created_unix = now_unix();
		{
			let mut store = self.invoice_store.write().await;
			store.insert(
				payment_hash,
				InvoiceRecord { created_unix, expiry_unix, settled_unix: None },
			);
			persist_invoice_store(&self.config.data_dir, &store)?;
		}
		Ok(())
	}

	async fn mark_invoice_settled(&self, payment_hash: PaymentHash) -> Result<()> {
		{
			let mut store = self.invoice_store.write().await;
			let entry = store.entry(payment_hash).or_insert_with(|| InvoiceRecord {
				created_unix: now_unix(),
				expiry_unix: now_unix(),
				settled_unix: None,
			});
			entry.settled_unix = Some(now_unix());
			persist_invoice_store(&self.config.data_dir, &store)?;
		}
		Ok(())
	}

	pub async fn invoice_record(&self, payment_hash: PaymentHash) -> Option<InvoiceRecord> {
		let store = self.invoice_store.read().await;
		store.get(&payment_hash).cloned()
	}

	pub fn subscribe_invoice_updates(&self) -> broadcast::Receiver<InvoiceUpdate> {
		self.invoice_updates_tx.subscribe()
	}

	pub async fn record_outbound_payment_started(&self, payment_hash: PaymentHash) -> Result<()> {
		{
			let mut store = self.outbound_payments.write().await;
			store.insert(
				payment_hash,
				OutboundPaymentRecord {
					status: OutboundPaymentStatus::Pending,
					updated_unix: now_unix(),
					failure_message: None,
				},
			);
			persist_outbound_payments(&self.config.data_dir, &store)?;
		}
		Ok(())
	}

	async fn mark_outbound_payment_succeeded(&self, payment_hash: PaymentHash) -> Result<()> {
		{
			let mut store = self.outbound_payments.write().await;
			store.insert(
				payment_hash,
				OutboundPaymentRecord {
					status: OutboundPaymentStatus::Succeeded,
					updated_unix: now_unix(),
					failure_message: None,
				},
			);
			persist_outbound_payments(&self.config.data_dir, &store)?;
		}
		Ok(())
	}

	async fn mark_outbound_payment_failed(
		&self,
		payment_hash: PaymentHash,
		failure_message: String,
	) -> Result<()> {
		{
			let mut store = self.outbound_payments.write().await;
			store.insert(
				payment_hash,
				OutboundPaymentRecord {
					status: OutboundPaymentStatus::Failed,
					updated_unix: now_unix(),
					failure_message: Some(failure_message),
				},
			);
			persist_outbound_payments(&self.config.data_dir, &store)?;
		}
		Ok(())
	}

	pub async fn record_outbound_payment_failed(
		&self,
		payment_hash: PaymentHash,
		failure_message: String,
	) -> Result<()> {
		self.mark_outbound_payment_failed(payment_hash, failure_message).await
	}

	pub async fn outbound_payment_record(
		&self,
		payment_hash: PaymentHash,
	) -> Option<OutboundPaymentRecord> {
		let store = self.outbound_payments.read().await;
		store.get(&payment_hash).cloned()
	}

	pub fn subscribe_outbound_payment_updates(&self) -> broadcast::Receiver<OutboundPaymentUpdate> {
		self.outbound_payment_updates_tx.subscribe()
	}

	pub fn subscribe_funding_tx_events(&self) -> broadcast::Receiver<FundingTxEvent> {
		self.funding_tx_events_tx.subscribe()
	}

	pub async fn chain_sync_last_unix(&self) -> Option<u64> {
		*self.chain_sync_last_unix.read().await
	}

	pub async fn gossip_sync_last_unix(&self) -> Option<u64> {
		*self.gossip_sync_last_unix.read().await
	}

	pub async fn connect_peer(&self, peer_pubkey: PublicKey, addr: &str) -> Result<bool> {
		if self.peer_manager.peer_by_node_id(&peer_pubkey).is_some() {
			return Ok(true);
		}

		let socket_addr = resolve_addr(addr).await?;
		if let Some(fut) =
			lightning_net_tokio::connect_outbound(Arc::clone(&self.peer_manager), peer_pubkey, socket_addr).await
		{
			tokio::spawn(fut);
		} else {
			return Err(anyhow!("failed to connect to {addr}"));
		}

		{
			let mut peers = self.peer_store.write().await;
			peers.insert(peer_pubkey, addr.to_string());
			persist_peer_store(&self.config.data_dir, &peers)?;
		}

		Ok(false)
	}

	pub async fn list_peers(&self) -> Vec<(PublicKey, String, bool)> {
		let peers = self.peer_store.read().await.clone();
		let mut out = Vec::new();
		for (pubkey, addr) in peers {
			let connected = self.peer_manager.peer_by_node_id(&pubkey).is_some();
			out.push((pubkey, addr, connected));
		}
		for peer in self.peer_manager.list_peers() {
			let pubkey = peer.counterparty_node_id;
			if out.iter().any(|(p, _, _)| *p == pubkey) {
				continue;
			}
			let addr = peer
				.socket_address
				.as_ref()
				.map(|a| a.to_string())
				.unwrap_or_default();
			out.push((pubkey, addr, true));
		}
		out
	}

	pub fn queue_custom_message(&self, peer_pubkey: PublicKey, msg_type: u16, data: Vec<u8>) {
		self.custom_handler.queue_outbound(peer_pubkey, msg_type, data);
		self.peer_manager.process_events();
	}

	pub fn subscribe_peer_events(&self) -> broadcast::Receiver<PeerStatusEvent> {
		self.peer_events_tx.subscribe()
	}

	pub fn subscribe_custom_messages(&self) -> broadcast::Receiver<RawCustomMessageEvent> {
		self.custom_messages_tx.subscribe()
	}
}

fn load_network_graph(data_dir: &Path, network: bitcoin::Network, logger: Logger) -> Result<NetworkGraphT> {
	let path = data_dir.join("ldk").join(NETWORK_GRAPH_FILE);
	let Some(bytes) = store::read_optional(&path)? else {
		return Ok(Arc::new(NetworkGraph::new(network, logger)));
	};
	let mut cursor = std::io::Cursor::new(bytes);
	let graph = NetworkGraph::read(&mut cursor, logger).map_err(|err| anyhow!("decode network graph: {err:?}"))?;
	if graph.get_chain_hash() != ChainHash::using_genesis_block(network) {
		return Err(anyhow!("network graph chain hash mismatch; refusing to load {}", path.display()));
	}
	Ok(Arc::new(graph))
}

fn persist_network_graph(data_dir: &Path, graph: &NetworkGraphT) -> Result<()> {
	let path = data_dir.join("ldk").join(NETWORK_GRAPH_FILE);
	store::write_atomic(path, &graph.encode()).context("write network graph")?;
	Ok(())
}

fn persist_channel_manager(store: &FilesystemStore, channel_manager: &ChannelManager) -> Result<()> {
	store
		.write("", "", CHANNEL_MANAGER_PERSISTENCE_KEY, channel_manager.encode())
		.context("kv_store write channel manager")?;
	Ok(())
}

fn load_peer_store(data_dir: &Path) -> Result<HashMap<PublicKey, String>> {
	let path = data_dir.join(PEER_STORE_FILE);
	let Some(bytes) = store::read_optional(&path)? else {
		return Ok(HashMap::new());
	};
	let disk: PeerStoreDisk = serde_json::from_slice(&bytes).context("parse peer store")?;
	let mut peers = HashMap::new();
	for peer in disk.peers {
		let pubkey_bytes = Vec::<u8>::from_hex(&peer.pubkey_hex).context("decode pubkey hex")?;
		let pubkey = PublicKey::from_slice(&pubkey_bytes).context("parse pubkey")?;
		peers.insert(pubkey, peer.addr);
	}
	Ok(peers)
}

fn persist_peer_store(data_dir: &Path, peers: &HashMap<PublicKey, String>) -> Result<()> {
	let disk = PeerStoreDisk {
		peers: peers
			.iter()
			.map(|(pubkey, addr)| PeerStoreDiskEntry {
				pubkey_hex: pubkey.serialize().to_vec().to_lower_hex_string(),
				addr: addr.clone(),
			})
			.collect(),
	};
	let bytes = serde_json::to_vec_pretty(&disk).context("serialize peer store")?;
	store::write_atomic(data_dir.join(PEER_STORE_FILE), &bytes).context("persist peer store")?;
	Ok(())
}

fn load_invoice_store(data_dir: &Path) -> Result<HashMap<PaymentHash, InvoiceRecord>> {
	let path = data_dir.join(INVOICE_STORE_FILE);
	let Some(bytes) = store::read_optional(&path)? else {
		return Ok(HashMap::new());
	};
	let disk: InvoiceStoreDisk = serde_json::from_slice(&bytes).context("parse invoice store")?;
	let mut out = HashMap::new();
	for inv in disk.invoices {
		let payment_hash = <[u8; 32]>::from_hex(&inv.payment_hash_hex).context("decode payment hash hex")?;
		out.insert(
			PaymentHash(payment_hash),
			InvoiceRecord {
				created_unix: inv.created_unix,
				expiry_unix: inv.expiry_unix,
				settled_unix: inv.settled_unix,
			},
		);
	}
	Ok(out)
}

fn persist_invoice_store(data_dir: &Path, invoices: &HashMap<PaymentHash, InvoiceRecord>) -> Result<()> {
	let disk = InvoiceStoreDisk {
		invoices: invoices
			.iter()
			.map(|(hash, rec)| InvoiceStoreDiskEntry {
				payment_hash_hex: hash.0.to_lower_hex_string(),
				created_unix: rec.created_unix,
				expiry_unix: rec.expiry_unix,
				settled_unix: rec.settled_unix,
			})
			.collect(),
	};
	let bytes = serde_json::to_vec_pretty(&disk).context("serialize invoice store")?;
	store::write_atomic(data_dir.join(INVOICE_STORE_FILE), &bytes).context("persist invoice store")?;
	Ok(())
}

fn load_outbound_payments(data_dir: &Path) -> Result<HashMap<PaymentHash, OutboundPaymentRecord>> {
	let path = data_dir.join(OUTBOUND_PAYMENT_STORE_FILE);
	let Some(bytes) = store::read_optional(&path)? else {
		return Ok(HashMap::new());
	};
	let disk: OutboundPaymentStoreDisk =
		serde_json::from_slice(&bytes).context("parse outbound payment store")?;
	let mut out = HashMap::new();
	for payment in disk.payments {
		let payment_hash =
			<[u8; 32]>::from_hex(&payment.payment_hash_hex).context("decode payment hash hex")?;
		let status = match payment.status.as_str() {
			"pending" => OutboundPaymentStatus::Pending,
			"succeeded" => OutboundPaymentStatus::Succeeded,
			"failed" => OutboundPaymentStatus::Failed,
			_ => continue,
		};
		out.insert(
			PaymentHash(payment_hash),
			OutboundPaymentRecord {
				status,
				updated_unix: payment.updated_unix,
				failure_message: payment.failure_message,
			},
		);
	}
	Ok(out)
}

fn persist_outbound_payments(
	data_dir: &Path,
	payments: &HashMap<PaymentHash, OutboundPaymentRecord>,
) -> Result<()> {
	let disk = OutboundPaymentStoreDisk {
		payments: payments
			.iter()
			.map(|(hash, rec)| OutboundPaymentStoreDiskEntry {
				payment_hash_hex: hash.0.to_lower_hex_string(),
				status: match rec.status {
					OutboundPaymentStatus::Pending => "pending",
					OutboundPaymentStatus::Succeeded => "succeeded",
					OutboundPaymentStatus::Failed => "failed",
				}
				.to_string(),
				updated_unix: rec.updated_unix,
				failure_message: rec.failure_message.clone(),
			})
			.collect(),
	};
	let bytes = serde_json::to_vec_pretty(&disk).context("serialize outbound payment store")?;
	store::write_atomic(data_dir.join(OUTBOUND_PAYMENT_STORE_FILE), &bytes)
		.context("persist outbound payment store")?;
	Ok(())
}

async fn resolve_addr(addr: &str) -> Result<SocketAddr> {
	if let Ok(sock) = addr.parse::<SocketAddr>() {
		return Ok(sock);
	}
	let mut addrs = tokio::net::lookup_host(addr)
		.await
		.with_context(|| format!("lookup_host {addr}"))?;
	addrs
		.next()
		.ok_or_else(|| anyhow!("could not resolve {addr}"))
}

fn now_unix() -> u64 {
	SystemTime::now()
		.duration_since(UNIX_EPOCH)
		.map(|d| d.as_secs())
		.unwrap_or_default()
}

fn sat_per_kw_to_sat_per_vbyte(sat_per_1000_weight: u32) -> u64 {
	// 1000 weight units == 250 vbytes
	let sat = u64::from(sat_per_1000_weight);
	let mut out = sat / 250;
	if sat % 250 != 0 {
		out += 1;
	}
	out.max(1)
}
