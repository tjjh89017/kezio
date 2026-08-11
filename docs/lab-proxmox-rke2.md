# Lab walkthrough: RKE2 on Proxmox VE, with a Redfish BMC shim

This guide builds one complete kezio lab, in order, from an empty
Proxmox VE host to a target VM that deploys an image and boots it. Every
step is a command you run. It uses Proxmox VMs instead of real servers,
and a Redfish shim in front of the Proxmox API instead of a real BMC.

`docs/physical-lab-deployment.md` is the reference guide: it explains
the network model, every option, and every port. This guide is the
opposite - it makes one choice at each point and tells you what to type.
Read the reference guide when you want to know why a step exists, or
when your site does not match the shape below.

The lab is one Site with one Subnet. The steps scale to more sites
without change; see `config/bootd/README.md`'s "Per-site addressing".

`docs/lab-all-in-one.yaml` holds everything sections 5, 6, and 8 create,
in one file. Read this guide first: that file assumes the cluster from
sections 2 to 4 already exists, and every address, image tag, and
credential in it needs editing before it works. Sections 5 and 6 stay
the explanation of what it contains.

## 1. What the lab looks like

Three parts run on one Proxmox VE host:

- **`kezio-node`** - one VM. It runs RKE2 and every kezio service.
- **`kezio-target-1`** - one VM. It is the "bare-metal machine" kezio
  deploys an OS onto.
- **proxmox-redfish** - a daemon on the Proxmox host itself. It gives
  each VM a Redfish endpoint, so kezio can power it on and set a
  one-time PXE boot. kezio has no mode that deploys a machine without a
  BMC.

Two bridges carry the traffic:

| Bridge | Use |
|---|---|
| `vmbr0` | Management and uplink. `kezio-node` reaches the internet and the Proxmox API through it. |
| `vmbr1` | The provisioning segment. It has no uplink port and no host address. `kezio-target-1` has only this network. |

Addresses on the provisioning segment (`192.0.2.0/24` in this guide -
replace it with your own range):

| Address | Owner |
|---|---|
| `192.0.2.1` | `kezio-node`'s own address on the segment (for your own `curl` checks) |
| `192.0.2.2` | bootd (`Subnet.spec.bootdServerIP`) |
| `192.0.2.4` | opentracker |
| `192.0.2.5` | the ezio seeder |
| `192.0.2.10` - `192.0.2.100` | the lease range bootd hands out |

The `kezio-node` VM needs at least 8 vCPU, 16 GiB RAM, and 200 GiB of
disk. Ingest writes a full copy of each image into PVCs, so give the
disk room for every image you plan to ingest.

## 2. Prepare the Proxmox host

### 2.1 Create the provisioning bridge

Add `vmbr1` in the Proxmox web UI (Datacenter -> Node -> System ->
Network -> Create -> Linux Bridge), or write it into
`/etc/network/interfaces`:

```
auto vmbr1
iface vmbr1 inet manual
    bridge-ports none
    bridge-stp off
    bridge-fd 0
```

Apply it (`ifreload -a`, or reboot). Leave the bridge with no address
and no port: nothing outside the lab must reach this segment, and bootd
becomes its DHCP authority in section 6.

Leave the Proxmox firewall off for both VMs' NICs on `vmbr1` (it is off
by default). The `kezio-node` VM forwards frames for pod MAC addresses
that are not its own NIC's, and a firewall on that port drops them.

### 2.2 Create the `kezio-node` VM

Create a VM with Debian 13 or Ubuntu 24.04, and two NICs:

- `net0` on `vmbr0` - management, DHCP or a static address.
- `net1` on `vmbr1` - the provisioning segment, no address of its own
  yet. Section 3.1 puts it into a bridge inside the VM.

### 2.3 Create the `kezio-target-1` VM

This VM stands in for a bare-metal machine, so build it the way a real
machine boots:

