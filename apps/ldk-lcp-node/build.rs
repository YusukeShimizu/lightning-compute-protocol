use std::path::PathBuf;

fn main() -> Result<(), Box<dyn std::error::Error>> {
	let proto_root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../go-lcpd/proto");
	let proto_file = proto_root.join("lnnode/v1/lnnode.proto");

	println!("cargo:rerun-if-changed={}", proto_file.display());

	tonic_build::configure()
		.build_server(true)
		.build_client(false)
		.compile(&[proto_file], &[proto_root])?;

	Ok(())
}
