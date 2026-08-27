# Lab walkthrough: RKE2 on Proxmox VE, with a Redfish BMC shim

This guide builds one complete kezio lab, in order. It starts with an
empty Proxmox VE host. It ends with a target VM that receives an image
and boots it. The lab uses Proxmox VMs in place of real servers, and a
Redfish shim in front of the Proxmox API in place of a real BMC.

Read [`docs/crd-reference.md`](crd-reference.md) first. It describes the
ten custom resources and how they refer to each other. Read
[`docs/network-model.md`](network-model.md) for the Site/Subnet model.
[`docs/physical-lab-deployment.md`](physical-lab-deployment.md) is the
reference guide: it gives every option and every port. This guide is
different - it makes one choice at each point, and tells you what to
type.

Most steps here have an equivalent composite action in
`.github/actions/`. Those actions run on every push, so they are the
source of truth. Where this guide and an action disagree, the action is
correct. This guide names the related action at each step, so you can
compare.

The lab is one Site with one Subnet. The same steps work for more
Subnets and more Sites.

**kezio does not configure your network.** The baseline is L3: you own
reachability between segments, and you own any DHCP relay. kezio places
its pods on the segments you name, and nothing more.

## 1. What the lab looks like

Three parts run on one Proxmox VE host:

- **`kezio-node`** - one VM. It runs RKE2 and every kezio service.
- **`kezio-target-1`** - one VM. It is the "bare-metal machine" that
  kezio deploys an OS onto.
- **proxmox-redfish** - a daemon on the Proxmox host. It gives each VM a
  Redfish endpoint, so kezio can set the power state and a one-time PXE
  boot. kezio has no mode that deploys a machine without a BMC.

Two bridges carry the traffic:

| Bridge | Use |
|---|---|
| `vmbr0` | Management and uplink. `kezio-node` reaches the internet and the Proxmox API through it. |
| `vmbr1` | The provisioning segment. It has no uplink port and no host address. `kezio-target-1` has only this network. |

The addresses on the provisioning segment. This guide uses
`192.0.2.0/24`. Replace it with your own range. The layout is the same
one `.github/actions/create-provisioning-nads` uses:

| Address | Owner |
|---|---|
| `192.0.2.1` | The address of `kezio-node` on the segment. It is for your own `curl` checks. |
| `192.0.2.2` | bootd (`Subnet.spec.bootdServerIP`) |
| `192.0.2.10` - `192.0.2.100` | The lease range that bootd gives out |
| `192.0.2.101` - `192.0.2.116` | The seeder address pool (`host-local` IPAM) |
| `192.0.2.116` | The tracker (`Site.spec.tracker.ip`), at the top of that pool |

The tracker address must be **inside** the seeder pool, and at the top
of it. Section 6.1 tells you why.

The `kezio-node` VM needs a minimum of 8 vCPU, 16 GiB RAM, and 200 GiB
of disk. Each import writes a full copy of the image into PVCs, so give
the disk sufficient space for every image that you import.

Every object in this guide is in the `kezio-system` namespace.
`config/default` sets that namespace and a `kezio-` name prefix, and
`config/image-service` and `config/bootd` name the same namespace. The
controller-manager Deployment is therefore
`kezio-controller-manager`.

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

Apply it with `ifreload -a`, or reboot. Give the bridge no address and
no port. Nothing outside the lab must reach this segment, and bootd
becomes the DHCP authority of the segment in section 6.

Keep the Proxmox firewall off for the NICs of both VMs on `vmbr1`. It is
off by default. The `kezio-node` VM forwards frames for pod MAC
addresses that are not the MAC address of its own NIC, and a firewall on
that port discards them.

### 2.2 Create the `kezio-node` VM

Create a VM with Debian 13 or Ubuntu 24.04, and two NICs:

- `net0` on `vmbr0` - management, with DHCP or a static address.
- `net1` on `vmbr1` - the provisioning segment, with no address of its
  own yet. Section 3.1 puts it into a bridge inside the VM.

### 2.3 Create the `kezio-target-1` VM

This VM is a substitute for a bare-metal machine. Build it in the way
that a real machine boots:

| Setting | Value | Why |
|---|---|---|
| BIOS | OVMF (UEFI) | The boot chain of kezio is shim -> GRUB -> live kernel. All of it is UEFI. |
| EFI disk | Added, **without** pre-enrolled keys | Secure Boot is off for the first run. See section 12. |
| Machine | q35 | It agrees with OVMF. |
| Network | `net0` on `vmbr1` only | The machine boots from this segment. Write down its MAC address. |
| Disk | SCSI, a minimum of 32 GiB, with a serial | kezio selects the disk by the serial. |
| Boot order | Any order that includes `net0` | proxmox-redfish changes the order of the existing list, so `net0` must be in it. It does not need to be first: the daemon moves it to the front itself. |
| QEMU Guest Agent | Enabled | It lets you confirm that the deployed OS booted. The deployed image must also contain the agent - section 7 puts it there. |

Set the disk serial on the Proxmox host. The web UI has no field for it.
`501` is the VM ID that this guide uses:

```sh
qm set 501 --scsi0 local-lvm:vm-501-disk-0,serial=KEZIOLAB0001
```

Keep the VM powered off. kezio powers it on through the BMC.

### 2.4 Install proxmox-redfish on the Proxmox host

