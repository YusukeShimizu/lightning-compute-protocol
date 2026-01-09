use std::sync::Mutex;

use bitcoin::secp256k1::PublicKey;
use lightning::ln::msgs::{DecodeError, Init, LightningError};
use lightning::ln::peer_handler::CustomMessageHandler;
use lightning::ln::wire::{CustomMessageReader, Type};
use lightning::types::features::{InitFeatures, NodeFeatures};
use lightning::util::ser::{LengthLimitedRead, Writeable, Writer};
use tokio::sync::broadcast;

#[derive(Clone, Debug)]
pub struct PeerStatusEvent {
	pub peer_pubkey: PublicKey,
	pub connected: bool,
}

#[derive(Clone, Debug)]
pub struct RawCustomMessageEvent {
	pub peer_pubkey: PublicKey,
	pub msg_type: u16,
	pub data: std::sync::Arc<[u8]>,
}

#[derive(Clone, Debug)]
pub struct RawCustomMessage {
	pub msg_type: u16,
	pub data: Vec<u8>,
}

impl Type for RawCustomMessage {
	fn type_id(&self) -> u16 {
		self.msg_type
	}
}

impl Writeable for RawCustomMessage {
	fn write<W: Writer>(&self, w: &mut W) -> Result<(), lightning::io::Error> {
		w.write_all(&self.data)?;
		Ok(())
	}
}

pub struct RawCustomMessageHandler {
	pending_msgs: Mutex<Vec<(PublicKey, RawCustomMessage)>>,
	peer_events_tx: broadcast::Sender<PeerStatusEvent>,
	custom_messages_tx: broadcast::Sender<RawCustomMessageEvent>,
}

impl RawCustomMessageHandler {
	pub fn new(
		peer_events_tx: broadcast::Sender<PeerStatusEvent>,
		custom_messages_tx: broadcast::Sender<RawCustomMessageEvent>,
	) -> Self {
		Self { pending_msgs: Mutex::new(Vec::new()), peer_events_tx, custom_messages_tx }
	}

	pub fn queue_outbound(&self, peer_pubkey: PublicKey, msg_type: u16, data: Vec<u8>) {
		let mut pending = self.pending_msgs.lock().expect("poisoned");
		pending.push((peer_pubkey, RawCustomMessage { msg_type, data }));
	}
}

impl CustomMessageReader for RawCustomMessageHandler {
	type CustomMessage = RawCustomMessage;

	fn read<R: LengthLimitedRead>(
		&self,
		message_type: u16,
		buffer: &mut R,
	) -> Result<Option<Self::CustomMessage>, DecodeError> {
		if message_type < 32768 {
			return Ok(None);
		}
		let mut data = Vec::new();
		buffer
			.read_to_limit(&mut data, buffer.remaining_bytes())
			.map_err(|_| DecodeError::InvalidValue)?;
		Ok(Some(RawCustomMessage { msg_type: message_type, data }))
	}
}

impl CustomMessageHandler for RawCustomMessageHandler {
	fn handle_custom_message(
		&self,
		msg: Self::CustomMessage,
		sender_node_id: PublicKey,
	) -> Result<(), LightningError> {
		let _ = self.custom_messages_tx.send(RawCustomMessageEvent {
			peer_pubkey: sender_node_id,
			msg_type: msg.msg_type,
			data: std::sync::Arc::from(msg.data.into_boxed_slice()),
		});
		Ok(())
	}

	fn get_and_clear_pending_msg(&self) -> Vec<(PublicKey, Self::CustomMessage)> {
		let mut pending = self.pending_msgs.lock().expect("poisoned");
		std::mem::take(&mut *pending)
	}

	fn peer_disconnected(&self, their_node_id: PublicKey) {
		let _ = self.peer_events_tx.send(PeerStatusEvent {
			peer_pubkey: their_node_id,
			connected: false,
		});
	}

	fn peer_connected(&self, their_node_id: PublicKey, _msg: &Init, _inbound: bool) -> Result<(), ()> {
		let _ = self.peer_events_tx.send(PeerStatusEvent {
			peer_pubkey: their_node_id,
			connected: true,
		});
		Ok(())
	}

	fn provided_node_features(&self) -> NodeFeatures {
		NodeFeatures::empty()
	}

	fn provided_init_features(&self, _their_node_id: PublicKey) -> InitFeatures {
		InitFeatures::empty()
	}
}
