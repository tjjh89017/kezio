# Legacy documents

This directory holds documents that describe an older shape of kezio.

They are kept for reference only. Nobody maintains them. They do not
describe the current tree, and they can disagree with it. Do not use
them to build or to operate a cluster. Read the documents in `docs/`
instead, and read `config/` and `api/v1alpha2/` when you need the
authority.

Expect these files to be deleted.

## What is here, and why

| File | Why it moved here |
|---|---|
| `lab-all-in-one.yaml` | Its sample `Site`, `Subnet`, and `Machine` objects still declare `apiVersion: kezio.kojuro.date/v1alpha1`, a version no CRD in the tree serves. It also holds a `MutatingWebhookConfiguration`, and `v1alpha1` validating webhooks, that the manager no longer serves. Its embedded `Subnet` CRD is behind `config/crd/bases/`: it has no `spec.dhcp.gateway`, so it prunes that field and does not enforce the rule that makes the field mandatory in lease mode. Use `config/` and the commands in `docs/lab-proxmox-rke2.md`. |
