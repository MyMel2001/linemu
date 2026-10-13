# LinEMU

x86_64 hardware translation emulator written in pure Go without external runtime dependencies or kernel patching.

## Features

- **JIT & Dynamic Retranslation**: Translates guest x86_64 instruction sequences into intermediate representations with an execution cache.
- **Full PCI Subsystem**: Emulates PCI Host Bridge Configuration Space (ports `0xCF8` and `0xCFC`), automatic BAR allocation sizing, and multi-function device discovery.
- **Intel e1000 Gigabit NIC**: Standard Intel 82540EM PCI network adapter with full EEPROM emulation and TX/RX descriptor ring processing for Windows 11 and Linux in-box drivers.
- **Intel High Definition Audio (HDA)**: PCI audio controller supporting CORB/RIRB DMA buffers and integrated Realtek ALC888 codec verb responses.
- **Storage Subsystem**: ATA/IDE task files, Bus Master DMA (PRD), and disk geometry reporting (`IDENTIFY DEVICE`) supporting HDD, CD/DVD, Floppy, and USB block devices.
- **Host GPU Hardware Bridge**: SPICE / VirtIO / QXL display interface communicating directly with host graphics drivers (Vulkan, OpenGL, DirectX 12).
- **System Call Translation**: Translates guest kernel syscall traps directly into standard Linux host kernel calls.

## Building and Testing

Run test suite:

```sh
go test -v ./...
```

Run emulator:

```sh
go run main.go
```

## License

Licensed under SPL-R5 (Custom license).
