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
	"context"
	"fmt"
	"sync"


	controllercommon "github.com/migtools/kubevirt-datamover-controller/pkg/common"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/clients"
	vmplugin "github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/vm"
	"github.com/sirupsen/logrus"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"

	kvcore "kubevirt.io/api/core/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// BackupPlugin is a BackupItemAction plugin for PersistentVolumeClaim resources.
type BackupPlugin struct {
	Log logrus.FieldLogger
	// map[namespace]->[map[vmVolumes]->[]vmName]
	nsPVCs map[string]map[string][]string
	lock  sync.Mutex
	
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
	// Get VMs for this volume, if any
	crClient, err := clients.CRClient()
	if err != nil {
		return item, nil, "", nil, fmt.Errorf("failed to get controller-runtime client: %w", err)
	}
	pvcs, err := p.getPVCList(crClient, pvc.Namespace)
	if err != nil {
		return item, nil, "", nil, fmt.Errorf("failed to get PVC list: %w", err)
	}
	kubevirtDMVM := ""
	vmNames := pvcs[pvc.Name]
	for _, vmName := range vmNames {
		vm := new(kvcore.VirtualMachine)
		err := crClient.Get(context.Background(), crclient.ObjectKey{Name: vmName, Namespace: pvc.Namespace}, vm)
		if err != nil {
			return item, nil, "", nil, fmt.Errorf("failed to get VM %s: %w", vmName, err)
		}
		eligible, _, err := vmplugin.CheckPreconditions(vm, backup, p.Log)
		if err != nil {
			return item, nil, "", nil, fmt.Errorf("failed to check preconditions: %w", err)
		}
		if eligible {
			kubevirtDMVM = vmName
			break
		}
	}
	// return without further action if this volume isn't attached to a kubevirt DM VM
	if kubevirtDMVM == "" {
		return item, nil, "", nil, nil
	}
	// If this PVC is shared across multiple VMs and is supposed to use kubevirt DM, error out
	if len(vmNames) > 1 {
		return item, nil, "", nil, fmt.Errorf("Kubevirt datamover does not support volumes shared across VMs. Use velero datamover for %v", pvc.Name)
	}
	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
	}
	p.Log.Infof("[pvc-backup] Adding VMName annotation %s to PersistentVolumeClaim %s/%s", kubevirtDMVM, pvc.Namespace, pvc.Name)
	pvc.Annotations[controllercommon.AnnotationVMName] = kubevirtDMVM
	// Convert back to unstructured
	pvcMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pvc)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to convert PVC to unstructured: %w", err)
	}
	
	return &unstructured.Unstructured{Object: pvcMap}, nil, "", nil, nil
}

func (p *BackupPlugin) getPVCList(crClient crclient.Client, ns string) (map[string][]string, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if p.nsPVCs == nil {
		p.nsPVCs = make(map[string]map[string][]string)
	}
	pvcList, ok := p.nsPVCs[ns]
	if ok {
		return pvcList, nil
	}
	pvcList = make(map[string][]string)
	vms := new(kvcore.VirtualMachineList)
	err := crClient.List(context.Background(), vms, crclient.InNamespace(ns))
	if err != nil {
		return nil, fmt.Errorf("failed to list VMs: %w", err)
	}
	for i := range vms.Items {
		pvcNames := controllercommon.GetVolumesForVm(&vms.Items[i])
		for _, volume := range pvcNames {
			pvcList[volume] = append(pvcList[volume], vms.Items[i].Name)
		}
	}
	p.nsPVCs[ns] = pvcList
	return pvcList, nil
}

// Progress is not implemented for this plugin.
func (p *BackupPlugin) Progress(operationID string, backup *velerov1.Backup) (velero.OperationProgress, error) {
	return velero.OperationProgress{}, nil
}

// Cancel is not implemented for this plugin.
func (p *BackupPlugin) Cancel(operationID string, backup *velerov1.Backup) error {
	return nil
}
