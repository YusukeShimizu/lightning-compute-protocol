use clap::Parser;
use tonic::{transport::Server, Request, Response, Status};

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

#[derive(Debug, Parser)]
#[command(name = "ldk-lcp-node", version)]
struct Args {
	#[arg(long, default_value = "127.0.0.1:10010", env = "LCP_LDK_NODE_GRPC_ADDR")]
	grpc_addr: String,
}

#[derive(Clone, Default)]
struct LightningNodeServiceImpl;

#[tonic::async_trait]
impl LightningNodeService for LightningNodeServiceImpl {
	async fn get_node_info(
		&self,
		_request: Request<proto::lnnode::v1::GetNodeInfoRequest>,
	) -> Result<Response<proto::lnnode::v1::GetNodeInfoResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn connect_peer(
		&self,
		_request: Request<proto::lnnode::v1::ConnectPeerRequest>,
	) -> Result<Response<proto::lnnode::v1::ConnectPeerResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn list_peers(
		&self,
		_request: Request<proto::lnnode::v1::ListPeersRequest>,
	) -> Result<Response<proto::lnnode::v1::ListPeersResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	type SubscribePeerEventsStream = tokio_stream::wrappers::ReceiverStream<
		Result<proto::lnnode::v1::PeerEvent, Status>,
	>;
	async fn subscribe_peer_events(
		&self,
		_request: Request<proto::lnnode::v1::SubscribePeerEventsRequest>,
	) -> Result<Response<Self::SubscribePeerEventsStream>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn send_custom_message(
		&self,
		_request: Request<proto::lnnode::v1::SendCustomMessageRequest>,
	) -> Result<Response<proto::lnnode::v1::SendCustomMessageResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	type SubscribeCustomMessagesStream = tokio_stream::wrappers::ReceiverStream<
		Result<proto::lnnode::v1::CustomMessage, Status>,
	>;
	async fn subscribe_custom_messages(
		&self,
		_request: Request<proto::lnnode::v1::SubscribeCustomMessagesRequest>,
	) -> Result<Response<Self::SubscribeCustomMessagesStream>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn open_channel(
		&self,
		_request: Request<proto::lnnode::v1::OpenChannelRequest>,
	) -> Result<Response<proto::lnnode::v1::OpenChannelResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn close_channel(
		&self,
		_request: Request<proto::lnnode::v1::CloseChannelRequest>,
	) -> Result<Response<proto::lnnode::v1::CloseChannelResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn list_channels(
		&self,
		_request: Request<proto::lnnode::v1::ListChannelsRequest>,
	) -> Result<Response<proto::lnnode::v1::ListChannelsResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn new_address(
		&self,
		_request: Request<proto::lnnode::v1::NewAddressRequest>,
	) -> Result<Response<proto::lnnode::v1::NewAddressResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn wallet_balance(
		&self,
		_request: Request<proto::lnnode::v1::WalletBalanceRequest>,
	) -> Result<Response<proto::lnnode::v1::WalletBalanceResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn send_to_address(
		&self,
		_request: Request<proto::lnnode::v1::SendToAddressRequest>,
	) -> Result<Response<proto::lnnode::v1::SendToAddressResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn create_invoice(
		&self,
		_request: Request<proto::lnnode::v1::CreateInvoiceRequest>,
	) -> Result<Response<proto::lnnode::v1::CreateInvoiceResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn wait_invoice_settled(
		&self,
		_request: Request<proto::lnnode::v1::WaitInvoiceSettledRequest>,
	) -> Result<Response<proto::lnnode::v1::WaitInvoiceSettledResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn decode_invoice(
		&self,
		_request: Request<proto::lnnode::v1::DecodeInvoiceRequest>,
	) -> Result<Response<proto::lnnode::v1::DecodeInvoiceResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}

	async fn pay_invoice(
		&self,
		_request: Request<proto::lnnode::v1::PayInvoiceRequest>,
	) -> Result<Response<proto::lnnode::v1::PayInvoiceResponse>, Status> {
		Err(Status::unimplemented("ldk-lcp-node: not implemented yet"))
	}
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
	tracing_subscriber::fmt()
		.with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
		.init();

	let args = Args::parse();
	let addr = args.grpc_addr.parse()?;

	tracing::info!(grpc_addr = %args.grpc_addr, "starting gRPC server");

	Server::builder()
		.add_service(LightningNodeServiceServer::new(LightningNodeServiceImpl::default()))
		.serve(addr)
		.await?;

	Ok(())
}
