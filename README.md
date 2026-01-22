# KubeVirt Datamover Plugin

OADP Velero plugin for KubeVirt incremental backup and restore.

## Overview

This repository contains Velero BackupItemAction (BIA) and RestoreItemAction (RIA) plugins that enable KubeVirt VirtualMachine backups using qcow2 incremental snapshots instead of traditional CSI snapshots.

These plugins work in conjunction with the [KubeVirt Datamover Controller](https://github.com/migtools/kubevirt-datamover-controller) to provide:

- True block-level Changed Block Tracking (CBT) instead of full volume scans
- Faster incremental backups using QEMU/libvirt native capabilities
- VM-aware backup operations (as opposed to treating disks as generic PVCs)

## How It Works

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Velero Backup Flow                                │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  1. Velero triggers backup                                                  │
│           │                                                                 │
│           ▼                                                                 │
│  2. VirtualMachine BIA Plugin                                               │
│      • Checks VM has ChangedBlockTracking enabled                           │
│      • Validates volume policies (action: "kubevirt")                       │
│      • Creates DataUpload with SnapshotType: "kubevirt"                     │
│           │                                                                 │
│           ▼                                                                 │
│  3. KubeVirt Datamover Controller (separate component)                      │
│      • Reconciles DataUpload                                                │
│      • Creates VirtualMachineBackup CR                                      │
│      • Copies qcow2 files to BSL                                            │
│           │                                                                 │
│           ▼                                                                 │
│  4. Backup completes with incremental qcow2 data in object storage          │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Plugins

### VirtualMachine BackupItemAction

Handles VirtualMachine resources during backup:

- Validates that Changed Block Tracking is enabled on the VM
- Checks that the VM is running (offline backup not supported in initial release)
- Evaluates volume policies to determine if KubeVirt datamover should be used
- Creates a `DataUpload` CR with:
  - `Spec.SnapshotType`: `"kubevirt"`
  - `Spec.DataMover`: `"kubevirt"`
- Adds `DataUploadNameAnnotation` to the VirtualMachine
- Returns an async operation ID for progress tracking

### VirtualMachine RestoreItemAction

Handles VirtualMachine resources during restore:

- Creates `DataDownload` CR based on backup annotations
- Coordinates with the KubeVirt Datamover Controller for qcow2-to-raw conversion

### VirtualMachineBackup/VirtualMachineBackupTracker RestoreItemAction

Prevents restoration of backup-related CRs:

- Discards `VirtualMachineBackup` resources on restore
- Discards `VirtualMachineBackupTracker` resources on restore
- Prevents accidental re-triggering of backup operations

## Prerequisites

- OADP 1.5+ with Velero 1.15+
- KubeVirt with Changed Block Tracking support
- KubeVirt Datamover Controller deployed in the cluster
- `SnapshotMoveData: true` on backups

## Integration with OADP

The plugin is deployed as an init container in the Velero deployment. Add the following to your OADP DataProtectionApplication CR:

```yaml
apiVersion: oadp.openshift.io/v1alpha1
kind: DataProtectionApplication
metadata:
  name: velero-dpa
spec:
  configuration:
    velero:
      defaultPlugins:
        - openshift
        - kubevirt
        - kubevirt-datamover  # Enable KubeVirt datamover plugins
  # ... other configuration
```

## Building

```bash
# Build locally
make build-local

# Build with Podman or Docker
make all

# Build container image
make container

# Run tests
make test

# Clean build artifacts
make clean
```

## Project Structure

```
kubevirt-datamover-plugin/
├── kubevirt-datamover-plugin/
│   ├── main.go                 # Plugin registration
│   └── clients/                # Kubernetes client factory
│       └── clients.go
├── Dockerfile
├── Makefile
├── go.mod
└── go.sum
```