[v1k0d3n/proxmox-redfish](https://github.com/v1k0d3n/proxmox-redfish) is
a Redfish API in front of the Proxmox API. It answers the calls that
`internal/bmc/redfish` makes: `ComputerSystem.Reset` for power, and a
`Boot` PATCH for the one-time PXE boot.

Install it from source, as root. The project also supplies
`scripts/install.sh`, but do not use it. Its version test compares
`3.11 >= 3.8` as a decimal number, so it rejects every Python version
that is more recent than 3.9 with "Python 3.8 or higher is required".
Proxmox VE 8 has Python 3.11 and Proxmox VE 9 has Python 3.13, so the
test fails on both.

```sh
apt install -y python3 python3-pip python3-venv git openssl

git clone https://github.com/v1k0d3n/proxmox-redfish.git /opt/proxmox-redfish
cd /opt/proxmox-redfish
python3 -m venv venv
venv/bin/pip install -e .
```

Write `/opt/proxmox-redfish/config/params.env` with the values of your
own host:

```sh
PROXMOX_HOST="10.0.0.10"             # the management address of this host
PROXMOX_USER="root@pam"
PROXMOX_PASSWORD="<the password of root>"
PROXMOX_API_PORT="8006"
PROXMOX_NODE="pve"                   # the host name of this node
VERIFY_SSL="false"
```

There are three ways to make this file incorrect. Each one fails
differently:

- **Write literal values, not shell substitutions.** systemd reads this
  file as an `EnvironmentFile`, and an `EnvironmentFile` never runs a
  command. A `$(hostname)` in it reaches the daemon as those exact
  characters. The upstream README writes this file with a quoted
  heredoc, which puts those characters in the file instead of their
  output.
- **`PROXMOX_USER` and `PROXMOX_PASSWORD` must name a real user and its
  password, never an API token.** These are the credentials that the
  daemon itself uses against the Proxmox API. It authenticates with them
  through `/api2/json/access/ticket`, which rejects a token with
  `authentication failure`. The API token in section 2.5 is a different
  credential, for a different direction.
- **An empty `PROXMOX_HOST` is not an error at start-up.** The daemon
  uses the literal string `pve-node-hostname` instead, serves
  `/redfish/v1/` correctly, and fails only on the first request that
  needs the Proxmox API. The check in section 2.5 finds this.

Then write `/etc/systemd/system/proxmox-redfish.service`. This unit
serves plain HTTP on port `8000`. That is the port and the scheme that
the rest of this guide uses. Basic credentials therefore cross the
management network in base64. This is acceptable for a lab on a trusted
segment. It is not acceptable anywhere else - see the Administrators
Guide of the project for the TLS options.

```ini
[Unit]
Description=Proxmox Redfish Daemon
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/proxmox-redfish
EnvironmentFile=/opt/proxmox-redfish/config/params.env
ExecStart=/opt/proxmox-redfish/venv/bin/python \
  /opt/proxmox-redfish/src/proxmox_redfish/proxmox_redfish.py --port 8000
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload
systemctl enable --now proxmox-redfish
systemctl status proxmox-redfish
```

If you change the port here, you must change it everywhere this guide
writes `8000`: the `curl` check in section 2.5, and the BMC address of
the Machine in section 8.

### 2.5 Create the Proxmox credentials that kezio uses

kezio connects with HTTP Basic authentication.
`internal/bmc/redfish` sets `BasicAuth: true`, because a controller
connects many times each hour and most BMCs limit the number of
concurrent Redfish sessions. proxmox-redfish accepts a Proxmox API token
in that Basic header.

**Two different credentials are in use, in two directions.** Keep them
apart:

| Direction | Credential | Where it goes |
|---|---|---|
| kezio -> proxmox-redfish | The API token below | The Secret in section 8 |
| proxmox-redfish -> Proxmox API | `PROXMOX_USER` and `PROXMOX_PASSWORD` | `params.env`, section 2.4 |

The daemon validates the token that comes in. Then it does each VM
operation as `PROXMOX_USER`. If you put the token in `params.env`, or
the root password in the Secret, the error message names the other one.

In the Proxmox web UI:

1. Datacenter -> Permissions -> Users -> Add: user `kezio@pve`.
2. Datacenter -> Permissions -> API Tokens -> Add: token ID `kezio` on
   that user. Copy the secret. Proxmox shows it one time only.
3. Datacenter -> Permissions -> Add -> **API Token Permission**: path
   `/vms`, token `kezio@pve!kezio`, role `PVEVMAdmin`.

Step 3 is not the same as a user permission. An API token gets no rights
from the user behind it while privilege separation is on, and a token
with no ACL entry of its own is rejected. Give the token its own entry,
or set privilege separation off on the token.

Test the endpoint before you continue. Use a real VM ID:

```sh
curl -u 'kezio@pve!kezio:<token-secret>' \
  http://10.0.0.10:8000/redfish/v1/Systems/501 | jq '.PowerState, .Name'
```

Use `http`, not `https`. The unit in section 2.4 serves plain HTTP. Read
what each result means before you continue:

- **`PowerState` and the name of the VM** - both directions work.
- **`Invalid Basic Authentication credentials`** - the token is
  rejected. Examine step 3, or the secret.
- **The connection closes with no response** - the token is correct and
  the credentials of the daemon are not. `journalctl -u proxmox-redfish`
  gives the reason. `Couldn't authenticate user ... /api2/json/access/ticket`
  means that `params.env` holds a token where a real user must be.
- **`/redfish/v1/` answers but `/redfish/v1/Systems/<id>` does not** -
  the same cause. The service root needs no connection to Proxmox, so it
  continues to answer long after the remainder has stopped.

## 3. Install RKE2 on `kezio-node`

Every command from here runs inside the `kezio-node` VM.

### 3.1 Put the provisioning NIC into a bridge

Multus attaches pods to the provisioning segment through the `bridge`
CNI plugin. The `net1` interface of the VM must therefore be a port of a
Linux bridge inside the VM. With netplan
(`/etc/netplan/60-kezio-prov.yaml`):

```yaml
network:
  version: 2
  ethernets:
    ens19:            # the net1 interface - examine `ip link`
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

Apply it with `netplan apply`. Then confirm that `ip -br addr show
kezio-prov0` reports the address and `UP`.

Keep the bridge name at 15 characters or fewer. The kernel rejects a
longer name, and the failure shows much later as a CNI "numerical result
out of range" on the first pod that attaches.

The address `192.0.2.1` is for your own checks from the node. It is not
a gateway. Each kezio service gets its own address on the segment.

### 3.2 Install RKE2 with Multus

The version and the configuration below are those of
`.github/actions/install-rke2`:

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

`multus` must be first in the `cni` list. `canal` stays the pod network.
Put the `KUBECONFIG` export into the profile of your shell.

### 3.3 Install storage that gives ReadWriteMany

RKE2 supplies no default StorageClass. kezio needs one, and the PVCs
that it creates ask for ReadWriteMany by default:

- The staging PVC of the image-service (`config/image-service/pvc.yaml`).
- The scratch PVC of each ImageImport
  (`ImageIngestConfig.ScratchAccessModes`).
- The content PVC of each PartitionContent.

ReadWriteMany is the production default, and this lab keeps it. A
plain `local-path-provisioner` cannot give ReadWriteMany at all.
`.github/actions/install-nfs-storage` solves this with an in-cluster NFS
server and the `csi-driver-nfs` CSI driver, and every deploy lane of
kezio uses it. Do the same here.

Install the driver:

```sh
CSI_NFS_VERSION=v4.13.4
base="https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/${CSI_NFS_VERSION}/deploy/${CSI_NFS_VERSION}"
kubectl apply -f "${base}/rbac-csi-nfs.yaml"
kubectl apply -f "${base}/csi-nfs-driverinfo.yaml"
kubectl apply -f "${base}/csi-nfs-controller.yaml"
kubectl apply -f "${base}/csi-nfs-node.yaml"
kubectl -n kube-system rollout status deployment/csi-nfs-controller --timeout=300s
kubectl -n kube-system rollout status daemonset/csi-nfs-node --timeout=300s
```

Deploy the NFS server that the driver mounts from:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nfs-server
  namespace: default
  labels:
    app: nfs-server
spec:
  selector:
    app: nfs-server
  ports:
    - {name: tcp-2049, port: 2049, protocol: TCP}
    - {name: udp-111, port: 111, protocol: UDP}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nfs-server
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: nfs-server
  template:
    metadata:
      labels:
        app: nfs-server
    spec:
      containers:
        - name: nfs-server
          image: itsthenetwork/nfs-server-alpine:12
          env:
            - name: SHARED_DIRECTORY
              value: "/exports"
          volumeMounts:
            - {mountPath: /exports, name: nfs-vol}
          securityContext:
            privileged: true
          ports:
            - {name: tcp-2049, containerPort: 2049, protocol: TCP}
            - {name: udp-111, containerPort: 111, protocol: UDP}
      volumes:
        - name: nfs-vol
          hostPath:
            path: /nfs-vol
            type: DirectoryOrCreate
EOF
kubectl -n default rollout status deployment/nfs-server --timeout=300s
```

Create the StorageClass and make it the cluster default:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nfs
  annotations:
    storageclass.kubernetes.io/is-default-class: "true"
provisioner: nfs.csi.k8s.io
parameters:
  server: nfs-server.default.svc.cluster.local
  share: /
  mountPermissions: "0777"
reclaimPolicy: Delete
volumeBindingMode: Immediate
EOF
```

`mountPermissions: "0777"` is necessary here, and it is a lab choice
only. Every Job pod of kezio runs as the fixed non-root UID 65532. The
driver leaves a new subdirectory with permissions that this UID cannot
write, and `fsGroup` has no result on an NFS volume. Use a real NFS
export with correct ownership in production.

The `/nfs-vol` directory on the node holds every byte of every image.
Keep the disk of the node sufficiently large.

### 3.4 Let PXE traffic cross the bridge

Two changes at host level. Make them after RKE2 starts, and before any
pod attaches to `kezio-prov0`. They are the two steps of
`.github/actions/harden-provisioning-bridge`:

```sh
# br_netfilter of canal sends bridged frames through FORWARD. The policy
# of that chain is DROP after the cluster starts, and the PXE
# DHCPDISCOVER is lost there.
if command -v iptables >/dev/null 2>&1; then
  sudo iptables -C FORWARD -i kezio-prov0 -o kezio-prov0 -j ACCEPT 2>/dev/null || \
    sudo iptables -I FORWARD -i kezio-prov0 -o kezio-prov0 -j ACCEPT
fi
if command -v nft >/dev/null 2>&1 && sudo nft list table inet filter >/dev/null 2>&1; then
  sudo nft list chain inet filter FORWARD 2>/dev/null | grep -q 'iifname "kezio-prov0" oifname "kezio-prov0" accept' || \
    sudo nft insert rule inet filter FORWARD iifname "kezio-prov0" oifname "kezio-prov0" accept
fi

# virtio-net leaves the UDP checksums to the hardware. The hop from the
# bridge to the pod veth never fills them in, and the kernel then
# discards the DHCP datagram.
if command -v iptables >/dev/null 2>&1; then
  for port in 67 68; do
    sudo iptables -t mangle -C POSTROUTING -o kezio-prov0 -p udp --dport "$port" -j CHECKSUM --checksum-fill 2>/dev/null || \
      sudo iptables -t mangle -A POSTROUTING -o kezio-prov0 -p udp --dport "$port" -j CHECKSUM --checksum-fill
  done
else
  sudo ethtool -K kezio-prov0 tx off || true
fi
```

`nft` has no `CHECKSUM` target. On a node with `nftables` only,
`ethtool -K kezio-prov0 tx off` is the less precise substitute, as the
last branch above shows.

Both changes are lost at reboot. Make them permanent with
`iptables-persistent`, or with a systemd unit, before you leave the lab
in operation.

## 4. Get the kezio images

kezio publishes six images: `kezio`, `kezio-ingest`, `kezio-seeder`,
`kezio-image-service`, `kezio-bootd`, and `kezio-boot-artifacts`. All six
get a `main` tag on each push to `main`, and a tag matching the git tag
verbatim (e.g. `v0.3.2`) on each release tag. The `main` tag of all six
comes from the same commit, because `.github/workflows/main.yaml` pushes
only after each e2e lane has passed. Tags before `v0.3.2` were published
without the leading `v` (e.g. `0.1.8`).

Each release tag matches its own checkout: the CRDs, the
kustomizations, and the environment variables that this guide describes
agree with the images published for that tag. Use `main` for the latest
unreleased state, or pin a release tag such as `v0.3.8` for a reproducible
build.

Clone the repository on the node. The steps below apply kustomizations
from the working tree:

```sh
git clone https://github.com/tjjh89017/kezio.git
cd kezio
make build-kezioctl        # writes bin/kezioctl
make kustomize             # writes bin/kustomize
```

Then name the images:

```sh
export KEZIO_VERSION=main
export MANAGER_IMG=ghcr.io/tjjh89017/kezio:${KEZIO_VERSION}
export IMAGE_SERVICE_IMG=ghcr.io/tjjh89017/kezio-image-service:${KEZIO_VERSION}
export INGEST_IMG=ghcr.io/tjjh89017/kezio-ingest:${KEZIO_VERSION}
export SEEDER_IMG=ghcr.io/tjjh89017/kezio-seeder:${KEZIO_VERSION}
export BOOTD_IMG=ghcr.io/tjjh89017/kezio-bootd:${KEZIO_VERSION}
export BOOT_ARTIFACTS_IMG=ghcr.io/tjjh89017/kezio-boot-artifacts:${KEZIO_VERSION}
```

To build the boot-artifacts image yourself instead, make the live
environment first, then package it:

```sh
hack/live-image/build.sh                # needs Docker; runs live-build privileged
export BOOT_ARTIFACTS_IMG=<your registry>/kezio-boot-artifacts:lab
make docker-build-boot-artifacts BOOT_ARTIFACTS_IMG="${BOOT_ARTIFACTS_IMG}"
```

Push that image to a registry that the cluster can read, or import it
into the containerd of RKE2 directly:

```sh
docker save "${BOOT_ARTIFACTS_IMG}" -o /tmp/boot-artifacts.tar
sudo ctr -n k8s.io images import /tmp/boot-artifacts.tar
```

To build the other five images yourself, use the `docker-build-*` and
`docker-push-*` targets. `make help` lists them.

## 5. Deploy kezio

### 5.1 Install cert-manager

Each CRD of kezio has a validating webhook, and the controller-manager
cannot admit a request until cert-manager has issued its serving
certificate. The version below is the pin of
`.github/actions/install-cert-manager`:

```sh
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.yaml
for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
  kubectl wait deployment.apps/"${d}" --for condition=Available -n cert-manager --timeout=180s
done
```

### 5.2 Make the overlay this lab deploys

`config/default` alone is not sufficient. A lab that net boots a machine
also needs:

- The boot config server (`config/components/boot-server`).
- The agent registration server (`config/components/agent-server`).
- The `fetch-boot-artifacts` initContainer that fills the directory that
  the boot config server serves
  (`config/components/boot-artifacts`).
- The `pod-security.kubernetes.io/enforce=privileged` label on the
  namespace. bootd starts a dnsmasq child that needs `NET_ADMIN`,
  `NET_RAW`, and `NET_BIND_SERVICE`. These are outside both the
  `baseline` and the `restricted` profiles.

Include both servers together. A machine that net boots but cannot
register stays in Inspecting for ever.

`config/netboot-e2e` composes exactly this set, but it pins its
boot-artifacts image to the local tag of the CI lane. Make your own
overlay instead:

```sh
mkdir -p config/lab
cp config/netboot-e2e/namespace-privileged-patch.yaml config/lab/

cat <<'EOF' > config/lab/kustomization.yaml
resources:
  - ../default

components:
  - ../components/boot-server
  - ../components/agent-server
  - ../components/boot-artifacts

patches:
  - path: namespace-privileged-patch.yaml
    target:
      kind: Namespace
      name: kezio-system
EOF
```

Point it at your images, and apply it:

```sh
(cd config/manager && ../../bin/kustomize edit set image "controller=${MANAGER_IMG}")
(cd config/lab && ../../bin/kustomize edit set image \
  "ghcr.io/tjjh89017/kezio-boot-artifacts=${BOOT_ARTIFACTS_IMG}")

make install                                    # the CRDs, always from config/crd
bin/kustomize build config/lab | kubectl apply -f -
kubectl -n kezio-system rollout status deployment/kezio-controller-manager --timeout=180s
```

`config/components/boot-artifacts` renames its own placeholder to
`ghcr.io/tjjh89017/kezio-boot-artifacts` before your overlay runs its
own image transformer. That renamed value is what the `kustomize edit
set image` command above must name.

Confirm that the manager created its default PostHook. If it did not,
the manager container lost its `POD_NAMESPACE` variable, and every
Machine waits for ever with nothing in the log to explain it:

```sh
kubectl wait --for=create posthook/kezio-default-finalize -n kezio-system --timeout=60s
```

Each **other** namespace that will hold a `Subnet` needs the same
`pod-security.kubernetes.io/enforce=privileged` label.

### 5.3 Deploy the image upload service

The image-service reads a bearer token from a Secret that you create
yourself. There is no safe placeholder for it in version control:

```sh
export IMAGE_SERVICE_TOKEN="$(openssl rand -hex 32)"
kubectl -n kezio-system create secret generic kezio-image-service-token \
  --from-literal=token="${IMAGE_SERVICE_TOKEN}" \
  --dry-run=client -o yaml | kubectl apply -f -
```

The Deployment does not become Ready until this Secret exists. Then:

```sh
(cd config/image-service && ../../bin/kustomize edit set image "image-service=${IMAGE_SERVICE_IMG}")
bin/kustomize build config/image-service | kubectl apply -f -
kubectl -n kezio-system rollout status deployment/kezio-image-service --timeout=180s
```

This creates the `kezio-image-service-staging` PVC, at 100Gi and
ReadWriteMany. The `nfs` StorageClass from section 3.3 satisfies both.
To change the size, overlay the PVC. Do not change
`config/image-service` itself.

`config/manager/manager.yaml` already points the manager at this
Service (`IMAGE_INGEST_IMAGE_SERVICE_URL`) and at this Secret's
`token` key (`IMAGE_INGEST_IMAGE_SERVICE_TOKEN`), so it can size a
`kezio-staged://` import's scratch volume from the real upload size.
Since section 5.2 rolled the manager out before this Secret existed,
restart it now so the token actually reaches the running container:
`kubectl -n kezio-system rollout restart deployment/kezio-controller-manager`.

There is no separate kustomization for ingest and no separate
kustomization for the seeder. The ingest Job, the publish Job and the
seeder Deployment are all built by controllers, from the images that
section 5.4 configures.

There is also no cluster-wide tracker. The Site reconciler builds one
tracker Deployment for each Site that sets `spec.tracker.ip`.
`config/opentracker` remains only for an operator who runs a tracker
independently of any Site and points `spec.tracker.externalURL` at it.
This lab does not use it.

### 5.4 Wire the controller-manager

Each optional feature is off until an environment variable turns it on.
The set below is the set that `.github/workflows/main.yaml` gives to
`deploy-kezio` for its deploy lane.

`192.0.2.2` is the address of bootd - the Subnet's own `bootdServerIP`
(section 6). Each machine on the segment reaches the boot API and the
agent API through the reverse proxy of bootd, so it is the only address
that the target VM ever needs; the manager derives it from the Subnet
itself, so no `BOOT_SERVER_URL` / `BOOT_AGENT_SERVER_URL` is set below.

```sh
kubectl -n kezio-system set env deployment/kezio-controller-manager \
  DEPLOYER=agent \
  BOOT_SERVER_ADDR=:8090 \
  AGENT_SERVER_ADDR=:8091 \
  BOOTD_DEPLOYMENT_IMAGE="${BOOTD_IMG}" \
  BOOTD_DEPLOYMENT_BOOT_ARTIFACTS_IMAGE="${BOOT_ARTIFACTS_IMG}" \
  BOOTD_DEPLOYMENT_BOOT_UPSTREAM_URL="http://boot-server.kezio-system.svc.cluster.local:8090" \
  BOOTD_DEPLOYMENT_AGENT_UPSTREAM_URL="http://agent-server.kezio-system.svc.cluster.local:8091" \
  IMAGE_INGEST_IMAGE="${INGEST_IMG}" \
  IMAGE_INGEST_STAGING_PVC=kezio-image-service-staging \
  PARTITIONCONTENT_PUBLISH_IMAGE="${INGEST_IMG}" \
  PARTITIONCONTENT_SEEDER_IMAGE="${SEEDER_IMG}" \
  PARTITIONCONTENT_SEEDER_GRACE_PERIOD=30s \
  TRACKER_DEPLOYMENT_IMAGE=ghcr.io/tunisiano187/opentracker-docker:master

kubectl -n kezio-system rollout status deployment/kezio-controller-manager --timeout=180s
```

What each group does, and where it is easy to make an error:

- **`DEPLOYER=agent`** selects the real deployer. Unset or `fake`, kezio
  uses `FakeDeployer`, which never touches hardware. Any other value
  stops the manager at start-up.
- **`BOOT_SERVER_ADDR` and `AGENT_SERVER_ADDR`** start the two servers
  inside the manager process. `BOOT_ARTIFACTS_DIR` comes from the
  boot-artifacts component in your overlay, so do not set it here.
- **`BOOT_SERVER_URL` / `BOOT_AGENT_SERVER_URL` are not set above.** The
  boot config server derives each Machine's boot server address from its
  own Subnet's `bootdServerIP` (section 6), so these two env vars are
  needed only as a manager-wide fallback, for a Machine whose Subnet
  declares no boot half. `BOOT_AGENT_SERVER_URL` falls back to
  `BOOT_SERVER_URL` when unset, and the two front different container
  ports; if a fallback is set incorrectly, the registration POST of the
  agent gets 404 for ever, and the Machine stays in Inspecting with
  nothing in the log of the manager to explain it.
- **`BOOTD_DEPLOYMENT_*_UPSTREAM_URL`** are ClusterIP Services, and they
  are correct as such. Only bootd calls them, from inside the cluster.
- **`PARTITIONCONTENT_PUBLISH_IMAGE` is the ingest image**, not the
  seeder image. The publish Job builds the `.torrent` with the same
  tools that ingest uses.
- **`IMAGE_INGEST_SOURCE_FORMAT` is not set above.** It defaults to
  `qcow2`, and ingest rejects a source that is not in the format that
  this variable declares. The Ubuntu cloud image in section 7 is qcow2,
  so the default is correct here. Set it to `raw` if you import raw
  disk images instead. It is a manager-wide setting: an `ImageImport`
  has no format field of its own.
- **`IMAGE_INGEST_IO_BANDWIDTH_BYTES_PER_SEC` is not set above.** It
  defaults to 64Mi/s and caps the write rate of the ingest Job's own
  file copies (the source download and each partition's slice), on top
  of a best-effort low disk/CPU priority for `qemu-img` and `partclone`.
  This keeps one ingest run from starving a node's disk of other work.
  Raise it, or set it to a very large value, on a node whose disk can
  take faster writes.
- **`IMAGE_INGEST_UNPRIVILEGED` is not set above**, so the ingest Job
  runs privileged and attaches the source with `qemu-nbd` by default.
  The node needs the `nbd` kernel module loaded with partition support
  before the first import: `sudo modprobe nbd max_part=16`. Without it,
  the ingest Job fails fast naming the missing `/dev/nbd0`. Set
  `IMAGE_INGEST_UNPRIVILEGED=true` instead if the node cannot load that
  module or the cluster cannot run privileged pods.
- **There is no tracker URL variable.** `TRACKER_DEPLOYMENT_IMAGE` gives
  the Site reconciler the image for the tracker that it runs. The
  announce URL comes from `Site.spec.tracker.ip`, and kezio writes it
  into each `.torrent` that the seeders of the Site serve.

## 6. Attach the segment, and create the Site and the Subnet

### 6.1 The NetworkAttachmentDefinitions

Two NADs on the one bridge. The shapes below are those of
`.github/actions/create-provisioning-nads`:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: kezio-boot-network
  namespace: kezio-system
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "kezio-boot-network",
      "plugins": [
        {
          "type": "bridge",
          "bridge": "kezio-prov0",
          "ipam": {
            "type": "static",
            "addresses": [{"address": "192.0.2.2/24"}]
          }
        }
      ]
    }
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: kezio-seeder-network
  namespace: kezio-system
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "kezio-seeder-network",
      "plugins": [
        {
          "type": "bridge",
          "bridge": "kezio-prov0",
          "ipam": {
            "type": "host-local",
            "subnet": "192.0.2.0/24",
            "rangeStart": "192.0.2.101",
            "rangeEnd": "192.0.2.116"
          }
        }
      ]
    }
