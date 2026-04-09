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

package pvc

import (
	"fmt"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

// BackupPlugin is a BackupItemAction plugin for PersistentVolumeClaim resources.
type BackupPlugin struct {
	Log logrus.FieldLogger
}

// NewBackupPlugin creates a new BackupPlugin instance.
func NewBackupPlugin(log logrus.FieldLogger) *BackupPlugin {
	return &BackupPlugin{Log: log}
}

// Name returns the plugin name.
func (p *BackupPlugin) Name() string {
	return "kubevirt-pvc-backup-plugin"
}

// AppliesTo returns a ResourceSelector that determines which resources
// this plugin applies to. This plugin handles PersistentVolumeClaim resources.
func (p *BackupPlugin) AppliesTo() (velero.ResourceSelector, error) {
	return velero.ResourceSelector{
		IncludedResources: []string{"persistentvolumeclaims"},
	}, nil
}

// Execute is called for each PersistentVolumeClaim resource during backup.
func (p *BackupPlugin) Execute(
	item runtime.Unstructured,
	backup *velerov1.Backup,
) (runtime.Unstructured, []velero.ResourceIdentifier, string, []velero.ResourceIdentifier, error) {
	p.Log.Info("[pvc-backup] Executing PersistentVolumeClaim backup plugin for kubevirt datamover")

	if backup == nil {
		return nil, nil, "", nil, fmt.Errorf("backup object is nil")
	}

	// Convert unstructured to PersistentVolumeClaim
	pvc := &corev1.PersistentVolumeClaim{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.UnstructuredContent(), pvc); err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to convert item to PersistentVolumeClaim: %w", err)
	}

	p.Log.Infof("[pvc-backup] Processing PersistentVolumeClaim %s/%s", pvc.Namespace, pvc.Name)

	return item, nil, "", nil, nil
}

// Progress is not implemented for this plugin.
func (p *BackupPlugin) Progress(operationID string, backup *velerov1.Backup) (velero.OperationProgress, error) {
	return velero.OperationProgress{}, nil
}

// Cancel is not implemented for this plugin.
func (p *BackupPlugin) Cancel(operationID string, backup *velerov1.Backup) error {
	return nil
}
