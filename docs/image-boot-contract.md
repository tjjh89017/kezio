# Image boot-entry contract

kezio-agent creates a firmware NVRAM boot entry for a deployed machine
(`efibootmgr --create`) once a deploy finishes, labelled
`kezio:<machine name>`. It never opens or edits a deployed image's file
system to do this - it only points the new entry at a fixed loader path
on the machine's own EFI System Partition (ESP).

**Contract: every bootable Image must already carry its own fallback
bootloader at the fixed path firmware falls back to on its own.** This
matches the shape of Clonezilla's `update-efi-nvram-boot-entry` and
Ironic IPA's `efi_utils`: both write an NVRAM entry only, and both leave
the fallback file to the image. kezio does the same.

The fixed path is per architecture. kezio supports exactly two
architectures today:

| Architecture | Fallback bootloader path on the ESP |
|---|---|
| x86_64 | `\EFI\BOOT\BOOTX64.EFI` |
| aarch64 | `\EFI\BOOT\BOOTAA64.EFI` (declared, not implemented yet) |

arm32 and RISC-V are out of scope; kezio-agent fails an aarch64 deploy
explicitly rather than writing an x86_64 path onto it.

A machine's NVRAM can lose a boot entry on its own - a factory reset, a
dead CMOS battery, a hypervisor's EFI variable store not surviving a
reboot. When that happens, firmware falls back to the fixed path above
with no NVRAM entry involved at all. The contract exists so that
fallback still finds a working bootloader.

kezio adds no ingest-time check on an uploaded Image and no Image status
warning. The operator who builds or picks a golden image carries the
responsibility to make sure it ships a fallback bootloader for its
architecture.

The shipped `kezio-default-finalize` PostHook does repair the common
case at deploy time. Its `install-removable-fallback` builtin step runs
before `efibootmgr` and makes the fallback directory hold a chain that
can start: it copies a shim or GRUB binary it finds under another
`EFI/<name>/` directory on the ESP into the fallback path, and it copies
GRUB in beside a shim. That second file is what an image which ships a
bare shim copy at the fallback path is missing - Ubuntu and Debian cloud
images both do. A shim never searches for its second stage; it opens
GRUB from its own directory only, so a lone shim starts and stops there.

The step is a no-op on an ESP that already boots, and it is scoped to
the same two architectures as the table above. It fails the deploy when
it installs a shim and finds no GRUB anywhere on the ESP to pair with
it, rather than reporting the deploy done and leaving the machine to
stop in its firmware minutes later. The agent also logs the fallback
directory's contents after every run, so a deploy record states what the
firmware will find.