EOF
```

The two NADs have different IPAM types on purpose:

- **`kezio-boot-network` is `static`.** bootd is its only user, and
  `bootdServerIP` must be that exact address. Firmware keeps the address
  during the boot, so a new address after a pod restart interrupts a
  TFTP fetch that is in progress. `internal/nadvalidate.CheckBootdAddress`
  reports a Violation for any IPAM type that is not static on this NAD.
- **`kezio-seeder-network` is `host-local` with a range.** This NAD
  holds two kinds of pod: each seeder Deployment of the Site, and the
  tracker of the Site. A `static` NAD gives the same address to each pod
  that attaches, so the tracker and a seeder collide immediately.
  `host-local` gives a different address to each pod.

The tracker keeps its own address with a per-pod override on this same
NAD. `host-local` refuses an address that is outside its own range, and
fails the pod with `failed to allocate all requested IPs`. The tracker
address must therefore be **inside** the range, and at the **top** of
it. `host-local` allocates upward from `rangeStart`, so no seeder pod
takes `192.0.2.116` before the tracker asks for it.

Size the pool for the number of Images that deploy at the same time at
this Site. There is one seeder Deployment for each (`Image`, `Site`)
pair, each with one pod. The rule is: pool size >= (Images that deploy
at the same time) + 1 for the tracker. The pool above holds 15 seeder
addresses and the tracker address, which is more than sufficient for
this lab. See the address-pool sizing rule in
[`docs/network-model.md`](network-model.md).

The range `192.0.2.101` - `192.0.2.116` is above the lease range
(`192.0.2.10` - `192.0.2.100`) and above the fixed low addresses. All of
them are on the same L2 bridge, so an address inside the lease range
could collide with the lease of a real machine.

### 6.2 Create the Site and the Subnet

`config/bootd` supplies the `kezio-bootd` ServiceAccount and the
ClusterRole that it binds to. Each Subnet's bootd Deployment names that
ServiceAccount, and the Subnet reconciler tests that it exists before it
builds the Deployment. Apply it first:

```sh
kubectl apply -k config/bootd
```

A Subnet in any other namespace needs its own `kezio-bootd`
ServiceAccount there, bound to the same ClusterRole, and the PSA label
from section 5.2.

The Site and the Subnet refer to each other, and neither can be created
first with both references complete: the Subnet webhook admits
`spec.siteRef` only against a Site that already exists, and the Site
webhook tests that `seederSubnetRef` names a Subnet that refers back.
Create the Site with no `seederSubnetRef`, then the Subnet, then apply
the Site again with the reference complete.
`.github/actions/create-site` does the same.

**Step 1** - the Site, with no seeding Subnet yet:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: kezio.kojuro.date/v1alpha3
kind: Site
metadata:
  name: lab
  namespace: kezio-system
spec: {}
EOF
```

