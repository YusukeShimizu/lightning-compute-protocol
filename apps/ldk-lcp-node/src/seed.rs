use std::path::Path;

use anyhow::{bail, Context, Result};
use rand::TryRngCore;

use crate::store;

pub const SEED_LEN: usize = 32;
const SEED_FILE: &str = "seed";

pub fn load_or_generate_seed(data_dir: &Path) -> Result<[u8; SEED_LEN]> {
	let path = data_dir.join(SEED_FILE);
	if let Some(bytes) = store::read_optional(&path)? {
		if bytes.len() != SEED_LEN {
			bail!(
				"invalid seed length in {} (got {}, expected {})",
				path.display(),
				bytes.len(),
				SEED_LEN
			);
		}
		let mut seed = [0u8; SEED_LEN];
		seed.copy_from_slice(&bytes);
		return Ok(seed);
	}

	let mut seed = [0u8; SEED_LEN];
	rand::rngs::OsRng
		.try_fill_bytes(&mut seed)
		.context("generate seed")?;
	store::write_atomic(&path, &seed).context("persist seed")?;
	Ok(seed)
}
