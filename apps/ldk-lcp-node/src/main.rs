use clap::Parser;
use std::collections::HashSet;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use bitcoin::hashes::sha256;
use bitcoin::hashes::Hash as _;
use lightning::ln::channelmanager::{PaymentId, RecipientOnionFields, Retry};
use lightning::ln::types::ChannelId;
use lightning::routing::router::{PaymentParameters, RouteParameters};
use lightning::types::payment::PaymentHash;
use lightning_invoice::{Bolt11Invoice, Bolt11InvoiceDescription, Bolt11InvoiceDescriptionRef, Sha256 as InvoiceSha256};
use tonic::{transport::Server, Request, Response, Status};
use tonic::service::Interceptor;
use tokio::sync::{broadcast, mpsc};
use tokio_stream::wrappers::ReceiverStream;

mod broadcaster;
mod config;
mod custom_message;
mod fee_estimator;
mod ldk_logger;
mod node;
mod seed;
mod store;
mod wallet;

mod proto {
	pub mod lnnode {
		pub mod v1 {
			tonic::include_proto!("lnnode.v1");
		}
	}
}

use proto::lnnode::v1::lightning_node_service_server::{
	LightningNodeService, LightningNodeServiceServer,
};

use crate::config::{parse_pubkey33, parse_socket_addr, BitcoinNetwork, NodeConfig};

#[derive(Debug, Parser)]
#[command(name = "ldk-lcp-node", version)]
struct Args {
	#[arg(long, default_value = "127.0.0.1:10010", env = "LCP_LDK_NODE_GRPC_ADDR")]
	grpc_addr: String,

	#[arg(long, env = "LCP_LDK_NODE_DATA_DIR")]
	data_dir: PathBuf,

	#[arg(long, default_value = "regtest", env = "LCP_LDK_NODE_NETWORK")]
	network: String,

	#[arg(
		long = "p2p-listen",
		env = "LCP_LDK_NODE_P2P_LISTEN",
		value_delimiter = ',',
		default_value = "0.0.0.0:9736"
	)]
	p2p_listen_addrs: Vec<String>,

	#[arg(long, env = "LCP_LDK_NODE_ESPLORA_BASE_URL")]
	esplora_base_url: String,

	#[arg(long, env = "LCP_LDK_NODE_RGS_BASE_URL")]
	rgs_base_url: Option<String>,

	#[arg(long, env = "LCP_LDK_NODE_RPC_AUTH_TOKEN")]
	rpc_auth_token: Option<String>,
}

#[derive(Clone)]
struct LightningNodeServiceImpl {
	node: Arc<node::Node>,
}

#[derive(Clone)]
struct AuthInterceptor {
	token: Option<String>,
}

impl Interceptor for AuthInterceptor {
	fn call(&mut self, req: Request<()>) -> Result<Request<()>, Status> {
		let Some(token) = self.token.as_deref().filter(|t| !t.is_empty()) else {
			return Ok(req);
		};
		let Some(auth) = req.metadata().get("authorization") else {
			return Err(Status::unauthenticated("missing authorization token"));
		};
		let Ok(auth) = auth.to_str() else {
			return Err(Status::unauthenticated("invalid authorization token"));
		};
		if auth == token {
			return Ok(req);
		}
		if let Some(bearer) = auth.strip_prefix("Bearer ") {
			if bearer == token {
				return Ok(req);
			}
		}
		Err(Status::unauthenticated("invalid authorization token"))
	}
}

fn parse_payment_hash(bytes: &[u8]) -> Result<PaymentHash, Status> {
	if bytes.len() != 32 {
		return Err(Status::invalid_argument("payment_hash must be 32 bytes"));
	}
	let mut out = [0u8; 32];
	out.copy_from_slice(bytes);
	Ok(PaymentHash(out))
}

fn parse_channel_id(bytes: &[u8]) -> Result<ChannelId, Status> {
	if bytes.len() != 32 {
		return Err(Status::invalid_argument("channel_id must be 32 bytes"));
	}
	let mut out = [0u8; 32];
	out.copy_from_slice(bytes);
	Ok(ChannelId::from_bytes(out))
}

fn parse_custom_msg_type(msg_type: u32) -> Result<u16, Status> {
	let Ok(msg_type_u16) = u16::try_from(msg_type) else {
		return Err(Status::invalid_argument("msg_type must fit uint16"));
	};
	if !(32768..=65535).contains(&msg_type_u16) {
		return Err(Status::invalid_argument("msg_type must be in [32768..65535]"));
	}
	Ok(msg_type_u16)
}