**Step 2** - the Subnet:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: kezio.kojuro.date/v1alpha3
kind: Subnet
metadata:
  name: lab-prov
  namespace: kezio-system
spec:
  siteRef:
    name: lab
  cidr: "192.0.2.0/24"
  bootdServerIP: "192.0.2.2"
  bootdNetworkRef:
    name: kezio-boot-network
  seederNetworkRef:
    name: kezio-seeder-network
  dhcp:
    mode: lease
    gateway: ""
    leaseRangeStart: 192.0.2.10
    leaseRangeEnd: 192.0.2.100
EOF
```

**Step 3** - the Site again, this time with the reference and the
tracker address:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: kezio.kojuro.date/v1alpha3
kind: Site
metadata:
  name: lab
  namespace: kezio-system
spec:
  seederSubnetRef:
    name: lab-prov
  tracker:
    ip: 192.0.2.116
EOF
```

Notes on these three objects:

- **`mode: lease`** makes the dnsmasq of bootd the DHCP authority of the
  segment. That is correct here, because `vmbr1` has no DHCP server of
  its own. Use `mode: proxy` when the segment already has a DHCP server.
  Both modes are tested end to end on each push
  (`.github/workflows/main.yaml`, the `dhcp-scenario` matrix).
- **`gateway: ""`** declares that this segment has no exit, so machines
  receive no default route. That is correct here: bootd, the tracker,
  the seeders and the machines are all on `vmbr1`, so nothing that a
  machine talks to is off-segment. A lab whose seeder is on a different
  segment names the router of that segment here instead. The field is
  mandatory in `mode: lease` and not defaulted. If it were absent,
  dnsmasq would advertise bootd itself as the router, and bootd forwards
  nothing.
