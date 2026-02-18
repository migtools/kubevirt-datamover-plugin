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
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
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
// Note: These match velero's internal/resourcepolicies constants but cannot be imported
// because they are in an internal package. Keep in sync with velero's VolumeActionType values.
// TODO(https://github.com/migtools/kubevirt-datamover-plugin/issues/4): Once upstream
// Velero changes are merged, add "custom" action type constant.
const (
	// VolumePolicyActionSkip indicates the volume should be skipped
	// Matches velero internal/resourcepolicies.Skip
	VolumePolicyActionSkip = "skip"

	// VolumePolicyActionSnapshot indicates CSI snapshot should be used
	// This is currently used to trigger kubevirt datamover backup
	// Matches velero internal/resourcepolicies.Snapshot
	VolumePolicyActionSnapshot = "snapshot"

	// VolumePolicyActionFSBackup indicates filesystem backup should be used
	// Matches velero internal/resourcepolicies.FSBackup
	VolumePolicyActionFSBackup = "fs-backup"
)

// Annotation keys - use velero's exported constants where available
const (
	// AnnDataUploadName is the annotation key for the DataUpload name on VirtualMachine
	// Re-exported from velero for convenience
	AnnDataUploadName = velerov1.DataUploadNameAnnotation

	// AnnVolumePolicy is the annotation for volume policy on PVC
	AnnVolumePolicy = "velero.io/volume-policy"
)

// Label keys - use velero's exported constants where available
const (
	// LabelBackupName is the label for the backup name
	// Re-exported from velero for convenience
	LabelBackupName = velerov1.BackupNameLabel
)

// VolumeSnapshotAction represents the action to take for a volume
// TODO(https://github.com/migtools/kubevirt-datamover-plugin/issues/4): Once upstream
// Velero changes are merged, add VolumeSnapshotActionCustom for the new "custom" action type.
type VolumeSnapshotAction string

const (
	// VolumeSnapshotActionSkip triggers kubevirt datamover backup for now.
	// We use "skip" because it prevents Velero's default CSI snapshot handling,
	// allowing the kubevirt datamover to handle the backup instead.
	// TODO(https://github.com/migtools/kubevirt-datamover-plugin/issues/4): Change to "custom" once upstream lands.
	VolumeSnapshotActionSkip VolumeSnapshotAction = "skip"

	// VolumeSnapshotActionSnapshot indicates CSI snapshot - conflicts with kubevirt datamover
	VolumeSnapshotActionSnapshot VolumeSnapshotAction = "snapshot"

	// VolumeSnapshotActionOther some other action (fs-backup, etc.)
	VolumeSnapshotActionOther VolumeSnapshotAction = "other"
)