fn now_unix() -> u64 {
	SystemTime::now()
		.duration_since(UNIX_EPOCH)
		.map(|d| d.as_secs())
		.unwrap_or_default()
}

#[tonic::async_trait]
impl LightningNodeService for LightningNodeServiceImpl {
	async fn get_node_info(
		&self,
		_request: Request<proto::lnnode::v1::GetNodeInfoRequest>,
	) -> Result<Response<proto::lnnode::v1::GetNodeInfoResponse>, Status> {
		let best_block = self.node.channel_manager.current_best_block();
		let last_sync_unix = self.node.chain_sync_last_unix().await.unwrap_or_default();
		let gossip_last_sync_unix = self.node.gossip_sync_last_unix().await.unwrap_or_default();
		let chain_sync_state = proto::lnnode::v1::ChainSyncState {
			best_height: best_block.height,
			best_block_hash_hex: best_block.block_hash.to_string(),
			last_sync_unix,
		};
		let gossip_sync_state = proto::lnnode::v1::GossipSyncState {
			last_sync_unix: gossip_last_sync_unix,
		};
		Ok(Response::new(proto::lnnode::v1::GetNodeInfoResponse {
			node_pubkey: self.node.node_pubkey.serialize().to_vec(),
			network: self.node.config.network.as_str().to_string(),
			p2p_listen_addrs: self
				.node
				.config
				.p2p_listen_addrs
				.iter()
				.map(|a| a.to_string())
				.collect(),
			chain_sync_state: Some(chain_sync_state),
			gossip_sync_state: Some(gossip_sync_state),
		}))
	}

	async fn connect_peer(
		&self,
		_request: Request<proto::lnnode::v1::ConnectPeerRequest>,
	) -> Result<Response<proto::lnnode::v1::ConnectPeerResponse>, Status> {
		let req = _request.into_inner();
		let peer_pubkey =
			parse_pubkey33(&req.peer_pubkey).map_err(|e| Status::invalid_argument(e.to_string()))?;
		let already_connected = self
			.node
			.connect_peer(peer_pubkey, &req.addr)
			.await
			.map_err(|e| Status::internal(e.to_string()))?;
		Ok(Response::new(proto::lnnode::v1::ConnectPeerResponse { already_connected }))
	}

	async fn list_peers(
		&self,
		_request: Request<proto::lnnode::v1::ListPeersRequest>,
	) -> Result<Response<proto::lnnode::v1::ListPeersResponse>, Status> {
		let peers = self.node.list_peers().await;
		let peers = peers
			.into_iter()
			.map(|(peer_pubkey, addr, connected)| proto::lnnode::v1::Peer {
				peer_pubkey: peer_pubkey.serialize().to_vec(),
				addr,
				connected,
			})
			.collect();
		Ok(Response::new(proto::lnnode::v1::ListPeersResponse { peers }))
	}