- **The MAC gate does not relax.** Only the `spec.bootMACAddress` of an
  enrolled Machine gets a lease. Each other device on the segment gets
  nothing.
- **`seederNetworkRef` is necessary.** Without it, a seeder pod gets a
  cluster pod address only, which is the one address that a machine on
  the provisioning segment cannot reach.
- **The tracker address is written into each `.torrent`.** No Service
  fronts the tracker or a seeder, because a ClusterIP would DNAT the
  connection. A BitTorrent peer connects to the address that the
  announce response gives it.

### 6.3 Wait for the objects, and read their conditions

```sh
kubectl -n kezio-system rollout status deployment/kezio-bootd-lab-prov --timeout=180s
curl -s -o /dev/null -w '%{http_code}\n' http://192.0.2.2:80/boot/artifacts/manifest.json
kubectl -n kezio-system get site lab -o jsonpath='{.status.trackerURL}{"\n"}'
```

The bootd Deployment is `kezio-bootd-<subnet name>`. The Subnet
reconciler builds it after it observes the Subnet, so it does not exist
at the instant that you apply the Subnet.

**Expect `Valid=Unknown` on the Subnet and on the Site.**
`internal/nadvalidate` does not model a `host-local` pool that
`rangeStart`/`rangeEnd` narrow, so `CheckSeederOverlap` and
`CheckTrackerAddress` can only report Indeterminate for the seeder NAD
above. Indeterminate raises the condition to Unknown, never to False.
Nothing is wrong. The CI lanes accept the same result
(`accept-valid-unknown` and `accept-unknown`).

