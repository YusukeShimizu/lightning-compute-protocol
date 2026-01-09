use std::path::{Path, PathBuf};
use std::str::FromStr;

use anyhow::{anyhow, Context, Result};
use bdk_esplora::EsploraAsyncExt;
use bdk_wallet::bitcoin::address::NetworkUnchecked;
use bdk_wallet::bitcoin::bip32::Xpriv;
use bdk_wallet::bitcoin::hex::DisplayHex;
use bdk_wallet::bitcoin::{Address, Amount, FeeRate, Network, ScriptBuf, Sequence, Transaction, Txid};
use bdk_wallet::file_store;
use bdk_wallet::{ChangeSet, KeychainKind, PersistedWallet, SignOptions, Update, Wallet};
use tokio::sync::Mutex;

use crate::store;

use esplora_client::r#async::DefaultSleeper;

pub type EsploraClient = esplora_client::AsyncClient<DefaultSleeper>;

const WALLET_DB_MAGIC: &[u8] = b"ldk-lcp-node wallet db v1";
const WALLET_DB_FILE: &str = "wallet.db";
const WALLET_IDEMPOTENCY_DIR: &str = "wallet-idempotency";

pub struct OnchainWallet {
	network: Network,
	data_dir: PathBuf,
	inner: Mutex<OnchainWalletInner>,
}

struct OnchainWalletInner {
	wallet: PersistedWallet<file_store::Store<ChangeSet>>,
	db: file_store::Store<ChangeSet>,
}

impl OnchainWallet {
	pub fn load_or_create(data_dir: &Path, network: Network, xprv: &Xpriv) -> Result<Self> {
		let (descriptor, change_descriptor) = descriptors_from_xprv(network, xprv)?;

		std::fs::create_dir_all(data_dir).context("create wallet data_dir")?;
		let db_path = data_dir.join(WALLET_DB_FILE);
		let (mut db, _changeset) =
			file_store::Store::<ChangeSet>::load_or_create(WALLET_DB_MAGIC, &db_path)
				.context("open wallet db")?;

		let wallet_opt = Wallet::load()
			.descriptor(KeychainKind::External, Some(descriptor.clone()))
			.descriptor(KeychainKind::Internal, Some(change_descriptor.clone()))
			.extract_keys()
			.check_network(network)
			.load_wallet(&mut db)
			.context("load wallet")?;

		let wallet = match wallet_opt {
			Some(wallet) => wallet,
			None => Wallet::create(descriptor, change_descriptor)
				.network(network)
				.create_wallet(&mut db)
				.context("create wallet")?,
		};

		Ok(Self {
			network,
			data_dir: data_dir.to_path_buf(),
			inner: Mutex::new(OnchainWalletInner { wallet, db }),
		})
	}

	pub async fn sync(&self, esplora: &EsploraClient) -> Result<()> {
		let mut inner = self.inner.lock().await;
		let request = inner.wallet.start_sync_with_revealed_spks().build();
		let response = esplora
			.sync(request, 5)
			.await
			.map_err(|err| anyhow!("esplora sync failed: {err:?}"))?;
		inner
			.wallet
			.apply_update(Update::from(response))
			.context("wallet apply update")?;
		let OnchainWalletInner { wallet, db } = &mut *inner;
		wallet.persist(db).context("wallet persist")?;
		Ok(())
	}

	pub async fn new_address(&self) -> Result<String> {
		let mut inner = self.inner.lock().await;
		let addr = inner.wallet.reveal_next_address(KeychainKind::External);
		let OnchainWalletInner { wallet, db } = &mut *inner;
		wallet.persist(db).context("wallet persist")?;
		Ok(addr.address.to_string())
	}

	pub async fn balance_sat(&self) -> Result<(u64, u64)> {
		let inner = self.inner.lock().await;
		let balance = inner.wallet.balance();
		let confirmed_sat = balance.confirmed.to_sat();
		let unconfirmed_sat =
			(balance.immature + balance.trusted_pending + balance.untrusted_pending).to_sat();
		Ok((confirmed_sat, unconfirmed_sat))
	}