	type SubscribePeerEventsStream = tokio_stream::wrappers::ReceiverStream<
		Result<proto::lnnode::v1::PeerEvent, Status>,
	>;
	async fn subscribe_peer_events(
		&self,
		_request: Request<proto::lnnode::v1::SubscribePeerEventsRequest>,
	) -> Result<Response<Self::SubscribePeerEventsStream>, Status> {
		let mut events = self.node.subscribe_peer_events();
		let (tx, rx) = mpsc::channel(128);
		tokio::spawn(async move {
			loop {
				match events.recv().await {
					Ok(event) => {
						let out = proto::lnnode::v1::PeerEvent {
							peer_pubkey: event.peer_pubkey.serialize().to_vec(),
							r#type: if event.connected {
								proto::lnnode::v1::peer_event::Type::Online as i32
							} else {
								proto::lnnode::v1::peer_event::Type::Offline as i32
							},
						};
						if tx.send(Ok(out)).await.is_err() {
							break;
						}
					},
					Err(broadcast::error::RecvError::Lagged(_)) => continue,
					Err(broadcast::error::RecvError::Closed) => break,
				}
			}
		});
		Ok(Response::new(ReceiverStream::new(rx)))
	}

	async fn send_custom_message(
		&self,
		_request: Request<proto::lnnode::v1::SendCustomMessageRequest>,
	) -> Result<Response<proto::lnnode::v1::SendCustomMessageResponse>, Status> {
		let req = _request.into_inner();
		let peer_pubkey =
			parse_pubkey33(&req.peer_pubkey).map_err(|e| Status::invalid_argument(e.to_string()))?;
		let msg_type = parse_custom_msg_type(req.msg_type)?;
		self.node
			.queue_custom_message(peer_pubkey, msg_type, req.data);
		Ok(Response::new(
			proto::lnnode::v1::SendCustomMessageResponse {},
		))
	}

	type SubscribeCustomMessagesStream = tokio_stream::wrappers::ReceiverStream<
		Result<proto::lnnode::v1::CustomMessage, Status>,
	>;
	async fn subscribe_custom_messages(
		&self,
		_request: Request<proto::lnnode::v1::SubscribeCustomMessagesRequest>,
	) -> Result<Response<Self::SubscribeCustomMessagesStream>, Status> {
		let req = _request.into_inner();
		let mut filter = HashSet::<u16>::new();
		for typ in req.msg_types {
			filter.insert(parse_custom_msg_type(typ)?);
		}
		let mut events = self.node.subscribe_custom_messages();
		let (tx, rx) = mpsc::channel(128);
		tokio::spawn(async move {
			loop {
				match events.recv().await {
					Ok(event) => {
						if !filter.is_empty() && !filter.contains(&event.msg_type) {
							continue;
						}
						let out = proto::lnnode::v1::CustomMessage {
							peer_pubkey: event.peer_pubkey.serialize().to_vec(),
							msg_type: u32::from(event.msg_type),
							data: event.data.as_ref().to_vec(),
						};
						if tx.send(Ok(out)).await.is_err() {
							break;
						}
					},
					Err(broadcast::error::RecvError::Lagged(_)) => continue,
					Err(broadcast::error::RecvError::Closed) => break,
				}
			}
		});
		Ok(Response::new(ReceiverStream::new(rx)))
	}

	async fn open_channel(
		&self,
		_request: Request<proto::lnnode::v1::OpenChannelRequest>,
	) -> Result<Response<proto::lnnode::v1::OpenChannelResponse>, Status> {
		let req = _request.into_inner();
		let peer_pubkey =
			parse_pubkey33(&req.peer_pubkey).map_err(|e| Status::invalid_argument(e.to_string()))?;

		let mut funding_events = self.node.subscribe_funding_tx_events();
		let channel_id = self
			.node
			.open_channel(peer_pubkey, req.local_funding_amount_sat, req.announce_channel)
			.map_err(|e| Status::internal(e.to_string()))?;

		let funding_txid = tokio::time::timeout(Duration::from_secs(30), async {
			loop {
				match funding_events.recv().await {
					Ok(ev) if ev.temporary_channel_id == channel_id => return Ok(ev.funding_txid),
					Ok(_) => continue,
					Err(broadcast::error::RecvError::Lagged(_)) => continue,
					Err(broadcast::error::RecvError::Closed) => {
						return Err(Status::internal("funding event channel closed"))
					},
				}
			}
		})
		.await
		.map_err(|_| Status::deadline_exceeded("timed out waiting for funding tx"))??;

		Ok(Response::new(proto::lnnode::v1::OpenChannelResponse {
			channel_id: channel_id.0.to_vec(),
			funding_txid_hex: funding_txid.to_string(),
		}))
	}

	async fn close_channel(
		&self,
		_request: Request<proto::lnnode::v1::CloseChannelRequest>,
	) -> Result<Response<proto::lnnode::v1::CloseChannelResponse>, Status> {
		let req = _request.into_inner();
		let channel_id = parse_channel_id(&req.channel_id)?;

		let channel = self
			.node
			.channel_manager
			.list_channels()
			.into_iter()
			.find(|c| c.channel_id == channel_id)
			.ok_or_else(|| Status::not_found("unknown channel_id"))?;

		if req.force {
			self.node
				.channel_manager
				.force_close_broadcasting_latest_txn(
					&channel_id,
					&channel.counterparty.node_id,
					"force close requested".to_string(),
				)
				.map_err(|e| Status::internal(format!("force_close failed: {e:?}")))?;
		} else {
			self.node
				.channel_manager
				.close_channel(&channel_id, &channel.counterparty.node_id)
				.map_err(|e| Status::internal(format!("close_channel failed: {e:?}")))?;
		}
		self.node.peer_manager.process_events();
		Ok(Response::new(proto::lnnode::v1::CloseChannelResponse {}))
	}

	async fn list_channels(
		&self,
		_request: Request<proto::lnnode::v1::ListChannelsRequest>,
	) -> Result<Response<proto::lnnode::v1::ListChannelsResponse>, Status> {
		let channels = self
			.node
			.channel_manager
			.list_channels()
			.into_iter()
			.map(|c| proto::lnnode::v1::Channel {
				channel_id: c.channel_id.0.to_vec(),
				peer_pubkey: c.counterparty.node_id.serialize().to_vec(),
				channel_value_sat: c.channel_value_satoshis,
				outbound_capacity_msat: c.outbound_capacity_msat,
				inbound_capacity_msat: c.inbound_capacity_msat,
				usable: c.is_usable,
			})
			.collect();
		Ok(Response::new(proto::lnnode::v1::ListChannelsResponse { channels }))
	}

	async fn new_address(
		&self,
		_request: Request<proto::lnnode::v1::NewAddressRequest>,
	) -> Result<Response<proto::lnnode::v1::NewAddressResponse>, Status> {
		let address = self
			.node
			.wallet
			.new_address()
			.await
			.map_err(|e| Status::internal(e.to_string()))?;
		Ok(Response::new(proto::lnnode::v1::NewAddressResponse { address }))
	}

	async fn wallet_balance(
		&self,
		_request: Request<proto::lnnode::v1::WalletBalanceRequest>,
	) -> Result<Response<proto::lnnode::v1::WalletBalanceResponse>, Status> {
		let (confirmed_sat, unconfirmed_sat) = self
			.node
			.wallet
			.balance_sat()
			.await
			.map_err(|e| Status::internal(e.to_string()))?;
		Ok(Response::new(proto::lnnode::v1::WalletBalanceResponse {
			confirmed_sat,
			unconfirmed_sat,
		}))
	}

	async fn send_to_address(
		&self,
		_request: Request<proto::lnnode::v1::SendToAddressRequest>,
	) -> Result<Response<proto::lnnode::v1::SendToAddressResponse>, Status> {
		let req = _request.into_inner();
		let txid = self
			.node
			.wallet
			.send_to_address(
				&self.node.esplora,
				&req.address,
				req.amount_sat,
				req.fee_rate_sat_per_vbyte,
				req.rbf,
				&req.idempotency_key,
			)
			.await
			.map_err(|e| Status::internal(e.to_string()))?;
		Ok(Response::new(proto::lnnode::v1::SendToAddressResponse {
			txid_hex: txid.to_string(),
		}))
	}

	async fn create_invoice(
		&self,
		_request: Request<proto::lnnode::v1::CreateInvoiceRequest>,
	) -> Result<Response<proto::lnnode::v1::CreateInvoiceResponse>, Status> {
		let req = _request.into_inner();
		if req.description_hash.len() != 32 {
			return Err(Status::invalid_argument("description_hash must be 32 bytes"));
		}
		let desc_hash = sha256::Hash::from_slice(&req.description_hash)
			.map_err(|_| Status::invalid_argument("description_hash must be 32 bytes"))?;
		let params = lightning::ln::channelmanager::Bolt11InvoiceParameters {
			amount_msats: Some(req.amount_msat),
			description: Bolt11InvoiceDescription::Hash(InvoiceSha256(desc_hash)),
			invoice_expiry_delta_secs: Some(u32::try_from(req.expiry_seconds).unwrap_or(u32::MAX)),
			..Default::default()
		};
		let invoice = self
			.node
			.channel_manager
			.create_bolt11_invoice(params)
			.map_err(|e| Status::internal(format!("create invoice failed: {e:?}")))?;
		let payment_hash = PaymentHash(*invoice.payment_hash().as_byte_array());
		let expiry_unix = now_unix().saturating_add(req.expiry_seconds);
		self.node
			.record_invoice_created(payment_hash, expiry_unix)
			.await
			.map_err(|e| Status::internal(e.to_string()))?;
		Ok(Response::new(proto::lnnode::v1::CreateInvoiceResponse {
			payment_request: invoice.to_string(),
			payment_hash: payment_hash.0.to_vec(),
		}))
	}

	async fn wait_invoice_settled(
		&self,
		_request: Request<proto::lnnode::v1::WaitInvoiceSettledRequest>,
	) -> Result<Response<proto::lnnode::v1::WaitInvoiceSettledResponse>, Status> {
		let req = _request.into_inner();
		let payment_hash = parse_payment_hash(&req.payment_hash)?;
		let timeout = Duration::from_secs(u64::from(req.timeout_seconds));

		if let Some(rec) = self.node.invoice_record(payment_hash).await {
			if rec.settled_unix.is_some() {
				return Ok(Response::new(proto::lnnode::v1::WaitInvoiceSettledResponse {
					state: proto::lnnode::v1::wait_invoice_settled_response::State::Settled as i32,
				}));
			}
			if now_unix() >= rec.expiry_unix {
				return Ok(Response::new(proto::lnnode::v1::WaitInvoiceSettledResponse {
					state: proto::lnnode::v1::wait_invoice_settled_response::State::Expired as i32,
				}));
			}
		}

		if timeout.is_zero() {
			return Ok(Response::new(proto::lnnode::v1::WaitInvoiceSettledResponse {
				state: proto::lnnode::v1::wait_invoice_settled_response::State::Unspecified as i32,
			}));
		}

		let mut updates = self.node.subscribe_invoice_updates();
		let _ = tokio::time::timeout(timeout, async {
			loop {
				match updates.recv().await {
					Ok(update) => {
						if update.payment_hash == payment_hash
							&& update.state == node::InvoiceState::Settled
						{
							return;
						}
					},
					Err(broadcast::error::RecvError::Lagged(_)) => continue,
					Err(broadcast::error::RecvError::Closed) => return,
				}
			}
		})
		.await;

		if let Some(rec) = self.node.invoice_record(payment_hash).await {
			if rec.settled_unix.is_some() {
				return Ok(Response::new(proto::lnnode::v1::WaitInvoiceSettledResponse {
					state: proto::lnnode::v1::wait_invoice_settled_response::State::Settled as i32,
				}));
			}
		if now_unix() >= rec.expiry_unix {
			return Ok(Response::new(proto::lnnode::v1::WaitInvoiceSettledResponse {
				state: proto::lnnode::v1::wait_invoice_settled_response::State::Expired as i32,
			}));
		}
	}

	Ok(Response::new(proto::lnnode::v1::WaitInvoiceSettledResponse {
		state: proto::lnnode::v1::wait_invoice_settled_response::State::Unspecified as i32,
	}))
	}

	async fn decode_invoice(
		&self,
		_request: Request<proto::lnnode::v1::DecodeInvoiceRequest>,
	) -> Result<Response<proto::lnnode::v1::DecodeInvoiceResponse>, Status> {
		let req = _request.into_inner();
		let invoice: Bolt11Invoice = req
			.payment_request
			.parse()
			.map_err(|_| Status::invalid_argument("invalid invoice"))?;

		let description_hash = match invoice.description() {
			Bolt11InvoiceDescriptionRef::Hash(hash) => hash.0.to_byte_array().to_vec(),
			Bolt11InvoiceDescriptionRef::Direct(_) => {
				return Err(Status::invalid_argument("invoice missing description_hash"))
			},
		};
		let amount_msat = invoice
			.amount_milli_satoshis()
			.ok_or_else(|| Status::invalid_argument("invoice missing amount"))?;

		Ok(Response::new(proto::lnnode::v1::DecodeInvoiceResponse {
			payee_pubkey: invoice.recover_payee_pub_key().serialize().to_vec(),
			description_hash,
			amount_msat,
			timestamp_unix: invoice.duration_since_epoch().as_secs(),
			expiry_seconds: invoice.expiry_time().as_secs(),
		}))
	}

	async fn pay_invoice(
		&self,
		_request: Request<proto::lnnode::v1::PayInvoiceRequest>,
	) -> Result<Response<proto::lnnode::v1::PayInvoiceResponse>, Status> {
		let req = _request.into_inner();
		let timeout = Duration::from_secs(u64::from(req.timeout_seconds));

		let invoice: Bolt11Invoice = req
			.payment_request
			.parse()
			.map_err(|_| Status::invalid_argument("invalid invoice"))?;

		let amount_msat = invoice
			.amount_milli_satoshis()
			.ok_or_else(|| Status::invalid_argument("invoice missing amount"))?;

		let payment_hash = PaymentHash(invoice.payment_hash().to_byte_array());
		let payment_id = PaymentId(payment_hash.0);

		let onion = RecipientOnionFields::secret_only(*invoice.payment_secret());

		let mut payment_params = PaymentParameters::from_bolt11_invoice(&invoice);
		payment_params.max_path_length = 1;
		payment_params.max_path_count = 1;

		let mut route_params =
			RouteParameters::from_payment_params_and_value(payment_params, amount_msat);
		route_params.max_total_routing_fee_msat = Some(req.fee_limit_msat);

		if let Err(err) = self.node.record_outbound_payment_started(payment_hash).await {
			return Err(Status::internal(err.to_string()));
		}
		let mut updates = self.node.subscribe_outbound_payment_updates();

		let retry_strategy = if timeout.is_zero() {
			Retry::Attempts(0)
		} else {
			Retry::Timeout(timeout)
		};

		if let Err(err) = self.node.channel_manager.send_payment(
			payment_hash,
			onion,
			payment_id,
			route_params,
			retry_strategy,
		) {
			let msg = format!("{err:?}");
			let _ = self.node.record_outbound_payment_failed(payment_hash, msg.clone()).await;
			return Ok(Response::new(proto::lnnode::v1::PayInvoiceResponse {
				status: proto::lnnode::v1::pay_invoice_response::Status::Failed as i32,
				payment_preimage: Vec::new(),
				failure_message: msg,
			}));
		}
		self.node.peer_manager.process_events();

		let result = if timeout.is_zero() {
			None
		} else {
			tokio::time::timeout(timeout, async {
				loop {
					match updates.recv().await {
						Ok(update) => match update {
							node::OutboundPaymentUpdate::Succeeded { payment_hash: hash, payment_preimage } if hash == payment_hash => {
								return Some(Ok(payment_preimage.0.to_vec()));
							},
							node::OutboundPaymentUpdate::Failed { payment_hash: hash, failure_message } if hash == payment_hash => {
								return Some(Err(failure_message));
							},
							_ => continue,
						},
						Err(broadcast::error::RecvError::Lagged(_)) => continue,
						Err(broadcast::error::RecvError::Closed) => return None,
					}
				}
			})
			.await
			.ok()
			.flatten()
		};

		match result {
			Some(Ok(preimage)) => Ok(Response::new(proto::lnnode::v1::PayInvoiceResponse {
				status: proto::lnnode::v1::pay_invoice_response::Status::Succeeded as i32,
				payment_preimage: preimage,
				failure_message: String::new(),
			})),
			Some(Err(msg)) => Ok(Response::new(proto::lnnode::v1::PayInvoiceResponse {
				status: proto::lnnode::v1::pay_invoice_response::Status::Failed as i32,
				payment_preimage: Vec::new(),
				failure_message: msg,
			})),
			None => {
				self.node.channel_manager.abandon_payment(payment_id);
				let msg = "timeout".to_string();
				let _ = self.node.record_outbound_payment_failed(payment_hash, msg.clone()).await;
				Ok(Response::new(proto::lnnode::v1::PayInvoiceResponse {
					status: proto::lnnode::v1::pay_invoice_response::Status::Failed as i32,
					payment_preimage: Vec::new(),
					failure_message: msg,
				}))
			},
		}
	}
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
	tracing_subscriber::fmt()
		.with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
		.init();

	let args = Args::parse();

	let grpc_addr: SocketAddr = parse_socket_addr(&args.grpc_addr)?;
	let network: BitcoinNetwork = args.network.parse()?;
	let p2p_listen_addrs = args
		.p2p_listen_addrs
		.iter()
		.map(|s| parse_socket_addr(s))
		.collect::<anyhow::Result<Vec<_>>>()?;

	let config = NodeConfig {
		data_dir: args.data_dir,
		network,
		grpc_addr,
		p2p_listen_addrs,
		esplora_base_url: args.esplora_base_url,
		rgs_base_url: args.rgs_base_url,
		rpc_auth_token: args.rpc_auth_token,
	};
	let node = node::Node::start(config).await?;

	tracing::info!(grpc_addr = %node.config.grpc_addr, "starting gRPC server");

	let svc = LightningNodeServiceServer::with_interceptor(
		LightningNodeServiceImpl { node: Arc::clone(&node) },
		AuthInterceptor { token: node.config.rpc_auth_token.clone() },
	);

	Server::builder()
		.add_service(svc)
		.serve_with_shutdown(node.config.grpc_addr, async {
			let _ = tokio::signal::ctrl_c().await;
			tracing::info!("shutdown requested");
		})
		.await?;

	Ok(())
}
