//! Rebuild when the embedded assets change: include_dir! doesn't tell cargo about the files it
//! reads on stable Rust. Cargo checks a directory's whole tree for changes.
fn main() {
    println!("cargo::rerun-if-changed=../../assets");
}
