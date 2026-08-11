# config

This directory holds every kustomization kezio ships: the CRDs and
controller-manager (`config/default`), and the standalone services
applied alongside it (`config/bootd`, `config/bootserver`,
`config/agentserver`, `config/boot-agent-server` - which turns on both
of those two at once, the ordinary case - `config/seeder`, and
`config/image-service`). Each of
those has its own README with its own setup steps. See
`docs/physical-lab-deployment.md` for the bring-up order across all of
them on real hardware, and `docs/lab-proxmox-rke2.md` for a
step-by-step walkthrough of that order on a Proxmox VE lab.

## Image boot-entry contract

An `Image` a `Machine` deploys must already carry its own fallback
bootloader on its EFI System Partition (ESP), at a fixed path per
architecture:

| Architecture | Fallback bootloader path on the ESP |
|---|---|
| x86_64 | `\EFI\BOOT\BOOTX64.EFI` |
| aarch64 | `\EFI\BOOT\BOOTAA64.EFI` (declared, not implemented yet) |

kezio-agent creates a UEFI NVRAM boot entry for a deployed machine at
finalize time, but it never opens or edits the deployed image's file
system to do so - it only points the entry at the fixed path above.
Firmware also falls back to that same path on its own, with no NVRAM
entry involved, whenever the entry kezio-agent created does not
survive (a factory reset, a dead CMOS battery, and so on). The fixed
path must already hold a working bootloader for that fallback to work.

kezio adds no check for this contract: no ingest-time check on an
uploaded `Image`, no `Image` status warning, no deploy-time gate. The
operator who builds the image carries the responsibility; this table
is the documentation of it. See
`docs/physical-lab-deployment.md`'s "Image boot-entry contract" section
for the full explanation, and the `install-removable-fallback` builtin
`PostHook` step for an opt-in way to add the fallback file to an image
that does not already carry it.