| Setting | Value | Why |
|---|---|---|
| BIOS | OVMF (UEFI) | kezio's boot chain is shim -> GRUB -> live kernel, all UEFI. |
| EFI disk | Added, **without** pre-enrolled keys | Secure Boot off for the first run. See section 12 to turn it on. |
| Machine | q35 | Matches OVMF. |
| Network | `net0` on `vmbr1` only | The machine boots from this segment. Write down its MAC address. |
| Disk | SCSI, 32 GiB or more, with a serial | The serial is how kezio picks the disk. |
| Boot order | `net0` first, then the disk | kezio sets a one-time PXE boot over Redfish, but a listed net device is what the shim reorders. |
| QEMU Guest Agent | Enabled | Lets you confirm the deployed OS booted. |

Set the disk serial on the Proxmox host, because the web UI has no field
for it (`501` is the VM ID used throughout this guide):

```sh
qm set 501 --scsi0 local-lvm:vm-501-disk-0,serial=KEZIOLAB0001
```

Leave the VM powered off. kezio powers it on through the BMC.

### 2.4 Install proxmox-redfish on the Proxmox host

[v1k0d3n/proxmox-redfish](https://github.com/v1k0d3n/proxmox-redfish) is
a Redfish API in front of the Proxmox API. It answers the exact calls
`internal/bmc/redfish` makes: `ComputerSystem.Reset` for power, and a
`Boot` PATCH for the one-time PXE boot.

Run its installer on the Proxmox host, as root:

```sh
curl -fsSLO https://raw.githubusercontent.com/v1k0d3n/proxmox-redfish/main/scripts/install.sh
less install.sh          # read it before you run it
bash install.sh
```

The installer puts the daemon in `/opt/proxmox-redfish`, writes a
self-signed certificate, and creates a systemd unit that listens on
port `8443`.

Fill in `/opt/proxmox-redfish/config/params.env` with the host's own
values:

```sh
export PROXMOX_HOST="10.0.0.10"      # this host's management address
export PROXMOX_USER="root@pam"
export PROXMOX_PASSWORD="..."
export PROXMOX_NODE="pve"            # this node's hostname
export VERIFY_SSL="false"
```

Start it, and confirm the port it really listens on - the shipped
example unit uses `8000` while the installer's own unit uses `8443`:

```sh
systemctl enable --now proxmox-redfish
systemctl cat proxmox-redfish | grep ExecStart
```

### 2.5 Create the Proxmox credentials kezio uses

kezio connects with HTTP Basic authentication (`internal/bmc/redfish`
sets `BasicAuth: true`, because a controller connects many times an hour
and BMCs cap concurrent Redfish sessions). proxmox-redfish accepts a
Proxmox API token in that Basic header.

In the Proxmox web UI:

1. Datacenter -> Permissions -> Users -> Add: user `kezio@pve`.
2. Datacenter -> Permissions -> API Tokens -> Add: token ID `kezio` on
   that user. Copy the secret - Proxmox shows it once.
3. Datacenter -> Permissions -> Add -> **API Token Permission**: path
   `/vms`, token `kezio@pve!kezio`, role `PVEVMAdmin`.

Step 3 is not optional and not the same as a user permission:
proxmox-redfish checks the ACL for the exact identity in the Basic
header, and that identity is the token (`kezio@pve!kezio`), not the user
behind it. A token with privilege separation turned off inherits the
user's rights inside Proxmox but still has no ACL entry of its own, and
the shim then refuses the call.

Check the endpoint before you go further:

```sh
curl -k -u 'kezio@pve!kezio:<token-secret>' \
  https://10.0.0.10:8443/redfish/v1/Systems/501 | jq .PowerState
```

## 3. Install RKE2 on `kezio-node`

Every command from here on runs inside the `kezio-node` VM.

### 3.1 Put the provisioning NIC into a bridge

Multus attaches pods to the provisioning segment through the `bridge`
CNI plugin, so the VM's `net1` interface must be a port of a Linux
bridge inside the VM. With netplan (`/etc/netplan/60-kezio-prov.yaml`):

```yaml
network:
  version: 2
  ethernets:
    ens19:            # the net1 interface - check `ip link`
      dhcp4: false
  bridges:
    kezio-prov0:
      interfaces: [ens19]
      addresses: [192.0.2.1/24]
      dhcp4: false
      parameters:
        stp: false
        forward-delay: 0
```

Apply it with `netplan apply`, then confirm `ip -br addr show
kezio-prov0` reports the address and `UP`.

The address `192.0.2.1` is for your own checks from the node. It is not
a gateway: every kezio service gets its own address on the segment.

### 3.2 Install RKE2 with Multus

```sh
sudo mkdir -p /etc/rancher/rke2
cat <<'EOF' | sudo tee /etc/rancher/rke2/config.yaml
write-kubeconfig-mode: "0644"
cni:
  - multus
  - canal
EOF

curl -sfL https://get.rke2.io | sudo INSTALL_RKE2_VERSION="v1.36.2+rke2r1" sh -
sudo systemctl enable --now rke2-server.service

sudo ln -sf /var/lib/rancher/rke2/bin/kubectl /usr/local/bin/kubectl
export KUBECONFIG=/etc/rancher/rke2/rke2.yaml
kubectl wait --for=condition=Ready node --all --timeout=300s
```

`multus` must come first in the `cni` list; `canal` stays the pod
network. Put the `KUBECONFIG` export into your shell profile.

### 3.3 Install a StorageClass

RKE2 ships no default StorageClass, and kezio's ingest path binds PVCs
for the staging area and for every image partition:

```sh
kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.37/deploy/local-path-storage.yaml
kubectl -n local-path-storage rollout status deployment/local-path-provisioner --timeout=180s
kubectl patch storageclass local-path \
  -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'
```

`local-path` is ReadWriteOnce only. `config/image-service/pvc.yaml` asks
for ReadWriteMany, because the ingest Job mounts the same staged upload
the image-service wrote. On a single node, ReadWriteOnce is enough -
section 5.1 patches the claim.

### 3.4 Let PXE traffic cross the bridge

Two host-level fixes, needed once RKE2 is up and before any pod attaches
to `kezio-prov0`:

```sh
# canal's br_netfilter sends bridged frames through FORWARD, whose
# policy is DROP once the cluster is up - that eats the PXE DHCPDISCOVER.
sudo iptables -C FORWARD -i kezio-prov0 -o kezio-prov0 -j ACCEPT 2>/dev/null || \
  sudo iptables -I FORWARD -i kezio-prov0 -o kezio-prov0 -j ACCEPT

# virtio-net leaves UDP checksums to hardware. The bridge-to-pod-veth
# hop never fills them in, and the kernel then drops the DHCP datagram.
for port in 67 68; do
  sudo iptables -t mangle -C POSTROUTING -o kezio-prov0 -p udp --dport "$port" -j CHECKSUM --checksum-fill 2>/dev/null || \
    sudo iptables -t mangle -A POSTROUTING -o kezio-prov0 -p udp --dport "$port" -j CHECKSUM --checksum-fill
done
```

Both rules are lost on reboot. Make them persistent
(`iptables-persistent`, or a systemd unit) before you leave the lab
running.

If your node runs `nftables` instead of legacy `iptables`, add the
FORWARD accept with `nft insert rule inet filter FORWARD iifname
"kezio-prov0" oifname "kezio-prov0" accept`. `nft` has no `CHECKSUM`
target; `sudo ethtool -K kezio-prov0 tx off` is the cruder replacement.

## 4. Get the kezio images

The release workflow publishes every image to `ghcr.io/tjjh89017`. Pin
one released version and use it everywhere:

```sh
export KEZIO_VERSION=0.1.7      # or "main" for the branch build
export MANAGER_IMG=ghcr.io/tjjh89017/kezio:${KEZIO_VERSION}
export IMAGE_SERVICE_IMG=ghcr.io/tjjh89017/kezio-image-service:${KEZIO_VERSION}
export INGEST_IMG=ghcr.io/tjjh89017/kezio-ingest:${KEZIO_VERSION}
export SEEDER_IMG=ghcr.io/tjjh89017/kezio-seeder:${KEZIO_VERSION}
export BOOTD_IMG=ghcr.io/tjjh89017/kezio-bootd:${KEZIO_VERSION}
export BOOT_ARTIFACTS_IMG=ghcr.io/tjjh89017/kezio-boot-artifacts:${KEZIO_VERSION}
```

The image tag has no `v`, but the git tag it comes from does: git tag
`v0.1.7` publishes image tag `0.1.7`. A `v` in `KEZIO_VERSION` makes
every pull fail with `not found`.

Releases up to and including `v0.1.7` are an exception for one image:
`kezio-boot-artifacts` kept the `v` in those releases, so for them use
`ghcr.io/tjjh89017/kezio-boot-artifacts:v${KEZIO_VERSION}` instead. All
later releases use the same tag as the other images.

To run your own build instead, clone the repository on the node and use
the `docker-build-*` / `docker-push-*` targets (`make help` lists them).
The boot-artifacts image is the one exception: build the live image
first with `hack/live-image/build.sh` (it needs Docker and runs
live-build in a privileged container), then `make
docker-build-boot-artifacts`.

You also need `kubectl`, `kustomize`, and `kezioctl`. Clone the
repository on the node - the deploy steps below apply kustomizations
from the working tree:

```sh
git clone https://github.com/tjjh89017/kezio.git
cd kezio
make build-kezioctl        # writes bin/kezioctl
```

## 5. Deploy kezio

### 5.1 CRDs and the controller-manager

`config/default` includes the validating webhook and a cert-manager
`Certificate` that serves it, so cert-manager must be installed first:

```sh
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=180s
```

Then deploy kezio itself:

```sh
make install
make deploy IMG="${MANAGER_IMG}"
kubectl label --overwrite ns kezio-system pod-security.kubernetes.io/enforce=privileged
kubectl rollout status deployment/kezio-controller-manager -n kezio-system --timeout=180s
```

The PSA label is required, not a shortcut: bootd's dnsmasq child needs
`NET_ADMIN` and `NET_RAW`, which are outside both the `baseline` and
`restricted` profiles. Every namespace that will hold a `Subnet` needs
this label.

### 5.2 The image upload service, ingest RBAC, and the tracker

The image-service reads a bearer token from a Secret you create out of
band - there is no safe placeholder for it in version control:

```sh
export IMAGE_SERVICE_TOKEN="$(openssl rand -hex 32)"
kubectl -n kezio-system create secret generic kezio-image-service-token \
  --from-literal=token="${IMAGE_SERVICE_TOKEN}" \
  --dry-run=client -o yaml | kubectl apply -f -
```

Point the kustomizations at your pinned images and apply them:

```sh
(cd config/image-service && ../../bin/kustomize edit set image "image-service=${IMAGE_SERVICE_IMG}")
(cd config/seeder && ../../bin/kustomize edit set image "seeder=${SEEDER_IMG}")

bin/kustomize build config/image-service | kubectl apply -f -
bin/kustomize build config/ingest        | kubectl apply -f -
bin/kustomize build config/seeder        | kubectl apply -f -
```

Relax the staging claim to ReadWriteOnce, because `local-path` serves
nothing else on a single node:

```sh
kubectl -n kezio-system patch pvc kezio-image-service-staging --type=merge \
  -p '{"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"local-path"}}'
```

If the claim is already Bound with the wrong mode, delete and re-apply
it: `accessModes` is immutable after binding.

### 5.3 The boot config and agent registration Services

Both servers run inside the controller-manager process. Their
kustomizations compose `config/default` (they patch the manager
Deployment), so apply them instead of, not beside, it:

```sh
bin/kustomize build config/bootserver  | kubectl apply -f -
bin/kustomize build config/agentserver | kubectl apply -f -
```

Do this before section 5.4. Both builds re-apply the manager
Deployment, so applying them again later drops the environment that
section sets, and you must repeat it.

### 5.4 Wire the controller-manager

One `kubectl set env` turns on every optional feature. `192.0.2.2` is
bootd's address from section 1 - every machine on the segment reaches
the boot and agent APIs through bootd's own reverse proxy, so it is the
only address the target VM ever needs:

```sh
kubectl -n kezio-system set env deployment/kezio-controller-manager \
  INGEST_IMAGE="${INGEST_IMG}" \
  INGEST_STAGING_PVC=kezio-image-service-staging \
  INGEST_SERVICE_ACCOUNT=kezio-ingest \
  SEEDER_TRACKER_URL="http://192.0.2.4:6969/announce" \
  SEEDER_DEPLOYMENT_IMAGE="${SEEDER_IMG}" \
  BOOT_SERVER_ADDR=:8090 \
  BOOT_ARTIFACTS_DIR=/boot-artifacts \
  BOOT_SERVER_URL="http://192.0.2.2:80" \
  AGENT_SERVER_ADDR=:8091 \
  BOOT_AGENT_SERVER_URL="http://192.0.2.2:80" \
  DEPLOYER=agent \
  BOOTD_DEPLOYMENT_IMAGE="${BOOTD_IMG}" \
  BOOTD_DEPLOYMENT_BOOT_ARTIFACTS_IMAGE="${BOOT_ARTIFACTS_IMG}" \
  BOOTD_DEPLOYMENT_AGENT_UPSTREAM_URL="http://kezio-agent-server.kezio-system.svc.cluster.local:8091" \
  BOOTD_DEPLOYMENT_BOOT_UPSTREAM_URL="http://kezio-boot-server.kezio-system.svc.cluster.local:8090" \
  BOOTD_DEPLOYMENT_SERVICE_ACCOUNT=kezio-bootd

kubectl -n kezio-system set image deployment/kezio-controller-manager \
  fetch-boot-artifacts="${BOOT_ARTIFACTS_IMG}"

kubectl rollout status deployment/kezio-controller-manager -n kezio-system --timeout=180s
```

Two of these are easy to get wrong:

- `SEEDER_TRACKER_URL` must be opentracker's own address on the
  provisioning segment, never a ClusterIP. A ClusterIP DNATs the
  announce, and BitTorrent needs the announced address to be the
  reachable one.
- `BOOT_AGENT_SERVER_URL` must be set, not left to fall back to
  `BOOT_SERVER_URL`. They front two different container ports. When it
  is wrong, the agent's registration POST 404s forever and the Machine
  stays in Inspecting with nothing in the manager log to explain it.

## 6. Attach the segment and create the Site and Subnet

### 6.1 The NetworkAttachmentDefinitions

Three NADs on the one bridge, one per pod that needs a fixed address.
The `static` IPAM plugin gives every attaching pod the same address, so
each address needs its own NAD:

```sh
for spec in kezio-boot-network:192.0.2.2/24 \
            kezio-tracker-network:192.0.2.4/24 \
            kezio-seeder-network:192.0.2.5/24; do
  name="${spec%%:*}"; addr="${spec#*:}"
  cat <<EOF | kubectl apply -f -
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: ${name}
  namespace: kezio-system
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "${name}",
      "plugins": [
        {
          "type": "bridge",
          "bridge": "kezio-prov0",
          "ipam": {
            "type": "static",
            "addresses": [{"address": "${addr}"}]
          }
        }
      ]
    }
EOF
done
```

One static seeder address is correct only while the Site deploys one
Image at a time. A Site that deploys several Images at once needs one
address per concurrent (Image, Site) seeder Deployment - move to the
`whereabouts` pool in
`config/seeder/networkattachmentdefinition-whereabouts.example.yaml`,
and deploy its `ip-reconciler` CronJob with it. The controller raises a
`SeederStaticMultiImage` condition on the Subnet when you cross that
line.

### 6.2 Put opentracker on the segment

`config/seeder` ships the annotation commented out, because the network
name is site-specific:

```sh
kubectl -n kezio-system patch deployment/kezio-opentracker --type=strategic -p='{
  "spec":{"template":{"metadata":{"annotations":{"k8s.v1.cni.cncf.io/networks":"kezio-tracker-network"}}}}
}'
kubectl -n kezio-system rollout status deployment/kezio-opentracker --timeout=180s
curl -s -o /dev/null -w '%{http_code}\n' http://192.0.2.4:6969/announce
```

### 6.3 bootd's RBAC, the Site, and the Subnet

```sh
kubectl apply -k config/bootd
```

That gives `kezio-system` the `kezio-bootd` ServiceAccount and the
ClusterRole it binds to. A Subnet in any other namespace needs its own
`kezio-bootd` ServiceAccount there, bound to the same ClusterRole, plus
the PSA label from section 5.1.

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: kezio.kojuro.date/v1alpha1
kind: Site
metadata:
  name: lab
  namespace: kezio-system
spec:
  seederSubnetRef:
    name: lab-prov
---
apiVersion: kezio.kojuro.date/v1alpha1
kind: Subnet
metadata:
  name: lab-prov
  namespace: kezio-system
spec:
  siteRef:
    name: lab
  cidr: 192.0.2.0/24
  bootdServerIP: 192.0.2.2
  bootdNetworkRef:
    name: kezio-boot-network
  seederNetworkRef:
    name: kezio-seeder-network
  dhcp:
    mode: lease
    leaseRangeStart: 192.0.2.10
    leaseRangeEnd: 192.0.2.100
EOF
```

`mode: lease` makes bootd's own dnsmasq the segment's DHCP authority.
That fits this lab, because `vmbr1` has no DHCP server of its own. The
MAC gate does not relax: only an enrolled Machine's
`spec.bootMACAddress` gets a lease, and every other device on the
segment gets nothing.

Use `mode: proxy` instead when the segment already has a DHCP server -
that is the shape kezio's own end-to-end CI lane exercises, while lease
mode is proven at packet level only
(`hack/bootd-packet-lab.sh`). See `docs/physical-lab-deployment.md`
section 1 for the full comparison.

`SubnetReconciler` builds the bootd Deployment from this Subnet. Wait
for it, then prove the reverse proxy answers over the segment:

```sh
kubectl -n kezio-system rollout status deployment/kezio-bootd-lab-prov --timeout=180s
curl -s -o /dev/null -w '%{http_code}\n' http://192.0.2.2:80/boot/artifacts/manifest.json
```

If the Deployment never appears, the Subnet itself says why:

```sh
kubectl -n kezio-system get subnet lab-prov -o jsonpath='{.status.conditions}' | jq .
```

`BootdNamespacePSALabelMissing` and `BootdServiceAccountMissing` are the
two prerequisites the controller checks for you.

## 7. Ingest an image

Any bootable disk image works: a `qcow2`, `raw`, or `vmdk` file. A cloud
image such as
`https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img`
is a good first one.

The image-service Service is ClusterIP, so port-forward it from the
node:

```sh
kubectl -n kezio-system port-forward svc/kezio-image-service 18080:8080 &
bin/kezioctl image upload ./ubuntu-24.04-minimal-cloudimg-amd64.img \
  --name lab-ubuntu \
  --namespace kezio-system \
  --server http://127.0.0.1:18080 \
  --token "${IMAGE_SERVICE_TOKEN}" \
  --format qcow2
```

The upload starts an ingest Job that converts the image, reads its
`sfdisk` layout into an `ImageLayout`, writes one PVC per partition, and
builds a `.torrent` inside each one. Watch it:

```sh
kubectl -n kezio-system get image lab-ubuntu -w
kubectl -n kezio-system logs -l job-name --tail=50
```

Wait until the Image reports `Ready`. No seeder runs yet - a seeder
Deployment exists only while a Machine at that Site is deploying that
Image.

**The image must already carry its own fallback bootloader** at
`\EFI\BOOT\BOOTX64.EFI` on its EFI System Partition. kezio-agent writes
a UEFI NVRAM entry after the deploy, but it never edits the deployed
file system, and firmware falls back to that fixed path whenever the
NVRAM entry does not survive. Ubuntu cloud images ship it. For an image
that does not, use the `install-removable-fallback` builtin `PostHook`
step.

## 8. Enroll the target machine

Store the Proxmox API token as a BMC credentials Secret:

```sh
kubectl -n kezio-system create secret generic lab-target-1-bmc \
  --type=kubernetes.io/basic-auth \
  --from-literal=username='kezio@pve!kezio' \
  --from-literal=password='<token-secret>'
```

Then create the Machine:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: kezio.kojuro.date/v1alpha1
kind: Machine
metadata:
  name: lab-target-1
  namespace: kezio-system
  annotations:
    kezio.kojuro.date/bmc-insecure-skip-verify: "true"
spec:
  bmc:
    address: redfish://10.0.0.10:8443/redfish/v1/Systems/501
    credentialsSecretRef:
      name: lab-target-1-bmc
  bootMACAddress: "52:54:00:be:ef:01"
  subnetRef:
    name: lab-prov
  online: true
  imageRef:
    name: lab-ubuntu
  targetDisk:
    serialNumber: "KEZIOLAB0001"
EOF
```

Three fields carry lab-specific detail:

- **The BMC path is explicit.** One proxmox-redfish daemon fronts every
  VM on the host, so its `Systems` collection has many members.
  `resolveSystem` refuses to guess among them and needs
  `/redfish/v1/Systems/<vmid>`.
- **The insecure annotation is needed** while proxmox-redfish uses its
  installer's self-signed certificate. It is an annotation, not a spec
  field, because it is a transport-trust decision, not deployment
  intent. Any value other than exactly `"true"` means verify. Drop the
  annotation once you give the daemon a certificate the cluster trusts.
- **`subnetRef` decides the Site.** A Machine never names its Site:
  kezio derives it as `subnetRef -> Subnet.spec.siteRef -> Site`, so a
  machine always leeches from the seeder at the segment its boot NIC is
  really wired to.

The MAC must match the target VM's `net0` MAC exactly. bootd answers
nothing to a MAC that is not an enrolled Machine's.

## 9. Watch the deploy

kezio powers the VM on through the BMC. Nothing else needs to touch it:

```sh
kubectl -n kezio-system get machine lab-target-1 -w
```

The Machine passes through Enrolling, Inspecting, Provisioning, and
Provisioned. What happens behind each step:

1. The controller sets a one-time PXE boot and powers the VM on.
2. bootd answers the DHCP and PXE request, and serves shim and GRUB over
   TFTP.
3. GRUB fetches its config from the boot config server through bootd's
   proxy, then the kernel, initrd, and squashfs.
4. kezio-agent boots in the live environment and registers with the
   agent server.
5. The controller starts an `ezio-seeder` Deployment for this (Image,
   Site) pair, and hands the agent a deploy plan.
6. The agent leeches each partition over BitTorrent, writes it with
   partclone, replays the `sfdisk` layout, runs any `PostHook` steps,
   and writes the UEFI boot entry.
7. The controller power-cycles the machine into the deployed disk.

Useful views while it runs:

```sh
kubectl -n kezio-system logs -f deployment/kezio-controller-manager
kubectl -n kezio-system logs -f deployment/kezio-bootd-lab-prov
kubectl -n kezio-system get deployment -l app.kubernetes.io/component=seeder
```

Watch the VM's own console at the same time, in the Proxmox web UI
(VM 501 -> Console). It shows the PXE exchange, GRUB, and the live
environment's boot, which is where a boot-path failure appears first.

The seeder Deployment disappears again a few minutes after the last
Machine stops deploying that Image. That is the designed behavior, not a
failure.

## 10. Confirm the result

```sh
kubectl -n kezio-system get machine lab-target-1 \
  -o jsonpath='{.status.phase}{"\n"}{.status.conditions}' | jq .
```

Then check the VM itself: the QEMU Guest Agent connects once the
deployed OS is up (Proxmox shows the guest's IP address on the VM
summary page), and the console shows the deployed system's login prompt
rather than the live environment's.

## 11. Add a second machine, or a second site

- **Another machine on the same segment**: create its VM (section 2.3),
  its BMC Secret, and its Machine (section 8). Nothing else changes.
  Watch the seeder address pool if two Images can deploy at once
  (section 6.1).
- **Another segment or site**: create the bridge and its NADs, a
  `Subnet` with its own `cidr` and `bootdServerIP`, and - if the
  segment is behind a router or firewall - its own `Site`. Give the new
  namespace, if you use one, the PSA label and a `kezio-bootd`
  ServiceAccount. `SubnetReconciler` builds that segment's bootd for
  you, and it proxies to the same cluster-wide boot and agent servers.

Remember what a Site means: every Subnet inside one Site is mutually
routable. Anything behind a barrier that breaks reachability is a
different Site, and a Site that deploys Images with content needs its
own seeder Subnet.

## 12. What this lab does not match in production

- **The Redfish shim is not a BMC.** proxmox-redfish drives the Proxmox
  API. It is fine for the lab, and it exercises kezio's real
  `redfish://` driver, but its behavior is its own, not a vendor BMC's.
- **A one-time PXE boot is not really one-time.** The shim implements
  `BootSourceOverrideTarget: Pxe` by moving the network device to the
  front of the VM's Proxmox boot order, and that change stays. A
  deployed machine therefore PXE-boots again on the next reboot, gets
  the "boot local" GRUB config (kezio hands that back to any machine
  that does not need the live environment right now), and falls through
  to its disk. It works, but it costs one PXE round trip per boot. Set
  the boot order back to the disk in Proxmox when that matters.
- **Secure Boot is off.** The shim and GRUB kezio serves are
  Debian-signed, so a machine can keep Secure Boot on through the whole
  chain, and the kernel-signing choice is what decides it. No CI lane
  exercises it. See `docs/secure-boot.md` before you enable it, and give
  the VM an EFI disk with pre-enrolled keys at that point.
- **Storage is single-node.** `local-path` and the ReadWriteOnce patch
  in section 5.2 work because everything runs on one node. A real
  cluster wants ReadWriteMany for the staging claim, as
  `config/image-service/pvc.yaml` ships it.
- **The seeder address is static.** One address per NAD suits one Image
  at a time. See section 6.1.
- **The certificate is self-signed.** Section 8's annotation is a
  per-Machine escape hatch, not the production shape.

## 13. When something does not work

| Symptom | Where to look |
|---|---|
| No bootd Deployment appears | `kubectl get subnet lab-prov -o yaml`, `status.conditions`: the PSA label and the `kezio-bootd` ServiceAccount are both checked there. |
| The VM shows PXE-E16 or PXE-E18 and never boots | The FORWARD accept and the checksum rule in section 3.4. Then confirm the bootd pod's `k8s.v1.cni.cncf.io/network-status` annotation really lists `kezio-boot-network`. |
| The VM gets no DHCP answer at all | The MAC gate. `spec.bootMACAddress` must match the VM's NIC exactly. bootd's log names every MAC it refuses. |
| GRUB loads, then nothing | `curl http://192.0.2.2:80/boot/artifacts/manifest.json` from the node. A failure there is the bootd proxy or `BOOT_SERVER_URL`. |
| The Machine stays in Inspecting | `BOOT_AGENT_SERVER_URL` (section 5.4), and `DEPLOYER=agent`. |
| The agent waits forever for a plan | `SEEDER_TRACKER_URL` must be unset only if the Image has no content. Otherwise the plan endpoint answers `wait` forever. |
| The leech never finishes | Ports `6969` (tracker) and `16881` (seeder) must be reachable from the target, with no NAT rewriting any address on the path. |
| The BMC call fails | The controller-manager log, filtered for `redfish`. A `found N systems` error means the address needs its `/redfish/v1/Systems/<vmid>` path; a TLS error means the annotation in section 8; a 401 means the token ACL in section 2.5. |
| The deployed disk does not boot | The fallback bootloader contract in section 7. |
