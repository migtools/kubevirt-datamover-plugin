// Copyright 2026 Red Hat Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package common

import (
	controllercommon "github.com/migtools/kubevirt-datamover-controller/pkg/common"
)

// Re-export shared constants from the controller package for convenience.
// These are used for DataUpload annotations that the controller reads.
const (
	// AnnotationVMName is the annotation key for the source VirtualMachine name.
	// Re-exported from controller common package.
	AnnotationVMName = controllercommon.AnnotationVMName

	// AnnotationVMNamespace is the annotation key for the source VirtualMachine namespace.
	// Re-exported from controller common package.
	AnnotationVMNamespace = controllercommon.AnnotationVMNamespace

	// AnnotationOperationID is the annotation for tracking async backup/restore operations.
	// Re-exported from controller common package.
	AnnotationOperationID = controllercommon.AnnotationOperationID

	// DataMoverKubeVirt is the datamover identifier for kubevirt incremental backups.
	// Re-exported from controller common package.
	DataMoverKubeVirt = controllercommon.DataMoverKubeVirt

	// SnapshotTypeCSI is the snapshot type for CSI-based backups.
	// Re-exported from controller common package.
	SnapshotTypeCSI = controllercommon.SnapshotTypeCSI
)

// Volume policy action constants - plugin-specific
const (
	// VolumePolicyActionKubevirt is the volume policy action that triggers kubevirt datamover
	VolumePolicyActionKubevirt = "kubevirt"

	// VolumePolicyActionSkip indicates the volume should be skipped
	VolumePolicyActionSkip = "skip"

	// VolumePolicyActionSnapshot indicates CSI snapshot should be used
	VolumePolicyActionSnapshot = "snapshot"

	// VolumePolicyActionFSBackup indicates filesystem backup should be used
	VolumePolicyActionFSBackup = "fs-backup"
)

// Annotation keys specific to the plugin (not shared with controller)
const (
	// AnnDataUploadName is the annotation key for the DataUpload name on VirtualMachine
	AnnDataUploadName = "velero.io/data-upload-name"

	// AnnVolumePolicy is the annotation for volume policy on PVC
	AnnVolumePolicy = "velero.io/volume-policy"
)

// Label keys specific to the plugin
const (
	// LabelBackupName is the label for the backup name
	LabelBackupName = "velero.io/backup-name"
)

// VolumeSnapshotAction represents the action to take for a volume
type VolumeSnapshotAction string

const (
	// VolumeSnapshotActionKubevirt use kubevirt datamover
	VolumeSnapshotActionKubevirt VolumeSnapshotAction = "kubevirt"

	// VolumeSnapshotActionSkip skip the volume
	VolumeSnapshotActionSkip VolumeSnapshotAction = "skip"

	// VolumeSnapshotActionOther some other action (snapshot, fs-backup, etc.)
	VolumeSnapshotActionOther VolumeSnapshotAction = "other"
)