	pub async fn send_to_address(
		&self,
		esplora: &EsploraClient,
		address: &str,
		amount_sat: u64,
		fee_rate_sat_per_vbyte: u64,
		rbf: bool,
		idempotency_key: &[u8],
	) -> Result<Txid> {
		if !idempotency_key.is_empty() {
			if let Some(txid) = self.load_idempotency_key(idempotency_key)? {
				return Ok(txid);
			}
		}

		let addr = Address::<NetworkUnchecked>::from_str(address)
			.context("parse address")?
			.require_network(self.network)
			.context("address network mismatch")?;

		let fee_rate = FeeRate::from_sat_per_vb(fee_rate_sat_per_vbyte)
			.ok_or_else(|| anyhow!("invalid fee_rate_sat_per_vbyte: {fee_rate_sat_per_vbyte}"))?;

		let tx = {
			let mut inner = self.inner.lock().await;

			let mut builder = inner.wallet.build_tx();
			builder.add_recipient(addr.script_pubkey(), Amount::from_sat(amount_sat));
			builder.fee_rate(fee_rate);
			builder.set_exact_sequence(if rbf {
				Sequence::ENABLE_RBF_NO_LOCKTIME
			} else {
				Sequence::ENABLE_LOCKTIME_NO_RBF
			});

			let mut psbt = builder.finish().context("build tx")?;
			let fully_signed = inner
				.wallet
				.sign(&mut psbt, SignOptions::default())
				.context("sign tx")?;
			if !fully_signed {
				return Err(anyhow!("transaction signing incomplete"));
			}

			let OnchainWalletInner { wallet, db } = &mut *inner;
			wallet.persist(db).context("wallet persist")?;

			psbt.extract_tx().context("extract tx")?
		};

		esplora
			.broadcast(&tx)
			.await
			.map_err(|err| anyhow!("esplora broadcast failed: {err:?}"))?;
		let txid = tx.compute_txid();
		if !idempotency_key.is_empty() {
			self.save_idempotency_key(idempotency_key, &txid)?;
		}
		Ok(txid)
	}

	pub async fn create_tx_to_script(
		&self,
		script_pubkey: ScriptBuf,
		amount_sat: u64,
		fee_rate_sat_per_vbyte: u64,
		rbf: bool,
	) -> Result<Transaction> {
		let fee_rate = FeeRate::from_sat_per_vb(fee_rate_sat_per_vbyte)
			.ok_or_else(|| anyhow!("invalid fee_rate_sat_per_vbyte: {fee_rate_sat_per_vbyte}"))?;

		let tx = {
			let mut inner = self.inner.lock().await;

			let mut builder = inner.wallet.build_tx();
			builder.add_recipient(script_pubkey, Amount::from_sat(amount_sat));
			builder.fee_rate(fee_rate);
			builder.set_exact_sequence(if rbf {
				Sequence::ENABLE_RBF_NO_LOCKTIME
			} else {
				Sequence::ENABLE_LOCKTIME_NO_RBF
			});

			let mut psbt = builder.finish().context("build tx")?;
			let fully_signed = inner
				.wallet
				.sign(&mut psbt, SignOptions::default())
				.context("sign tx")?;
			if !fully_signed {
				return Err(anyhow!("transaction signing incomplete"));
			}

			let OnchainWalletInner { wallet, db } = &mut *inner;
			wallet.persist(db).context("wallet persist")?;

			psbt.extract_tx().context("extract tx")?
		};

		Ok(tx)
	}

	fn load_idempotency_key(&self, key: &[u8]) -> Result<Option<Txid>> {
		let path = self.idempotency_path(key);
		let Some(bytes) = store::read_optional(&path)? else {
			return Ok(None);
		};
		let txid_str = std::str::from_utf8(&bytes).context("idempotency txid not utf8")?;
		let txid = Txid::from_str(txid_str.trim()).context("parse idempotency txid")?;
		Ok(Some(txid))
	}

	fn save_idempotency_key(&self, key: &[u8], txid: &Txid) -> Result<()> {
		let dir = self.data_dir.join(WALLET_IDEMPOTENCY_DIR);
		std::fs::create_dir_all(&dir).context("create idempotency dir")?;
		let path = self.idempotency_path(key);
		store::write_atomic(path, txid.to_string().as_bytes())
			.context("persist idempotency mapping")?;
		Ok(())
	}

	fn idempotency_path(&self, key: &[u8]) -> PathBuf {
		let key_hex = key.to_lower_hex_string();
		self.data_dir
			.join(WALLET_IDEMPOTENCY_DIR)
			.join(format!("{key_hex}.txt"))
	}
}

fn descriptors_from_xprv(network: Network, xprv: &Xpriv) -> Result<(String, String)> {
	let coin_type = match network {
		Network::Bitcoin => 0,
		Network::Testnet | Network::Signet | Network::Regtest => 1,
		_ => return Err(anyhow!("unsupported network: {network:?}")),
	};
	let xprv = xprv.to_string();
	Ok((
		format!("wpkh({xprv}/84'/{coin_type}'/0'/0/*)"),
		format!("wpkh({xprv}/84'/{coin_type}'/0'/1/*)"),
	))
}
