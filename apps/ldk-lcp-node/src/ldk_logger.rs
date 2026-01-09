use lightning::util::logger::{Level, Logger, Record};

#[derive(Clone, Debug, Default)]
pub struct TracingLogger;

impl Logger for TracingLogger {
	fn log(&self, record: Record) {
		let peer_id = record.peer_id.map(|p| p.to_string());
		let channel_id = record.channel_id.map(|c| c.to_string());
		let msg = record.args.to_string();

		match record.level {
			Level::Gossip | Level::Trace => {
				tracing::trace!(
					module_path = record.module_path,
					file = record.file,
					line = record.line,
					peer_id = peer_id.as_deref(),
					channel_id = channel_id.as_deref(),
					"{msg}"
				);
			},
			Level::Debug => {
				tracing::debug!(
					module_path = record.module_path,
					file = record.file,
					line = record.line,
					peer_id = peer_id.as_deref(),
					channel_id = channel_id.as_deref(),
					"{msg}"
				);
			},
			Level::Info => {
				tracing::info!(
					module_path = record.module_path,
					file = record.file,
					line = record.line,
					peer_id = peer_id.as_deref(),
					channel_id = channel_id.as_deref(),
					"{msg}"
				);
			},
			Level::Warn => {
				tracing::warn!(
					module_path = record.module_path,
					file = record.file,
					line = record.line,
					peer_id = peer_id.as_deref(),
					channel_id = channel_id.as_deref(),
					"{msg}"
				);
			},
			Level::Error => {
				tracing::error!(
					module_path = record.module_path,
					file = record.file,
					line = record.line,
					peer_id = peer_id.as_deref(),
					channel_id = channel_id.as_deref(),
					"{msg}"
				);
			},
		}
	}
}