If the bootd Deployment never appears, the Subnet states the cause:

```sh
kubectl -n kezio-system get subnet lab-prov -o jsonpath='{.status.conditions}' | jq .
```

`BootdNamespacePSALabelMissing` and `BootdServiceAccountMissing` are the
two prerequisites that the controller tests for you.
`BootdDeploymentImageUnconfigured` means that `BOOTD_DEPLOYMENT_IMAGE`
is not set on the manager.

## 7. Ingest an image

Any bootable disk image works. A cloud image such as
`https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img`
is a good first one. It is qcow2, which agrees with the default
`IMAGE_INGEST_SOURCE_FORMAT`.

### 7.1 Prepare the image

kezio deploys the image unchanged. It writes the partitions to the disk
and it writes a UEFI NVRAM entry, but it never edits the file system.
Everything that you want in the deployed system must be in the image
before you upload it. A pristine cloud image gives a system that you
cannot log in to and cannot see from Proxmox, for two reasons:

- **No account.** Cloud images get their users from cloud-init, and
  cloud-init finds no datasource on a bare-metal machine. The console
  shows a login prompt that no password opens.
- **No guest agent.** The minimal cloud image does not contain
  `qemu-guest-agent`, so the QEMU Guest Agent option of the VM stays
  disconnected and Proxmox never shows the address of the guest.

Correct both with `virt-customize` on the node. It edits the downloaded
file in place, so keep a copy of the download if you want to start
again.

Do not use `virt-customize --install`. It needs working networking
inside the appliance of libguestfs, which frequently does not start. apt
then fails each index with `Temporary failure resolving
archive.ubuntu.com` and reports the package as missing. That message
looks like a package-name problem, and it is not one. Download the
packages on the node instead, and install them offline. The node here
runs the same Ubuntu release as the image, so its archive gives
versions that agree:

```sh
sudo apt-get install -y --no-install-recommends libguestfs-tools
sudo chmod 0644 /boot/vmlinuz-*        # supermin builds its appliance from it
export LIBGUESTFS_BACKEND=direct

# Resolve the full runtime closure of qemu-guest-agent. Then keep only
# what the image does not already have, from its published manifest.
mapfile -t closure < <(apt-get install --simulate -o Dir::State::status=/dev/null \
  --no-install-recommends qemu-guest-agent | awk '/^Inst /{n=$2; sub(/:.*$/,"",n); print n}' | sort -u)
curl -sfL -o /tmp/img.manifest \
  https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.manifest
mapfile -t missing < <(comm -23 <(printf '%s\n' "${closure[@]}" | sort -u) \
  <(awk -F'\t' '{n=$1; sub(/:.*$/,"",n); print n}' /tmp/img.manifest | sort -u))
printf '%s\n' "${missing[@]}" | grep -qxF qemu-guest-agent || missing+=(qemu-guest-agent)
echo "injecting: ${missing[*]}"

mkdir -p qga-debs && (cd qga-debs && for p in "${missing[@]}"; do apt-get download "$p"; done)

sudo -E virt-customize -a ./ubuntu-24.04-minimal-cloudimg-amd64.img \
  --no-network \
  --copy-in "$PWD/qga-debs:/tmp" \
  --run-command 'dpkg -i /tmp/qga-debs/*.deb' \
  --run-command 'rm -rf /tmp/qga-debs' \
  --root-password password:kezio \
  --run-command 'touch /etc/cloud/cloud-init.disabled'
```

Resolve the closure. Do not use a fixed dependency list: which packages
the minimal image does not have changes between releases. For noble they
are `libnuma1` and `liburing2`. An incomplete set leaves `qemu-ga` in a
crash loop at each boot, with no indication why.

The disabled cloud-init also removes the datasource search, which
otherwise adds minutes to each boot.

Confirm the three changes before you upload:

```sh
sudo -E virt-ls -a ./ubuntu-24.04-minimal-cloudimg-amd64.img /usr/sbin | grep qemu-ga
sudo -E virt-ls -a ./ubuntu-24.04-minimal-cloudimg-amd64.img /etc/cloud | grep disabled
sudo -E virt-ls -a ./ubuntu-24.04-minimal-cloudimg-amd64.img -m /dev/sda15 /EFI/BOOT
```

A lab root password is acceptable. Never build a production image this
way. Give it an account through your own image pipeline instead.

The image must also contain its own fallback bootloader at
`\EFI\BOOT\BOOTX64.EFI` on its EFI System Partition. kezio-agent writes
a UEFI NVRAM entry that names that path after the deploy, and firmware
uses the same path whenever the NVRAM entry does not survive.

