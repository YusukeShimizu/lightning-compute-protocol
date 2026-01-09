use std::fs::{self, File, OpenOptions};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};

use anyhow::{Context, Result};

pub fn read_optional(path: impl AsRef<Path>) -> Result<Option<Vec<u8>>> {
	let path = path.as_ref();
	let mut file = match File::open(path) {
		Ok(f) => f,
		Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(None),
		Err(err) => return Err(err).context(format!("open {}", path.display())),
	};

	let mut buf = Vec::new();
	file.read_to_end(&mut buf)
		.context(format!("read {}", path.display()))?;
	Ok(Some(buf))
}

pub fn write_atomic(path: impl AsRef<Path>, data: &[u8]) -> Result<()> {
	let path = path.as_ref();
	let dir = path
		.parent()
		.with_context(|| format!("path has no parent: {}", path.display()))?;
	fs::create_dir_all(dir).context(format!("create_dir_all {}", dir.display()))?;

	let tmp_path = tmp_path_for(path);
	{
		let mut file = File::create(&tmp_path)
			.with_context(|| format!("create temp file {}", tmp_path.display()))?;
		file.write_all(data)
			.with_context(|| format!("write temp file {}", tmp_path.display()))?;
		file.sync_all()
			.with_context(|| format!("fsync temp file {}", tmp_path.display()))?;
	}

	fs::rename(&tmp_path, path).with_context(|| {
		format!(
			"rename temp file {} -> {}",
			tmp_path.display(),
			path.display()
		)
	})?;
	fsync_dir(dir)?;

	Ok(())
}

fn tmp_path_for(path: &Path) -> PathBuf {
	let parent = path.parent().unwrap_or_else(|| Path::new("."));
	let file_name = path
		.file_name()
		.unwrap_or_else(|| std::ffi::OsStr::new("tmp"))
		.to_string_lossy();
	let pid = std::process::id();
	let nanos = std::time::SystemTime::now()
		.duration_since(std::time::UNIX_EPOCH)
		.map(|d| d.as_nanos())
		.unwrap_or_default();
	parent.join(format!("{file_name}.tmp.{pid}.{nanos}"))
}

fn fsync_dir(dir: &Path) -> Result<()> {
	let file = OpenOptions::new()
		.read(true)
		.open(dir)
		.with_context(|| format!("open dir {}", dir.display()))?;
	file.sync_all()
		.with_context(|| format!("fsync dir {}", dir.display()))?;
	Ok(())
}

