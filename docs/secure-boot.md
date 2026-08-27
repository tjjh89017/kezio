# UEFI Secure Boot

This document is for whoever operates a kezio-managed boot segment and
wants to know whether a target machine can keep UEFI Secure Boot turned
on through the whole kezio boot chain (PXE firmware -> shim -> GRUB ->
live kernel), and what that requires from them.

Short answer: yes, a production machine can keep Secure Boot on. Every
binary kezio serves before the live kernel - shim and GRUB - is a
Debian-signed release artifact, not something kezio builds itself. What
you decide is the kernel-signing story (below), and CI does not exercise
any of this end to end (also below).

## The chain

```
UEFI firmware (db: Microsoft UEFI CA)
  -> shim   (shimx64.efi, Microsoft-signed; embeds Debian's vendor cert)
    -> GRUB   (grubx64.efi, Debian-signed; trusted via shim's vendor cert)
      -> kernel (vmlinuz; trust depends on how it was built - see below)
```

1. **Firmware -> shim.** The firmware's `db` (signature database) ships
   pre-loaded with Microsoft's UEFI CA on essentially all x86 hardware.
   `shimx64.efi` is signed by that CA (specifically, by one of
   Microsoft's "Microsoft Corporation UEFI CA" / "Microsoft UEFI CA
   2023" intermediates), so the firmware chainloads it without any
   per-site key enrollment.

2. **Shim -> GRUB.** Debian's shim build embeds a second, Debian-owned
   certificate ("Debian Secure Boot CA") as its vendor cert - a trust
   root shim itself checks in addition to the firmware's `db`. Debian's
   GRUB build (published as `grubx64.efi`; the netboot variant
   `grubnetx64.efi.signed` shipped by the `grub-efi-amd64-signed`
   package, renamed to the name shim requests - see
   `hack/live-image/build.sh`) is signed by a certificate chaining up
   to that same Debian CA ("Debian Secure Boot Signer 2022 - grub2"),
   so shim chainloads it without needing that key enrolled in firmware
   `db` or a MOK (Machine Owner Key) separately.

3. **GRUB -> kernel.** GRUB's shim-lock verifier checks the signature on
   whatever PE/COFF binary it loads next (the kernel), against the same
   trust roots shim already validated GRUB against. Whether that check
   passes depends entirely on whether the kernel kezio serves is signed
   by a certificate in that chain - see the next section.

`hack/live-image/build.sh`'s `stage_signed_boot_binaries` step is what
pulls `shimx64.efi`/`grubx64.efi` into the published `kezio-boot-
artifacts` image (`docker/boot-artifacts/Dockerfile`). Each `Subnet`'s
bootd Deployment then carries a `fetch-boot-artifacts` initContainer
that `cp`s those two files out of that image into the directory bootd
serves over TFTP. The controller builds that initContainer, not a
manifest: see `internal/controller/subnet_bootd_deployment.go`, and the
`BOOTD_DEPLOYMENT_BOOT_ARTIFACTS_IMAGE` variable that names the image.
The build step extracts the `.signed`
member of each package (`shimx64.efi.signed`, and
`grubnetx64.efi.signed` - the netboot GRUB build, whose embedded
`/grub` prefix resolves against the TFTP device it was loaded from,
unlike the disk-boot `grubx64.efi.signed` whose `/EFI/debian` prefix
can never resolve on a netboot; the extraction only accepts a path
ending in `.signed`, so it fails closed instead of silently taking an
unsigned copy if a future package revision were to add one) and,
before publishing either file,
runs `sbverify --list` against it and rejects the build if that
verification fails or the file is empty. That check confirms the
binaries actually carry an Authenticode signature; it does not (and
cannot, without loading `db`/vendor certs into the checking environment)
confirm the firmware will end up trusting that signature - that trust
is a property of the certificates above, not of the build script.

## Kernel signing

The live kernel `hack/live-image/build.sh` ships
(`hack/live-image/config/chroot`'s `LB_LINUX_PACKAGES="linux-image"`,
which live-build resolves to Debian's `linux-image-amd64` metapackage)
is **not built by kezio**. It is whatever `linux-image-amd64` pulls in
from Debian sid at build time. There are two realistic ways to make
that kernel trusted by the chain above:

**(a) Ride Debian's own signed kernel - the recommended default.**
Debian publishes `linux-signed-amd64`, a build of the current kernel
signed by the same "Debian Secure Boot" key family that signs GRUB.
`linux-image-amd64` depends on the matching `linux-signed-amd64`
version when one is available for that kernel ABI, and installs the
signed `vmlinuz` with no extra step from kezio. Because shim already
trusts the Debian vendor cert (step 2 above), this kernel chainloads
under Secure Boot with **zero enrollment work per machine or per
site** - the same property that makes shim/GRUB work out of the box.
This is why option (a), not self-signing, is the right default for
kezio: kezio's whole point is unattended, fleet-wide PXE provisioning,
and any option that requires a human at each machine (or a
provisioning-time MOK enrollment step per machine) works against that.

The caveat that comes with tracking Debian sid (`hack/live-image/
build.sh`'s own header explains why sid is the right tradeoff for
everything else this image builds): `linux-signed-amd64` is built and
published *after* the plain `linux-image-amd64-unsigned` upload it
signs, so there is a window - usually short, but real - where sid's
`linux-image-amd64` metapackage still resolves to an unsigned kernel
build because the signed counterpart has not caught up yet. A live
image built during that window boots fine with Secure Boot off but
would fail the GRUB -> kernel signature check with Secure Boot on. If a
built image needs to be Secure-Boot-verified before it is trusted for a
production rollout, check the kernel `dist/live/vmlinuz` actually
carries a Debian signature (`sbverify --list dist/live/vmlinuz`,
looking for the same "Debian Secure Boot" issuer this document already
confirms for GRUB) rather than assuming it, given that gap.

**(b) Self-sign the kernel and enroll the key - not the default.**
kezio could sign `vmlinuz` itself (`sbsign` with a project-owned key)
and enroll that key either into firmware `db` directly or as a MOK via
shim's MokManager. This works, but MOK enrollment is an interactive,
per-machine step (a human confirms it from the firmware console on
next reboot, or it is scripted via `mokutil --import` plus a reboot
that a human or a remote KVM session drives through the MokManager
prompt) - it does not fit an operator-less PXE provisioning flow, and
enrolling straight into `db` requires the same kind of per-site
physical/remote access kezio's network-boot design otherwise avoids
needing. This option is left documented, not implemented: it is the
right answer only if a site's kernel needs to diverge from stock
Debian (a custom build, an out-of-tree module signed the same way)
badly enough to be worth that operational cost.

## GRUB configuration is not signature-checked

The GRUB config kezio's boot server hands back per machine
(`internal/bootserver`'s `renderNetBootConfig`) is generated dynamically
per request and served in plaintext over HTTP - it is not embedded in
the signed `grubx64.efi` binary and is not itself signed. This matters
because GRUB's shim-lock verifier only checks the signature of PE/COFF
binaries it loads next (GRUB itself, and the kernel); it does not check
the integrity or authenticity of the `grub.cfg` text GRUB parses to get
there, and Debian's `grub-efi-amd64-signed` build does not add the
extra "signed environment / lockdown" verifiers (used by some other
distributions) that would change that. In other words: Secure Boot on
this chain guarantees the shim, GRUB, and kernel binaries are the ones
Debian signed - it does not by itself guarantee the boot *parameters*
GRUB was told to use were not tampered with in transit. (This
assumption is based on Debian's shim-lock verifier scope and the
absence of any config-signing tooling in `grub-efi-amd64-signed`; if a
future Debian GRUB build changes this, re-verify before relying on it.)
kezio's mitigation for that gap is the same one used elsewhere in the
boot path, not a GRUB feature: `internal/bootserver` resolves the
requesting MAC to a `Machine` and only returns a live-boot config
(carrying a freshly minted, single-use token) when that machine's state
calls for one; every other case - an unrecognized MAC included - gets
the fixed, fail-secure `bootLocalConfig` that chainloads the local disk
(see `internal/bootserver`'s package doc). Operators are also expected
to run the boot L2 segment as a controlled network, not an open,
untrusted one: target machines attach only to the provisioning bridge,
and they do not share it with unrelated cluster or data-plane traffic
(`docs/network-model.md`).

## CI does not exercise Secure Boot

None of kezio's CI workflows boot with Secure Boot enabled:

- Every target VM comes from the `.github/actions/create-target-vm`
  composite action, which defines it with OVMF UEFI firmware and
  `firmware.bootloader.efi.secureBoot: false` - no Secure Boot keys are
  enrolled, so no lane exercises the shim/GRUB/kernel signature checks
  at all. The shim and GRUB binaries these VMs boot are still the real
  signed artifacts the `boot-artifacts` job builds - only the
  firmware-side enforcement is off.

This is a deliberate, documented gap, not a silent one: a real
end-to-end Secure Boot test needs enrolled Secure Boot keys in the test
firmware (OVMF's `OVMF_VARS` pre-seeded with Microsoft's keys, which is
possible but adds real setup and maintenance cost) plus a kernel
confirmed signed at test time (see the sid timing caveat above) plus a
firmware that actually enforces the checks (OVMF has an SB-enabled
variant; real hardware would be release-gated at best, and kezio's e2e
lanes run in KubeVirt/OVMF, not on physical hardware). None of kezio's
CI lanes currently do this. If this coverage gap is ever worth closing,
it belongs in a release-gated lane using OVMF's Secure-Boot-enabled
firmware variant with pre-enrolled Microsoft keys, verifying the VM
firmware itself refuses to boot when handed a deliberately-corrupted
signed artifact - not something to bolt onto the existing always-on
lanes.

Because the served shim/GRUB are always the signed variants (verified
mechanically at build time, see above) and the boot chain's trust
requirements are documented here, a production site can turn Secure
Boot on for its machines today; kezio's CI simply never proves that
configuration works before every merge.
