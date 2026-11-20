from haptic.samples import write_manifest, write_samples

if __name__ == "__main__":
    write_samples("samples")
    print(write_manifest("samples"))
