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

package main

import (
	"github.com/sirupsen/logrus"
	veleroplugin "github.com/vmware-tanzu/velero/pkg/plugin/framework"

	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/pvc"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/vm"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/vmbackup"
)

func main() {
	veleroplugin.NewServer().
		// VirtualMachine BackupItemAction for kubevirt datamover
		// This plugin handles VirtualMachine resources by checking preconditions
		// (CBT enabled, VM running, kubevirt volume policy) and creating DataUpload CRs
		// for incremental backup via the kubevirt datamover controller.
		RegisterBackupItemActionV2("kubevirt.io/01-vm-datamover-backup-plugin", newVMBackupPlugin).
		RegisterBackupItemActionV2("kubevirt.io/02-pvc-datamover-backup-plugin", newPVCBackupPlugin).
		RegisterDeleteItemAction("kubevirt.io/01-vm-datamover-delete-plugin", newVMDeletePlugin).
		// PersistentVolumeClaim RestoreItemAction for kubevirt datamover.
		// Recognizes PVCs tagged by the PVC BackupItemAction (AnnotationVMName) and
		// creates DataDownload CRs to trigger the kubevirt datamover controller's
		// restore path, tracking progress via the async operation ID mechanism.
		RegisterRestoreItemActionV2("kubevirt.io/03-pvc-datamover-restore-plugin", newPVCRestorePlugin).
		// VirtualMachineBackup/VirtualMachineBackupTracker RestoreItemAction.
		// Discards these backup-time-only bookkeeping CRs on restore.
		RegisterRestoreItemAction("kubevirt.io/04-vmbackup-discard-plugin", newVMBackupDiscardPlugin).
		// VirtualMachine RestoreItemAction for kubevirt datamover.
		// Halts a kubevirt-datamover-backed VM at restore time (stashing its
		// original run state in annotations) so its virt-launcher pod cannot
		// race a WaitForFirstConsumer-bound PVC into a wrong PV before this
		// VM's DataDownload(s) rebind it. Targets VirtualMachine only, so it
		// never conflicts with 04's VirtualMachineBackup/Tracker selector.
		RegisterRestoreItemActionV2("kubevirt.io/05-vm-datamover-restore-plugin", newVMRestorePlugin).
		Serve()
}

// newVMBackupPlugin creates a new VirtualMachine BackupItemAction plugin.
// This plugin is responsible for:
// - Checking if VirtualMachine is eligible for kubevirt datamover backup
// - Creating DataUpload CRs with SnapshotType and DataMover set to "kubevirt"
// - Tracking async operation progress via the DataUpload status
func newVMBackupPlugin(logger logrus.FieldLogger) (interface{}, error) {
	return vm.NewBackupPlugin(logger, nil)
}

// newPVCBackupPlugin creates a new PVC BackupItemAction plugin.
func newPVCBackupPlugin(logger logrus.FieldLogger) (interface{}, error) {
	return pvc.NewBackupPlugin(logger, nil)
}

// newVMDeletePlugin creates a new VirtualMachine DeleteItemAction plugin.
func newVMDeletePlugin(logger logrus.FieldLogger) (interface{}, error) {
	return vm.NewDeletePlugin(logger, nil), nil
}

// newPVCRestorePlugin creates a new PersistentVolumeClaim RestoreItemAction plugin.
// This plugin is responsible for:
// - Recognizing PVCs backed up via the kubevirt datamover (AnnotationVMName annotation)
// - Creating DataDownload CRs to trigger the kubevirt datamover controller's restore path
// - Tracking async operation progress via the DataDownload status
func newPVCRestorePlugin(logger logrus.FieldLogger) (interface{}, error) {
	return pvc.NewRestorePlugin(logger, nil)
}

// newVMBackupDiscardPlugin creates a new VirtualMachineBackup/VirtualMachineBackupTracker
// RestoreItemAction plugin that discards these backup-time-only bookkeeping CRs on restore.
func newVMBackupDiscardPlugin(logger logrus.FieldLogger) (interface{}, error) {
	return vmbackup.NewDiscardPlugin(logger), nil
}

// newVMRestorePlugin creates a new VirtualMachine RestoreItemAction plugin.
// This plugin is responsible for:
// - Recognizing VMs backed up via the kubevirt datamover (DataUpload annotation)
// - Halting VMs that were running at backup time, stashing their original run state
// - Tracking async completion of the VM's sibling DataDownloads via the operation ID
func newVMRestorePlugin(logger logrus.FieldLogger) (interface{}, error) {
	return vm.NewRestorePlugin(logger), nil
}
