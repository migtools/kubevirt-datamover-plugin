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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	kvcore "kubevirt.io/api/core/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type kubevirtInstallState uint8

const (
	kubevirtInstallUnknown kubevirtInstallState = iota
	kubevirtInstallMissing
	kubevirtInstallPresent
)

// BackupPlugin is a BackupItemAction plugin for PersistentVolumeClaim resources.
type BackupPlugin struct {
	Log logrus.FieldLogger
	// map[namespace]->[map[vmVolumes]->[]vmName]
	nsPVCs                 map[string]map[string][]string
	lock                   sync.Mutex
	pluginPVCPodCache      vmplugin.PluginPVCPodCache
	crClient               crclient.Client
	kubevirtInstall        kubevirtInstallState
	checkKubeVirtInstalled func() (bool, error) // tests only
}

// NewBackupPlugin creates a new BackupPlugin instance.
func NewBackupPlugin(log logrus.FieldLogger, client *crclient.Client) (*BackupPlugin, error) {
	var crClient crclient.Client
	var err error
	if client != nil {
		crClient = *client
	} else {
		crClient, err = clients.CRClient()
		if err != nil {
			return nil, fmt.Errorf("failed to get controller-runtime client: %w", err)
		}
	}

	return &BackupPlugin{Log: log, crClient: crClient}, nil
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
	pvcs, err := p.getPVCList(p.crClient, pvc.Namespace)
	if err != nil {
		return item, nil, "", nil, fmt.Errorf("failed to get PVC list: %w", err)
	}
	vmNames := pvcs[pvc.Name]
	if len(vmNames) == 0 {
		return item, nil, "", nil, nil
	}

	kubevirtDMVM := ""
	vh, err := p.pluginPVCPodCache.GetOrCreateVolumeHelper(backup, p.crClient, p.Log)
	if err != nil {
		return item, nil, "", nil, err
	}
	for _, vmName := range vmNames {
		vm := new(kvcore.VirtualMachine)
		err := p.crClient.Get(context.Background(), crclient.ObjectKey{Name: vmName, Namespace: pvc.Namespace}, vm)
		if err != nil {
			return item, nil, "", nil, fmt.Errorf("failed to get VM %s: %w", vmName, err)
		}
		eligible, _, err := vmplugin.CheckPreconditions(vm, backup, p.Log, vh)
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

	listVMs, err := p.shouldListVMsLocked()
	if err != nil {
		return nil, err
	}
	if !listVMs {
		return map[string][]string{}, nil
	}

	if p.nsPVCs == nil {
		p.nsPVCs = make(map[string]map[string][]string)
	}
	pvcList, ok := p.nsPVCs[ns]
	if ok {
		return pvcList, nil
	}

	pvcList = make(map[string][]string)
	vms := new(kvcore.VirtualMachineList)
	if err := crClient.List(context.Background(), vms, crclient.InNamespace(ns)); err != nil {
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

// shouldListVMsLocked runs the once-per-backup discovery check.
// Must be called with p.lock held.
func (p *BackupPlugin) shouldListVMsLocked() (bool, error) {
	switch p.kubevirtInstall {
	case kubevirtInstallMissing:
		return false, nil
	case kubevirtInstallPresent:
		return true, nil
	default:
		installed, err := p.kubevirtInstalled()
		if err != nil {
			return false, fmt.Errorf("failed to detect kubevirt API: %w", err)
		}
		if !installed {
			p.kubevirtInstall = kubevirtInstallMissing
			p.Log.Warnf("[pvc-backup] kubevirt.io API group not found in cluster discovery; skipping VM ownership lookup for PVCs in this backup (kubevirt-datamover PVC annotations will not be applied)")
			return false, nil
		}
		p.kubevirtInstall = kubevirtInstallPresent
		return true, nil
	}
}

func (p *BackupPlugin) kubevirtInstalled() (bool, error) {
	if p.checkKubeVirtInstalled != nil {
		return p.checkKubeVirtInstalled()
	}
	dc, err := newDiscoveryClient()
	if err != nil {
		return false, err
	}
	return kubevirtGroupInstalled(dc)
}

var newDiscoveryClient = func() (discovery.ServerGroupsInterface, error) {
	cfg, err := clients.GetInClusterConfig()
	if err != nil {
		return nil, err
	}
	return discovery.NewDiscoveryClientForConfig(cfg)
}

func kubevirtGroupInstalled(dc discovery.ServerGroupsInterface) (bool, error) {
	groups, err := dc.ServerGroups()
	if err != nil {
		return false, err
	}
	for i := range groups.Groups {
		if groups.Groups[i].Name == kvcore.GroupVersion.Group {
			return true, nil
		}
	}
	return false, nil
}

// Progress is not implemented for this plugin.
func (p *BackupPlugin) Progress(operationID string, backup *velerov1.Backup) (velero.OperationProgress, error) {
	return velero.OperationProgress{}, nil
}

// Cancel is not implemented for this plugin.
func (p *BackupPlugin) Cancel(operationID string, backup *velerov1.Backup) error {
	return nil
}
