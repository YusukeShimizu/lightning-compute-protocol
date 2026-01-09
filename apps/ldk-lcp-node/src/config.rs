use std::net::{IpAddr, SocketAddr};
use std::path::PathBuf;
use std::str::FromStr;

use anyhow::{anyhow, bail, Context, Result};

#[derive(Clone, Copy, Debug)]
pub enum BitcoinNetwork {
	Mainnet,
	Testnet,
	Signet,
	Regtest,
}

impl BitcoinNetwork {
	pub fn as_str(&self) -> &'static str {
		match self {
			BitcoinNetwork::Mainnet => "mainnet",
			BitcoinNetwork::Testnet => "testnet",
			BitcoinNetwork::Signet => "signet",
			BitcoinNetwork::Regtest => "regtest",
		}
	}

	pub fn to_bitcoin(self) -> bitcoin::Network {
		match self {
			BitcoinNetwork::Mainnet => bitcoin::Network::Bitcoin,
			BitcoinNetwork::Testnet => bitcoin::Network::Testnet,
			BitcoinNetwork::Signet => bitcoin::Network::Signet,
			BitcoinNetwork::Regtest => bitcoin::Network::Regtest,
		}
	}

	pub fn to_invoice_currency(self) -> lightning_invoice::Currency {
		match self {
			BitcoinNetwork::Mainnet => lightning_invoice::Currency::Bitcoin,
			BitcoinNetwork::Testnet => lightning_invoice::Currency::BitcoinTestnet,
			BitcoinNetwork::Signet => lightning_invoice::Currency::BitcoinTestnet,
			BitcoinNetwork::Regtest => lightning_invoice::Currency::Regtest,
		}
	}
}

impl FromStr for BitcoinNetwork {
	type Err = anyhow::Error;
	fn from_str(s: &str) -> Result<Self> {
		match s {
			"mainnet" | "bitcoin" => Ok(BitcoinNetwork::Mainnet),
			"testnet" => Ok(BitcoinNetwork::Testnet),
			"signet" => Ok(BitcoinNetwork::Signet),
			"regtest" => Ok(BitcoinNetwork::Regtest),
			other => bail!("invalid --network: {other} (expected mainnet|testnet|signet|regtest)"),
		}
	}
}

#[derive(Clone)]
pub struct NodeConfig {
	pub data_dir: PathBuf,
	pub network: BitcoinNetwork,
	pub grpc_addr: SocketAddr,
	pub p2p_listen_addrs: Vec<SocketAddr>,
	pub esplora_base_url: String,
	pub rgs_base_url: Option<String>,
	pub rpc_auth_token: Option<String>,
}

impl NodeConfig {
	pub fn validate(&self) -> Result<()> {
		let grpc_ip = match self.grpc_addr.ip() {
			IpAddr::V4(ip) => IpAddr::V4(ip),
			IpAddr::V6(ip) => IpAddr::V6(ip),
		};
		if !grpc_ip.is_loopback() && self.rpc_auth_token.as_deref().unwrap_or_default().is_empty() {
			bail!(
				"--rpc-auth-token is required when binding gRPC to a non-loopback address (grpc_addr={})",
				self.grpc_addr
			);
		}
		if self.esplora_base_url.trim().is_empty() {
			bail!("--esplora-base-url is required");
		}
		if self.p2p_listen_addrs.is_empty() {
			bail!("at least one --p2p-listen is required");
		}
		Ok(())
	}

	pub fn ldk_user_agent(&self) -> String {
		format!("ldk-lcp-node/{}", env!("CARGO_PKG_VERSION"))
	}
}

pub fn parse_socket_addr(s: &str) -> Result<SocketAddr> {
	s.parse::<SocketAddr>()
		.with_context(|| format!("invalid socket address: {s}"))
}

pub fn parse_pubkey33(bytes: &[u8]) -> Result<bitcoin::secp256k1::PublicKey> {
	bitcoin::secp256k1::PublicKey::from_slice(bytes)
		.map_err(|err| anyhow!("invalid compressed pubkey (expected 33 bytes): {err}"))
}
