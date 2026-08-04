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

// Package vmbackup contains a RestoreItemAction that discards
// VirtualMachineBackup and VirtualMachineBackupTracker resources on restore.
package vmbackup

import (
	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

// DiscardPlugin is a RestoreItemAction that prevents VirtualMachineBackup and
// VirtualMachineBackupTracker resources from being restored. These are
// backup-time-only bookkeeping CRs owned by the kubevirt datamover controller;
// restoring them would risk re-triggering backup logic on the restore target.
type DiscardPlugin struct {
	Log logrus.FieldLogger
}

// NewDiscardPlugin creates a new DiscardPlugin instance.
func NewDiscardPlugin(log logrus.FieldLogger) *DiscardPlugin {
	return &DiscardPlugin{Log: log}
}

// AppliesTo returns a ResourceSelector that determines which resources
// this plugin applies to: VirtualMachineBackup and VirtualMachineBackupTracker.
func (p *DiscardPlugin) AppliesTo() (velero.ResourceSelector, error) {
	return velero.ResourceSelector{
		IncludedResources: []string{
			"virtualmachinebackups.backup.kubevirt.io",
			"virtualmachinebackuptrackers.backup.kubevirt.io",
		},
	}, nil
}

// Execute always skips restoring the item: VirtualMachineBackup and
// VirtualMachineBackupTracker resources are backup-time bookkeeping only.
func (p *DiscardPlugin) Execute(input *velero.RestoreItemActionExecuteInput) (*velero.RestoreItemActionExecuteOutput, error) {
	item := &unstructured.Unstructured{Object: input.ItemFromBackup.UnstructuredContent()}
	p.Log.Infof("[vmbackup-discard] Discarding %s %s/%s on restore",
		item.GetKind(), item.GetNamespace(), item.GetName())

	return velero.NewRestoreItemActionExecuteOutput(input.Item).WithoutRestore(), nil
}