Ubuntu cloud images have a shim there but no GRUB beside it, and a shim
alone cannot start. The `install-removable-fallback` builtin in the
`kezio-default-finalize` PostHook completes the pair at deploy time, so
this needs no action here.

### 7.2 Upload it

The image-service Service is ClusterIP, so port-forward it from the
node:

```sh
kubectl -n kezio-system port-forward svc/kezio-image-service 18080:8080 &
bin/kezioctl image upload ./ubuntu-24.04-minimal-cloudimg-amd64.img \
  --name lab-ubuntu \
  --namespace kezio-system \
  --server http://127.0.0.1:18080 \
  --token "${IMAGE_SERVICE_TOKEN}"
```

The command streams the file to the image-service, then creates an
`ImageImport` that names the staged upload. The ingest Job converts the
image and cuts each partition one time with partclone, in the cluster. A
publish Job then builds the `.torrent` for each partition. The operator
writes one immutable `PartitionContent` for each partition that is not
swap - `lab-ubuntu-p1`, `lab-ubuntu-p2`, and so on - and the `Image`
`lab-ubuntu`, whose `spec.layout` holds the `sfdisk` dump inline.
`--image-name` and `--content-prefix` change those names. Both default
to `--name`.

Watch it:

```sh
kubectl -n kezio-system get imageimport lab-ubuntu -w
kubectl -n kezio-system get image lab-ubuntu -w
kubectl -n kezio-system get partitioncontent
```

Wait until the `ImageImport` reports `Succeeded`, and then until the
`Image` reports `Ready`. Watch the import beside the Image: an import that fails never
creates the Image at all, so the Image alone would show only the
time-out.

Give `--wait` to `image upload` to do this in the command itself. The
command then returns only when the `Image` is `Ready`, or stops with the
reason of the import as soon as the import fails. `--wait-timeout`
changes how long it waits.

No seeder runs yet. A seeder Deployment exists only while a Machine at
that Site deploys that Image.

Do not delete the `ImageImport` until each content that it created is
Ready. The scratch PVC of the import outlives the ingest Job, and each
publish Job reads the already-cut content out of it. The same care
applies to `spec.ttlSecondsAfterFinished`: it is unset by default, so
the manager never deletes a finished import on its own; set it only
once every content the import created has published.

## 8. Enroll the target machine

Store the Proxmox API token as a BMC credentials Secret. This is the
token from section 2.5, not the `PROXMOX_USER` credentials that
`params.env` holds:

```sh
kubectl -n kezio-system create secret generic lab-target-1-bmc \
  --type=kubernetes.io/basic-auth \
  --from-literal=username='kezio@pve!kezio' \
  --from-literal=password='<token-secret>'
```

Then create the Machine:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: kezio.kojuro.date/v1alpha3
kind: Machine
metadata:
  name: lab-target-1
  namespace: kezio-system
spec:
  bmc:
    address: redfish+http://10.0.0.10:8000/redfish/v1/Systems/501
    credentialsSecretRef:
      name: lab-target-1-bmc
  bootMACAddress: "52:54:00:be:ef:01"
  subnetRef:
    name: lab-prov
EOF
```

The Machine carries no deploy intent yet. Enrollment and deployment are
separate: kezio inspects the machine first, and section 9 binds a
`MachineClaim` after the agent registers. `.github/actions/create-bmc-machine`
and `.github/actions/deploy-machine` are the same two halves.

Four fields carry lab-specific detail:

- **The BMC path is explicit.** One proxmox-redfish daemon fronts each
  VM on the host, so its `Systems` collection has many members.
  `resolveSystem` refuses to select among them, and needs
  `/redfish/v1/Systems/<vmid>`.
- **The scheme is `redfish+http`.** It agrees with the plain-HTTP unit
  in section 2.4. `redfish://` means HTTPS, and fails against that unit
  with a connection error. If you give the daemon TLS, use `redfish://`
  and either give it a certificate that the manager trusts, or put
  `kezio.kojuro.date/bmc-insecure-skip-verify: "true"` on the Machine to
  connect without certificate verification.
- **`subnetRef` selects the Site.** A Machine never names its Site.
  kezio derives it as `subnetRef` -> `Subnet.spec.siteRef` -> `Site`
  (`internal/sitederive`). A machine therefore always leeches from the
  seeder at the segment that its boot NIC is really connected to.
- **`bootMACAddress` must be exactly the MAC of `net0` on the target
  VM.** bootd answers nothing to a MAC that is not the MAC of an
  enrolled Machine.

kezio now powers the VM on through the BMC, boots the live environment,
and waits for the agent to register. Watch it:

```sh
kubectl -n kezio-system get machine lab-target-1 -w
```

Wait until `status.state` is `Available`.

One difference from a real BMC: proxmox-redfish does a one-time PXE
request by rewriting the persistent boot order of the VM so that `net0`
is first, and nothing puts it back. The one-time override of a real BMC
is a firmware flag that clears itself after one boot. The deployed
machine therefore continues to try PXE first at each later boot. The MAC
gate of bootd leaves it unanswered, and firmware goes to the disk after
the PXE time-out. Set the order back by hand
(`qm set 501 --boot order=scsi0;net0`) if that delay is a problem.

## 9. Deploy, and watch it

Bind a `MachineClaim` carrying the deploy intent:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: kezio.kojuro.date/v1alpha3
kind: MachineClaim
metadata:
  name: lab-target-1-claim
  namespace: kezio-system
spec:
  machineName: lab-target-1
  imageRef:
    name: lab-ubuntu
  targetDisk:
    serialNumber: "KEZIOLAB0001"
EOF
kubectl -n kezio-system get machineclaim lab-target-1-claim -w
```

Wait until `status.phase` is `Bound`, then watch the machine deploy:

```sh
kubectl -n kezio-system get machine lab-target-1 -w
```

Leave `spec.postHookRefs` unset on the claim. kezio then substitutes
the shipped `kezio-default-finalize` PostHook, whose steps are
`mkswap`, `install-removable-fallback` and `efibootmgr`. **A claim that
names any hook opts out of that substitution.** If you name your own
hook, you must name `kezio-default-finalize` beside it, or the disk is
written and never made bootable.

The Machine goes through Enrolling, Inspecting, Available,
Provisioning, and Provisioned. `status.state` is the workflow axis.
`status.operationalStatus` is a separate axis, so a failure never erases
the position of the machine in the workflow. There is no Error state.

What happens behind each step:

1. The controller sets a one-time PXE boot and powers the VM on.
2. bootd answers the DHCP and the PXE request, and serves shim and GRUB
   over TFTP.
3. GRUB gets its configuration from the boot config server through the
   proxy of bootd, then the kernel, the initrd, and the squashfs.
4. kezio-agent starts in the live environment and registers with the
   agent server.
5. The Image reconciler starts a seeder Deployment for this (Image,
   Site) pair, and the manager gives the agent a deploy plan.
6. The agent leeches each partition over BitTorrent, writes it with
   partclone, replays the `sfdisk` layout, runs the `PostHook` steps,
   and writes the UEFI boot entry.
7. The controller power-cycles the machine into the deployed disk.

Useful views while it runs:

```sh
kubectl -n kezio-system logs -f deployment/kezio-controller-manager
kubectl -n kezio-system logs -f deployment/kezio-bootd-lab-prov
kubectl -n kezio-system get deployment -l app.kubernetes.io/component=image-seeder
kubectl -n kezio-system get deployrun
```

Watch the console of the VM at the same time, in the Proxmox web UI (VM
501 -> Console). It shows the PXE exchange, GRUB, and the boot of the
live environment. A failure in the boot path appears there first.

The seeder Deployment goes away again a few minutes after the last
Machine stops the deployment of that Image.
`PARTITIONCONTENT_SEEDER_GRACE_PERIOD` sets that delay. This is the
designed behavior, not a failure.

## 10. Confirm the result

```sh
kubectl -n kezio-system get machine lab-target-1 \
  -o jsonpath='{.status.state}{"\n"}{.status.conditions}' | jq .
