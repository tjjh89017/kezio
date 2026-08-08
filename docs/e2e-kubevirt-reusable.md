# Reuse of the KubeVirt BT-transfer e2e steps

kezio has no `workflow_call` entry point for the BT-transfer/full-deploy
e2e scenario. `.github/workflows/main.yaml`'s `e2e-bmc` job runs the
scenario directly as a sequence of steps, most of them calls to composite
actions under `.github/actions/`. Those composite actions are the reuse
surface: any caller, inside kezio or in another repository, can call one
directly with `uses:`.

## What the scenario proves

`e2e-bmc` stands up a KubeVirt + Multus cluster, ingests a real disk
image, seeds it with opentracker and an ezio seeder, PXE-boots a target
VM through a real KubeVirtBMC/Redfish endpoint, leeches the image over
BitTorrent onto the VM's disk, and verifies the deployed disk actually
boots: controller status, guest-agent connection, and a byte-for-byte
content check through QGA.

## Calling a composite action from another repository

```yaml
# .github/workflows/e2e.yml, in another repository
jobs:
  install-rke2:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: tjjh89017/kezio/.github/actions/install-rke2@main
        with:
          rke2-version: v1.36.2+rke2r1
```

`@main` tracks kezio's default branch. Pin a commit SHA instead for a
reproducible build.

## Composite actions used by `e2e-bmc`

| Action | Purpose |
|---|---|
| `customize-cloud-image` | Injects a marker file and qemu-guest-agent into the downloaded Ubuntu cloud image. |
| `install-rke2` | Installs RKE2 with Multus enabled. |
| `harden-provisioning-bridge` | Fixes up the provisioning bridge for real PXE/DHCP traffic. |
| `install-cluster-addons` | Installs cert-manager, KubeVirt, virtctl, and CDI. |
| `create-provisioning-nads` | Creates the provisioning `NetworkAttachmentDefinition`s. |
| `deploy-existing-dhcp` | Deploys a dnsmasq pod standing in for a site's existing DHCP server. |
| `deploy-bootd` | Deploys kezio-bootd on the provisioning bridge. |
| `assert-boot-artifacts-agent-commit` | Confirms the booted agent matches the commit under test. |
| `wait-for-image-ready` | Waits for an `Image` to reach `Ready`. |
| `provision-target-vm-via-bmc` | Creates the target VM, its `VirtualMachineBMC`, and the `Machine` that drives it. |
| `capture-vm-console` | Captures the target VM's console continuously, across VMI recreation. |
| `dump-e2e-diagnostics` | Dumps cluster state on failure. |
| `collect-e2e-diagnostics-bundle` | Collects an always-on diagnostics bundle as workflow artifacts. |

Each action's own `action.yml` documents its inputs.

## Caveats

- **Needs a KubeVirt-capable runner.** `/dev/kvm` must be usable; there
  is no software-emulation fallback.
- **Long-running.** `e2e-bmc` budgets 100 minutes: cluster+addon
  installs, a real PXE boot, a real BitTorrent leech, an in-guest
  reboot, and a second BMC power cycle.
- **BMC-driven only.** kezio never ships without a BMC driver, so the
  scenario has only the one, KubeVirtBMC-backed shape.
