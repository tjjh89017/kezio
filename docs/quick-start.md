# Quick start

This guide takes one Kubernetes cluster and one bare-metal machine from
nothing to a deployed OS. Each step is a command you run and a result
you check. Do the steps in order.

The machine in this guide is on one provisioning segment that has no
DHCP server of its own. kezio becomes the DHCP server of that segment.
If the segment already has a DHCP server, one field changes; the guide
says where.

## 0. What you need before you start

Have all of these before step 1:

- A Kubernetes cluster (one node is enough) with:
  - [Multus](https://github.com/k8snetworkplumbingwg/multus-cni) installed.
    kezio attaches its boot and seeder pods to the provisioning segment
    through a Multus `NetworkAttachmentDefinition`.
  - A default `StorageClass` that supports `ReadWriteMany`. The image
    upload service and the content store use it.
  - The `bridge` CNI plugin binary on the node (it ships with the
    standard CNI plugins).
  - The `nbd` kernel module on the node: `sudo modprobe nbd max_part=16`.
    The import Job attaches the source image with `qemu-nbd`.
- One node interface on the provisioning segment, enslaved to a Linux
  bridge named `kezio-prov0`. The node itself needs no address on that
  bridge.
- The target machine:
  - Its boot NIC is on the same segment, and you know that NIC's MAC.
  - A BMC that kezio can reach from the cluster, with Redfish or IPMI,
    and its username and password.
  - UEFI firmware. A machine that can only boot in legacy BIOS mode
    does not work.
- `kubectl` with access to the cluster, `kustomize` (v5), `git`, and
  `curl`.

Pick the kezio release to install and keep it in a variable for every
later command:

```sh
export KEZIO_VERSION=v0.3.12
```

The addresses in this guide, on the provisioning segment
`192.0.2.0/24`:

| Address | Used by |
|---|---|
| `192.0.2.2` | bootd (PXE, TFTP, and the proxy the machine talks to) |
| `192.0.2.10` - `192.0.2.100` | DHCP leases for machines that net boot |
| `192.0.2.101` - `192.0.2.115` | seeder pods |
| `192.0.2.116` | the tracker |

Change them all consistently if your segment is a different one.

## 1. Install cert-manager

kezio's webhooks take their serving certificate from cert-manager.

```sh
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.yaml
for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
  kubectl wait deployment.apps/"${d}" --for condition=Available -n cert-manager --timeout=180s
done
```

Check: the three `kubectl wait` commands return `condition met`.

## 2. Get the kezio sources at the release

The installer that a release ships (`install.yaml`) carries the
controller manager alone. A cluster that net boots machines also needs
the boot server, the agent server, and the boot artifacts inside the
manager pod. Those come from kustomize components in the repository, so
check the repository out at the release:

```sh
git clone --branch "${KEZIO_VERSION}" --depth 1 https://github.com/tjjh89017/kezio.git
cd kezio
```

Check: `git describe --tags` prints `${KEZIO_VERSION}`.

## 3. Install the controller manager

Make an overlay that adds the net boot components to the default
install, marks the namespace as privileged (bootd's dnsmasq needs
`NET_ADMIN`, `NET_RAW`, and `NET_BIND_SERVICE`), and points every image
at the release:

```sh
mkdir -p config/quickstart
cp config/netboot-e2e/namespace-privileged-patch.yaml config/quickstart/

cat <<'EOF' > config/quickstart/kustomization.yaml
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

(cd config/manager && kustomize edit set image "controller=ghcr.io/tjjh89017/kezio:${KEZIO_VERSION}")
(cd config/quickstart && kustomize edit set image \
  "ghcr.io/tjjh89017/kezio-boot-artifacts=ghcr.io/tjjh89017/kezio-boot-artifacts:${KEZIO_VERSION}")

kustomize build config/crd | kubectl apply --server-side -f -
kustomize build config/quickstart | kubectl apply -f -
kubectl -n kezio-system rollout status deployment/kezio-controller-manager --timeout=180s
```

Check:

```sh
kubectl wait --for=create posthook/kezio-default-finalize -n kezio-system --timeout=60s
```

The manager creates this PostHook at start. If it never appears, the
manager is not running correctly; read
`kubectl -n kezio-system logs deployment/kezio-controller-manager`.

## 4. Install the bootd RBAC

Each `Subnet` gets its own bootd Deployment, built by the manager. That
Deployment runs as the `kezio-bootd` ServiceAccount, which the installer
does not create:

```sh
kustomize build config/bootd | kubectl apply -f -
```

Check: `kubectl -n kezio-system get serviceaccount kezio-bootd` lists it.

## 5. Install the image upload service

The image upload service receives disk images from `kezioctl` and stages
them for import. It reads a bearer token from a Secret that you create:

```sh
export IMAGE_SERVICE_TOKEN="$(openssl rand -hex 32)"
kubectl -n kezio-system create secret generic kezio-image-service-token \
  --from-literal=token="${IMAGE_SERVICE_TOKEN}"

(cd config/image-service && kustomize edit set image "image-service=ghcr.io/tjjh89017/kezio-image-service:${KEZIO_VERSION}")
kustomize build config/image-service | kubectl apply -f -
kubectl -n kezio-system rollout status deployment/kezio-image-service --timeout=180s
```

This creates a 100Gi `ReadWriteMany` PVC named
`kezio-image-service-staging` from the default StorageClass.

The manager read the token Secret at start, and it did not exist then.
Restart the manager so it picks the token up:

```sh
kubectl -n kezio-system rollout restart deployment/kezio-controller-manager
kubectl -n kezio-system rollout status deployment/kezio-controller-manager --timeout=180s
```

Check: `kubectl -n kezio-system get pvc kezio-image-service-staging`
shows `Bound`. If it stays `Pending`, the default StorageClass does not
provide `ReadWriteMany`.

## 6. Configure the manager

Every net boot feature is off until an environment variable turns it on.
`192.0.2.2` is bootd's address on the segment; a machine reaches the
boot API and the agent API through bootd's proxy at that one address.

```sh
kubectl -n kezio-system set env deployment/kezio-controller-manager \
  DEPLOYER=agent \
  BOOT_SERVER_ADDR=:8090 \
  BOOT_SERVER_URL="http://192.0.2.2" \
  BOOT_AGENT_SERVER_URL="http://192.0.2.2" \
  AGENT_SERVER_ADDR=:8091 \
  BOOTD_DEPLOYMENT_IMAGE="ghcr.io/tjjh89017/kezio-bootd:${KEZIO_VERSION}" \
  BOOTD_DEPLOYMENT_BOOT_ARTIFACTS_IMAGE="ghcr.io/tjjh89017/kezio-boot-artifacts:${KEZIO_VERSION}" \
  BOOTD_DEPLOYMENT_BOOT_UPSTREAM_URL="http://boot-server.kezio-system.svc.cluster.local:8090" \
  BOOTD_DEPLOYMENT_AGENT_UPSTREAM_URL="http://agent-server.kezio-system.svc.cluster.local:8091" \
  IMAGE_INGEST_IMAGE="ghcr.io/tjjh89017/kezio-ingest:${KEZIO_VERSION}" \
  IMAGE_INGEST_STAGING_PVC=kezio-image-service-staging \
  PARTITIONCONTENT_PUBLISH_IMAGE="ghcr.io/tjjh89017/kezio-ingest:${KEZIO_VERSION}" \
  PARTITIONCONTENT_SEEDER_IMAGE="ghcr.io/tjjh89017/kezio-seeder:${KEZIO_VERSION}" \
  TRACKER_DEPLOYMENT_IMAGE=ghcr.io/tunisiano187/opentracker-docker:master

kubectl -n kezio-system rollout status deployment/kezio-controller-manager --timeout=180s
```

Two of these deserve a note:

- `DEPLOYER=agent` selects the deployer that drives real hardware. Unset,
  kezio uses a fake deployer that never touches a machine.
- `BOOT_AGENT_SERVER_URL` must be set. Without it the agent's
  registration fails with 404 and the machine waits in `Inspecting`
  with nothing in the manager log to explain it.

The import Job expects a `qcow2` source. If you import raw disk images,
also set `IMAGE_INGEST_SOURCE_FORMAT=raw`.

## 7. Attach the cluster to the provisioning segment

Two Multus attachments on the `kezio-prov0` bridge: one static address
for bootd, and one pool for the seeder pods and the tracker.

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

The tracker takes the top address of the seeder pool (`192.0.2.116`);
keep that address inside the pool's range.

Check: `kubectl -n kezio-system get network-attachment-definitions`
lists both.

## 8. Create the Site and the Subnet

A `Site` owns the tracker; a `Subnet` is one segment with its bootd. The
two refer to each other, so create the Site without its Subnet
reference first, then the Subnet, then the Site again complete.

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: kezio.kojuro.date/v1alpha3
kind: Site
metadata:
  name: lab
  namespace: kezio-system
spec: {}
---
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

If the segment already has a DHCP server, replace the `dhcp:` block
with `dhcp: {mode: proxy}`. bootd then answers only the PXE part of the
exchange and the existing server keeps giving out addresses.

`gateway: ""` says the segment has no router, so machines get no
default route. That is correct when bootd, the tracker, the seeders, and
the machines all sit on this one segment.

Check, once the manager has built bootd:

```sh
kubectl -n kezio-system rollout status deployment/kezio-bootd-lab-prov --timeout=180s
kubectl -n kezio-system get site lab -o jsonpath='{.status.trackerURL}{"\n"}'
```

The first prints `successfully rolled out`; the second prints the
tracker's announce URL, which names `192.0.2.116`. A `Valid=Unknown` condition on the
Site or the Subnet is normal with a `host-local` pool and means nothing
is wrong.

If the bootd Deployment never appears, the Subnet's conditions name the
cause:

```sh
kubectl -n kezio-system get subnet lab-prov -o jsonpath='{.status.conditions}' | jq .
```

## 9. Upload an image

Get `kezioctl` from the release. On Linux amd64:

```sh
curl -fsSLO "https://github.com/tjjh89017/kezio/releases/download/${KEZIO_VERSION}/kezioctl-linux-amd64.tar.gz"
tar -xzf kezioctl-linux-amd64.tar.gz
sudo install -m 0755 kezioctl-linux-amd64 /usr/local/bin/kezioctl
```

Any bootable UEFI disk image in qcow2 format works. The Ubuntu cloud
image is a small one to start with:

```sh
curl -fsSLO https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img
```

A cloud image has no password and no user you can log in as. To get a
console login after the deploy, set a root password in the image first
(`virt-customize -a ubuntu-24.04-minimal-cloudimg-amd64.img --root-password password:changeme`
from the `guestfs-tools` package does that).

The upload service is a ClusterIP Service, so port-forward it and
upload through the forward. `--wait` makes the command return only once
the Image is ready to deploy:

```sh
kubectl -n kezio-system port-forward svc/kezio-image-service 18080:8080 &
kezioctl image upload ./ubuntu-24.04-minimal-cloudimg-amd64.img \
  --name lab-ubuntu \
  --namespace kezio-system \
  --server http://127.0.0.1:18080 \
  --token "${IMAGE_SERVICE_TOKEN}" \
  --wait
```

Behind the command: the upload lands on the staging PVC, an
`ImageImport` starts a Job that slices each partition with partclone,
a second Job builds a `.torrent` per partition, and the manager creates
one `PartitionContent` per partition (`lab-ubuntu-p1`, ...) and the
`Image` `lab-ubuntu`.

Check:

```sh
kubectl -n kezio-system get imageimport lab-ubuntu
kubectl -n kezio-system get image lab-ubuntu
```

The import shows `Succeeded` and the Image shows `Ready`. If the import
fails, `kubectl -n kezio-system describe imageimport lab-ubuntu` states
why. Do not delete the `ImageImport` until the Image is `Ready`.

## 10. Enroll the machine

Store the BMC credentials, then create the `Machine`. Replace the BMC
address, the credentials, and the MAC with your machine's:

```sh
kubectl -n kezio-system create secret generic target-1-bmc \
  --type=kubernetes.io/basic-auth \
  --from-literal=username='admin' \
  --from-literal=password='<bmc password>'

cat <<'EOF' | kubectl apply -f -
apiVersion: kezio.kojuro.date/v1alpha3
kind: Machine
metadata:
  name: target-1
  namespace: kezio-system
spec:
  bmc:
    address: redfish://10.0.0.10
    credentialsSecretRef:
      name: target-1-bmc
  bootMACAddress: "aa:bb:cc:dd:ee:01"
  subnetRef:
    name: lab-prov
EOF
```

The BMC address forms:

| Form | Meaning |
|---|---|
| `redfish://host` | Redfish over HTTPS |
| `redfish+http://host` | Redfish over plain HTTP |
| `ipmi://host` | IPMI over LAN |

A BMC whose HTTPS certificate is self-signed needs the annotation
`kezio.kojuro.date/bmc-insecure-skip-verify: "true"` on the Machine.
A Redfish service that fronts more than one system needs the explicit
system path, `redfish://host/redfish/v1/Systems/<id>`.

`bootMACAddress` must be exactly the MAC of the NIC that net boots.
bootd answers DHCP only for the MAC of an enrolled Machine; every other
device on the segment gets no answer.

kezio now sets a one-time PXE boot, powers the machine on, and boots it
into the live agent. The agent registers and reports the machine's
disks, NICs, CPU, and memory. Watch it:

```sh
kubectl -n kezio-system get machine target-1 -w \
  -o custom-columns=NAME:.metadata.name,STATE:.status.state,STATUS:.status.operationalStatus
```

Check: `STATE` becomes `Available`. It takes a few minutes; the state
goes `Enrolling`, `Inspecting`, `Available`. Then read the disks the
machine reported, because the next step names one of them:

```sh
kubectl -n kezio-system get machinehardware target-1 -o jsonpath='{.spec.disks}' | jq .
```

If the machine never reaches `Available`, open its console (through the
BMC) and watch the boot: the PXE exchange, GRUB, and the live
environment's boot appear there before anything appears in the cluster.
`kubectl -n kezio-system logs deployment/kezio-bootd-lab-prov` shows
the DHCP and TFTP side.

## 11. Deploy

Bind a `MachineClaim` to the machine. It names the Image and the target
disk; the disk hint must select exactly one disk from the list of step
10 (serial number is the safest hint):

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: kezio.kojuro.date/v1alpha3
kind: MachineClaim
metadata:
  name: target-1-claim
  namespace: kezio-system
spec:
  machineName: target-1
  imageRef:
    name: lab-ubuntu
  targetDisk:
    serialNumber: "<serial of the disk to write>"
EOF
```

**This overwrites the named disk.**

Watch the deploy:

```sh
# PHASE becomes Bound
kubectl -n kezio-system get machineclaim target-1-claim -w \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase
# STATE becomes Provisioning, then Provisioned
kubectl -n kezio-system get machine target-1 -w \
  -o custom-columns=NAME:.metadata.name,STATE:.status.state,STATUS:.status.operationalStatus
# PHASE walks Partitioning, WritingContent, RunningPostHook, Finalizing, Succeeded
kubectl -n kezio-system get deployrun -w \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase
```

Behind it: the manager starts a seeder pod for the Image at this Site,
boots the machine into the live agent again, the agent fetches each
partition over BitTorrent from that seeder and writes it with partclone,
replays the partition table, makes the disk bootable, and the manager
power-cycles the machine into the deployed disk. The seeder pod goes
away a few minutes after the last deploy of that Image finishes; that
is normal.

Check:

```sh
kubectl -n kezio-system get machine target-1 -o jsonpath='{.status.state} {.status.operationalStatus}{"\n"}'
```

prints `Provisioned OK`, and the machine's console shows the deployed
system's login prompt.

## 12. Deploy again, or deploy more machines

- **The same machine, a new image**: delete the `MachineClaim`. The
  Machine goes to `Released`; its disk keeps what the deploy wrote.
  Ask for a fresh inspection to make it `Available` again, then create
  a new claim:

  ```sh
  kubectl -n kezio-system annotate machine target-1 kezio.kojuro.date/re-inspect=true
  ```
- **Another machine on the same segment**: repeat step 10 and step 11
  with its BMC Secret, MAC, and disk. Nothing else changes; the seeder
  serves every machine of the segment at once.

## When something does not work

| Symptom | Where to look |
|---|---|
| bootd Deployment never appears | `kubectl -n kezio-system get subnet lab-prov -o jsonpath='{.status.conditions}'` names the missing prerequisite (namespace label, ServiceAccount, manager image variable). |
| bootd pod not Ready | Its readiness probe needs `192.0.2.2` on its Multus interface. `kubectl -n kezio-system describe pod -l app.kubernetes.io/component=bootd` shows the attachment; a pod with only a cluster address lost the bridge. |
| Site `Ready=False`, `TrackerNetworkReady=False` | The tracker pod did not get `192.0.2.116` on the seeder attachment. Delete the tracker pod to recreate it, and confirm the seeder pool includes that address. |
| Machine stays `Enrolling` with an error | The BMC address, credentials, or reachability. `kubectl -n kezio-system describe machine target-1` shows the BMC error. |
| Machine powers on but stays `Inspecting` | The PXE path. Watch the console; then bootd's log for DHCP/TFTP, then the manager log for the boot config and registration requests. |
| Deploy stalls at 0% | The machine cannot reach the tracker or the seeder on `192.0.2.0/24`. Check the Site condition above and that the seeder pod has an address in the pool. |
| Claim never binds | The Machine is not `Available`, or the claim's disk hint matches zero or several disks. `kubectl -n kezio-system describe machineclaim target-1-claim`. |

The manager log has everything the controllers decided:

```sh
kubectl -n kezio-system logs -f deployment/kezio-controller-manager
```