kubectl -n kezio-system get deployrun \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase
```

`status.state` must be `Provisioned`, and the phase of the DeployRun
must be `Succeeded`.

Then examine the VM itself. The console shows the login prompt of the
deployed system instead of the live environment, and the root password
from section 7.1 opens it. The QEMU Guest Agent also connects, so
Proxmox shows the address of the guest on the summary page of the VM.

## 11. Add a second machine, or a second site

- **Another machine on the same segment**: create its VM (section 2.3),
  its BMC Secret, and its Machine (section 8). Nothing else changes.
- **Another segment in the same Site**: create the bridge and its boot
  NAD, and a `Subnet` with its own `cidr` and `bootdServerIP`, whose
  `siteRef` names the same Site. The Subnet reconciler builds the bootd
  of that segment, and it proxies to the same boot server and agent
  server. Give it no `seederNetworkRef`: one Site has one seeding
  Subnet. **You must give the machines on that segment a route to the
  seeding segment.** Set `dhcp.gateway` to the router of that segment,
  and configure the router yourself. kezio configures no routing.
- **Another Site**: anything behind a barrier that breaks reachability
  is a different Site. Create its own `Site`, its own seeding `Subnet`,
  its own tracker address, and its own seeder NAD. A Machine at a Site
  never leeches across a link to the seeder of another Site.

Remember what a Site means: each Subnet inside one Site is mutually
routable. You declare this; kezio never tests it. A Site whose Images
have content needs a `seederSubnetRef`, or its Machines wait for ever.

## 12. What this lab does not match in production

- **The Redfish shim is not a BMC.** proxmox-redfish drives the Proxmox
  API. It is sufficient for the lab, and it exercises the real
  `redfish://` driver of kezio, but its behavior is its own, not the
  behavior of a vendor BMC.
- **A one-time PXE boot is not really one-time.** The shim does
  `BootSourceOverrideTarget: Pxe` by moving the network device to the
  front of the Proxmox boot order of the VM, and that change stays. See
  section 8.
- **Secure Boot is off.** The shim and the GRUB that kezio serves are
  signed by Debian, so a machine can keep Secure Boot on through the
  whole chain, and the kernel-signing choice is what decides it. No CI
  lane exercises this. Read [`docs/secure-boot.md`](secure-boot.md)
  before you turn it on, and give the VM an EFI disk with pre-enrolled
  keys at that point.
- **Storage is a single node.** The NFS server in section 3.3 exports a
  `hostPath` on the one node. It gives real ReadWriteMany semantics, but
  it has no redundancy and no capacity control.
- **The tracker is plain HTTP with no authentication**, and the segment
  is trusted because it is isolated.
- **The BMC credentials cross the management network in base64**,
  because the shim serves plain HTTP.

## 13. When something does not work

| Symptom | Where to look |
|---|---|
| No bootd Deployment appears | `kubectl -n kezio-system get subnet lab-prov -o yaml`, `status.conditions`. `BootdNamespacePSALabelMissing`, `BootdServiceAccountMissing`, and `BootdDeploymentImageUnconfigured` are all named there. |
| `Valid=Unknown` on the Subnet or the Site | Expected with the `host-local` seeder NAD above. See section 6.3. |
| The VM shows PXE-E16 or PXE-E18 and never boots | The FORWARD accept and the checksum rule in section 3.4. Then confirm that the `k8s.v1.cni.cncf.io/network-status` annotation of the bootd pod really lists `kezio-boot-network`. |
| The VM gets no DHCP answer at all | The MAC gate. `spec.bootMACAddress` must be exactly the MAC of the NIC. The log of bootd names each MAC that it refuses. |
| GRUB loads, then nothing | `curl http://192.0.2.2:80/boot/artifacts/manifest.json` from the node. A failure there is the proxy of bootd, or the Subnet's `bootdServerIP`. |
| The Machine stays in Inspecting | The Subnet's `bootdServerIP` (or `BOOT_AGENT_SERVER_URL` if its Subnet has no boot half) and `DEPLOYER=agent` (section 5.4). |
| The ImageImport stays at Pending | `IMAGE_INGEST_IMAGE` and `IMAGE_INGEST_STAGING_PVC`. The conditions of the import name which one is unset. |
| The ingest Job fails on the format | `IMAGE_INGEST_SOURCE_FORMAT` must match the real format of the source. It defaults to `qcow2`. |
| The ingest Job fails naming `/dev/nbd0` or a missing partition device | The node's `nbd` kernel module is not loaded (or lacks partition support): `sudo modprobe nbd max_part=16`. Or set `IMAGE_INGEST_UNPRIVILEGED=true` to fall back to the unprivileged copy path. |
| A PartitionContent stays at Pending | `PARTITIONCONTENT_PUBLISH_IMAGE`. |
| A PVC stays Pending | The `nfs` StorageClass from section 3.3. Each PVC that kezio creates asks for ReadWriteMany. |
| The agent waits for ever for a plan | The Site has no tracker, or no seeder. `kubectl get site lab -o jsonpath='{.status.trackerURL}'` must be non-empty, and `status.seederReady` must be true. |
| The leech never finishes | Port `6969` (tracker), port `16881` (BitTorrent) and port `8080` (the `.torrent` server of the seeder) must be reachable from the target, with nothing that rewrites an address on the path. |
| The seeder pod cannot get an address | The `host-local` pool in section 6.1 is too small, or the tracker address is outside it. |
| The BMC call fails | The log of the controller-manager, filtered for `redfish`. A `found N systems` error means that the address needs its `/redfish/v1/Systems/<vmid>` path. A TLS error means that the scheme must be `redfish+http`. A 401 means the token ACL in section 2.5. |
| The deployed disk does not boot | The fallback bootloader contract in section 7.1, and the hook rule in section 9. |
