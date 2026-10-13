# LinEMU

x86_64 hardware translation emulator written in pure Go without external runtime dependencies or kernel patching.

## Features

- **JIT & Dynamic Retranslation**: Translates guest x86_64 instruction sequences into intermediate representations with an execution cache.
- **Full PCI Subsystem**: Emulates PCI Host Bridge Configuration Space (ports `0xCF8` and `0xCFC`), automatic BAR allocation sizing, and multi-function device discovery.
- **Virtual Disk Images & ISO Booting**: Supports direct booting from ISO and raw IMG disk images, with the ability to dynamically create new virtual HDD images without relying on user physical drives.
- **Unique Drive Identifiers**: All attached virtual drives and storage media are assigned cryptographic/hardware unique IDs on boot and reported via IDE/ATA `IDENTIFY DEVICE`.
- **Intel e1000 Gigabit NIC**: Standard Intel 82540EM PCI network adapter with full EEPROM emulation and TX/RX descriptor ring processing for Windows 11 and Linux in-box drivers.
- **Intel High Definition Audio (HDA)**: PCI audio controller supporting CORB/RIRB DMA buffers and integrated Realtek ALC888 codec verb responses.
- **Storage Subsystem**: ATA/IDE task files, Bus Master DMA (PRD), and disk geometry reporting (`IDENTIFY DEVICE`) supporting HDD, CD/DVD, Floppy, and USB block devices.
- **Host GPU Hardware Bridge**: SPICE / VirtIO / QXL display interface communicating directly with host graphics drivers (Vulkan, OpenGL, DirectX 12).
- **System Call Translation**: Translates guest kernel syscall traps directly into standard Linux host kernel calls.

## Command Line Usage

### 1. Create a virtual HDD image
```sh
go run main.go -create-hdd="my_disk.img 2048"
```
*(Creates a 2048 MB raw virtual disk)*

### 2. Boot from ISO image with virtual HDD attached
```sh
go run main.go -iso=/path/to/installer.iso -img=my_disk.img
```

### 3. Boot directly from IMG virtual disk
```sh
go run main.go -img=my_disk.img
```

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
