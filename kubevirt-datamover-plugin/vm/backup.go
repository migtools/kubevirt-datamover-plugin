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

package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	velerov2alpha1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/kuberesource"
	"github.com/vmware-tanzu/velero/pkg/label"
	"github.com/vmware-tanzu/velero/pkg/plugin/utils/volumehelper"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	podvolumeutil "github.com/vmware-tanzu/velero/pkg/util/podvolume"
	vhutil "github.com/vmware-tanzu/velero/pkg/util/volumehelper"

	kvcore "kubevirt.io/api/core/v1"

	controllercommon "github.com/migtools/kubevirt-datamover-controller/pkg/common"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/clients"
)

// apiCallTimeout bounds each individual Kubernetes API call made by this
// plugin so a stuck API server can't hang a Velero backup indefinitely.
const apiCallTimeout = 30 * time.Second

// BackupPlugin is a BackupItemAction plugin for VirtualMachine resources
// that handles kubevirt incremental backup via DataUpload CRs.
type BackupPlugin struct {
	Log               logrus.FieldLogger
	pluginPVCPodCache PluginPVCPodCache
	crClient          crclient.Client
}

type PluginPVCPodCache struct {
	pvcPodCache *podvolumeutil.PVCPodCache
}

var kubevirtCustomPolicy = map[string]any{
	"datamover": "kubevirt",
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
	return "kubevirt-vm-backup-plugin"
}

// AppliesTo returns a ResourceSelector that determines which resources
// this plugin applies to. This plugin handles VirtualMachine resources.
func (p *BackupPlugin) AppliesTo() (velero.ResourceSelector, error) {
	return velero.ResourceSelector{
		IncludedResources: []string{
			"virtualmachines.kubevirt.io",
		},
	}, nil
}

// Execute is called for each VirtualMachine resource during backup.
// It checks preconditions and creates a DataUpload CR for kubevirt datamover.
//
// Returns:
//   - Modified item (with DataUpload annotation)
//   - Additional items to backup
//   - Operation ID for async progress tracking
//   - Items to backup after operation completes
//   - Error if preconditions are not met
func (p *BackupPlugin) Execute(
	item runtime.Unstructured,
	backup *velerov1.Backup,
) (runtime.Unstructured, []velero.ResourceIdentifier, string, []velero.ResourceIdentifier, error) {
	p.Log.Info("[vm-backup] Executing VirtualMachine backup plugin for kubevirt datamover")

	if backup == nil {
		return nil, nil, "", nil, fmt.Errorf("backup object is nil")
	}

	// Convert unstructured to VirtualMachine
	vm := &kvcore.VirtualMachine{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.UnstructuredContent(), vm); err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to convert item to VirtualMachine: %w", err)
	}

	p.Log.Infof("[vm-backup] Processing VirtualMachine %s/%s", vm.Namespace, vm.Name)

	// Get or create the cached VolumeHelper for this backup
	vh, err := p.pluginPVCPodCache.GetOrCreateVolumeHelper(backup, p.crClient, p.Log)
	if err != nil {
		return nil, nil, "", nil, err
	}

	// Check preconditions
	eligible, reason, err := CheckPreconditions(vm, backup, p.Log, vh)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to check preconditions: %w", err)
	}

	if !eligible {
		p.Log.Infof("[vm-backup] VirtualMachine %s/%s is not eligible for kubevirt datamover: %s",
			vm.Namespace, vm.Name, reason)
		// Return item as-is without creating DataUpload
		// This allows fallback to other backup mechanisms
		return item, nil, "", nil, nil
	}

	// Generate operation ID for async tracking
	operationID := generateOperationID(backup.Name, vm.Namespace, vm.Name)
	p.Log.Infof("[vm-backup] Generated operation ID: %s", operationID)

	// Get the first PVC with skip volume policy for SourcePVC (kubevirt datamover trigger)
	sourcePVC, err := p.getFirstKubevirtPVC(vm, backup, vh)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to get source PVC: %w", err)
	}

	// Create DataUpload CR
	dataUpload, err := p.createDataUpload(vm, backup, operationID, sourcePVC)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to create DataUpload: %w", err)
	}

	p.Log.Infof("[vm-backup] Created DataUpload %s/%s for VirtualMachine %s/%s",
		dataUpload.Namespace, dataUpload.Name, vm.Namespace, vm.Name)

	// Read the operationID back off the returned object rather than trusting the
	// locally-generated value: on the idempotent-reuse path (see createDataUpload)
	// this is an existing DataUpload that could in principle have been created by
	// an older plugin build with a different ID scheme, and Progress/Cancel must be
	// given whatever ID actually matches what's stored on the object.
	returnedOperationID := operationID
	if stored := dataUpload.Annotations[controllercommon.AnnotationOperationID]; stored != "" {
		returnedOperationID = stored
	}

	// Add DataUpload annotation to VM
	if vm.Annotations == nil {
		vm.Annotations = make(map[string]string)
	}
	vm.Annotations[velerov1.DataUploadNameAnnotation] = dataUpload.Name
	vm.Annotations[controllercommon.AnnotationOperationID] = returnedOperationID
	vm.Annotations[velerov1.PVCNamespaceNameLabel] = label.GetValidName(fmt.Sprintf("%s.%s", sourcePVC.Namespace, sourcePVC.Name))

	// Convert back to unstructured
	vmMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(vm)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to convert VirtualMachine to unstructured: %w", err)
	}

	// Return additional items to backup (the DataUpload itself)
	additionalItems := []velero.ResourceIdentifier{
		{
			GroupResource: schema.GroupResource{
				Group:    "velero.io",
				Resource: "datauploads",
			},
			Namespace: dataUpload.Namespace,
			Name:      dataUpload.Name,
		},
	}

	return &unstructured.Unstructured{Object: vmMap}, additionalItems, returnedOperationID, nil, nil
}

// Progress returns the progress of an async backup operation.
// It monitors the DataUpload CR status to report progress.
func (p *BackupPlugin) Progress(operationID string, backup *velerov1.Backup) (velero.OperationProgress, error) {
	p.Log.Infof("[vm-backup] Checking progress for operation %s", operationID)

	progress := velero.OperationProgress{
		Completed:      false,
		OperationUnits: "bytes",
		Description:    "Kubevirt datamover backup in progress",
		Started:        time.Now(),
		Updated:        time.Now(),
	}

	// Get the DataUpload CR to check status
	dataUpload, err := p.getDataUploadByOperationID(operationID, backup)
	if err != nil {
		p.Log.Warnf("[vm-backup] Failed to get DataUpload for operation %s: %v", operationID, err)
		progress.Err = fmt.Sprintf("failed to get DataUpload: %v", err)
		return progress, nil
	}

	if dataUpload == nil {
		p.Log.Errorf("[vm-backup] DataUpload not found for operation %s", operationID)
		progress.Completed = true
		progress.Err = fmt.Sprintf("DataUpload not found for operation %s", operationID)
		progress.Description = "DataUpload not found"
		return progress, nil
	}

	// Update progress based on DataUpload status
	switch dataUpload.Status.Phase {
	case velerov2alpha1.DataUploadPhaseNew:
		progress.Description = "DataUpload created, waiting for processing"
	case velerov2alpha1.DataUploadPhaseAccepted:
		progress.Description = "DataUpload accepted, preparing for backup"
	case velerov2alpha1.DataUploadPhasePrepared:
		progress.Description = "DataUpload prepared, starting data transfer"
	case velerov2alpha1.DataUploadPhaseInProgress:
		progress.Description = "Data transfer in progress"
		progress.NTotal = dataUpload.Status.Progress.TotalBytes
		progress.NCompleted = dataUpload.Status.Progress.BytesDone
	case velerov2alpha1.DataUploadPhaseCompleted:
		progress.Completed = true
		progress.Description = "Backup completed successfully"
		progress.NTotal = dataUpload.Status.Progress.TotalBytes
		progress.NCompleted = dataUpload.Status.Progress.TotalBytes
	case velerov2alpha1.DataUploadPhaseCanceled:
		progress.Completed = true
		progress.Err = "Backup was canceled"
		progress.Description = "Backup canceled"
	case velerov2alpha1.DataUploadPhaseCanceling:
		progress.Description = "Backup is being canceled"
	case velerov2alpha1.DataUploadPhaseFailed:
		progress.Completed = true
		progress.Err = dataUpload.Status.Message
		progress.Description = "Backup failed"
	}

	if dataUpload.Status.StartTimestamp != nil {
		progress.Started = dataUpload.Status.StartTimestamp.Time
	}
	progress.Updated = time.Now()

	p.Log.Infof("[vm-backup] Operation %s progress: phase=%s, completed=%v, bytes=%d/%d",
		operationID, dataUpload.Status.Phase, progress.Completed, progress.NCompleted, progress.NTotal)

	return progress, nil
}

// Cancel requests cancellation of an async backup operation.
func (p *BackupPlugin) Cancel(operationID string, backup *velerov1.Backup) error {
	p.Log.Infof("[vm-backup] Canceling operation %s", operationID)

	dataUpload, err := p.getDataUploadByOperationID(operationID, backup)
	if err != nil {
		return fmt.Errorf("failed to get DataUpload for cancellation: %w", err)
	}

	if dataUpload == nil {
		p.Log.Warnf("[vm-backup] DataUpload not found for operation %s, nothing to cancel", operationID)
		return nil
	}

	// Set cancel flag on DataUpload
	dataUpload.Spec.Cancel = true

	// Update the DataUpload
	if err := p.updateDataUpload(dataUpload); err != nil {
		return fmt.Errorf("failed to update DataUpload for cancellation: %w", err)
	}

	p.Log.Infof("[vm-backup] Requested cancellation for DataUpload %s/%s", dataUpload.Namespace, dataUpload.Name)
	return nil
}

// CheckPreconditions verifies that the VirtualMachine meets all requirements
// for kubevirt datamover backup.
func CheckPreconditions(vm *kvcore.VirtualMachine, backup *velerov1.Backup, log logrus.FieldLogger, vh vhutil.VolumeHelper) (bool, string, error) {
	// Check 1: SnapshotMoveData must be true
	if backup.Spec.SnapshotMoveData == nil || !*backup.Spec.SnapshotMoveData {
		return false, "backup.Spec.SnapshotMoveData is not enabled", nil
	}

	// Check 2: VM must be running (offline backup not supported in initial release)
	// Use validation function from controller common package
	if err := controllercommon.ValidateVMIsRunning(vm); err != nil {
		return false, err.Error(), nil
	}

	// Check 3: ChangedBlockTracking must be enabled
	// Use validation function from controller common package
	if err := controllercommon.ValidateCBTEnabled(vm); err != nil {
		return false, err.Error(), nil
	}

	// Check 4: Volume policy check
	// - At least one PVC must have "custom" action with "datamover=kubevirt" parameter (triggers kubevirt datamover)
	// - No PVC can have "snapshot" action (would conflict with kubevirt datamover)
	hasKubevirtPolicy, hasConflictingPolicy, err := checkVolumePolicies(vm, backup, log, vh)
	if err != nil {
		return false, "", fmt.Errorf("failed to check volume policies: %w", err)
	}

	if !hasKubevirtPolicy {
		return false, "no PVCs with custom kubevirt volume policy found for kubevirt datamover", nil
	}

	if hasConflictingPolicy {
		return false, "VirtualMachine has PVCs with snapshot volume policy which conflicts with kubevirt datamover", nil
	}

	return true, "", nil
}

// checkVolumePolicies examines the volume policies for all PVCs associated with the VM.
// Uses Velero's volumehelper to check resource policies from the configmap.
// Returns:
//   - hasKubevirtPolicy: true if VM is eligible for kubevirt datamover
//   - hasConflictingPolicy: true if any PVC has "snapshot" action (conflicts with kubevirt datamover)
//   - error: if there was an error checking policies
func checkVolumePolicies(vm *kvcore.VirtualMachine, backup *velerov1.Backup, log logrus.FieldLogger, vh vhutil.VolumeHelper) (bool, bool, error) {
	// Get all PVCs associated with this VM using controller common function
	pvcNames := controllercommon.GetVolumesForVm(vm)
	if len(pvcNames) == 0 {
		log.Infof("[vm-backup] VirtualMachine %s/%s has no PVCs", vm.Namespace, vm.Name)
		return false, false, nil
	}

	coreClient, err := clients.CoreClient()
	if err != nil {
		return false, false, fmt.Errorf("failed to get core client: %w", err)
	}

	hasConflictingPolicy := false
	hasKubevirtPolicy := false

	for _, pvcName := range pvcNames {
		// Get the PVC
		pvc, err := coreClient.PersistentVolumeClaims(vm.Namespace).Get(context.Background(), pvcName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// PVC doesn't exist yet (might be a DataVolume that hasn't created PVC yet)
				log.Infof("[vm-backup] PVC %s/%s not found, skipping", vm.Namespace, pvcName)
				continue
			}
			// Other errors (RBAC, timeout, etc.) should be propagated
			return false, false, fmt.Errorf("failed to get PVC %s/%s: %w", vm.Namespace, pvcName, err)
		}

		// Convert PVC to unstructured for volumehelper
		pvcMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pvc)
		if err != nil {
			return false, false, fmt.Errorf("failed to convert PVC to unstructured: %w", err)
		}
		pvcUnstructured := &unstructured.Unstructured{Object: pvcMap}

		// Use Velero's volumehelper to check if this PVC has snapshot policy
		shouldSnapshot, err := vh.ShouldPerformSnapshot(
			pvcUnstructured,
			kuberesource.PersistentVolumeClaims,
		)
		if err != nil {
			return false, false, fmt.Errorf("failed to check volume policy for PVC %s: %w", pvcName, err)
		}

		if shouldSnapshot {
			// This PVC has snapshot policy - conflicts with kubevirt datamover
			hasConflictingPolicy = true
			log.Warnf("[vm-backup] PVC %s/%s has snapshot policy which conflicts with kubevirt datamover", vm.Namespace, pvcName)
		}

		// Use Velero's volumehelper to check if this PVC has snapshot policy
		shouldUseKubevirtDm, err := vh.ShouldPerformCustomAction(
			pvcUnstructured,
			kuberesource.PersistentVolumeClaims,
			kubevirtCustomPolicy,
		)
		if err != nil {
			return false, false, fmt.Errorf("failed to check volume policy for PVC %s: %w", pvcName, err)
		}

		if shouldUseKubevirtDm {
			// This PVC has custom kubevirt policy
			hasKubevirtPolicy = true
			log.Warnf("[vm-backup] PVC %s/%s has kubevirt custom policy for kubevirt datamover", vm.Namespace, pvcName)
		}
	}

	return hasKubevirtPolicy, hasConflictingPolicy, nil
}

// getFirstKubevirtPVC returns the first PVC that doesn't have a snapshot policy.
// This PVC will be used as the SourcePVC in the DataUpload spec.
//
// Current behavior: Returns the first PVC without "snapshot" policy. This ensures we
// don't select a PVC that Velero would handle via CSI snapshot.
//
// TODO(https://github.com/migtools/kubevirt-datamover-plugin/issues/4): After upstream
// Velero changes merge, update this to return the first PVC with "custom" action type
// and kubevirt-specific parameter set. The volumehelper API will need to expose the
// actual action type (not just shouldSnapshot boolean) for this check.
func (p *BackupPlugin) getFirstKubevirtPVC(vm *kvcore.VirtualMachine, backup *velerov1.Backup, vh vhutil.VolumeHelper) (*corev1.PersistentVolumeClaim, error) {
	pvcNames := controllercommon.GetVolumesForVm(vm)
	if len(pvcNames) == 0 {
		return nil, fmt.Errorf("no PVCs found for VirtualMachine %s/%s", vm.Namespace, vm.Name)
	}

	coreClient, err := clients.CoreClient()
	if err != nil {
		return nil, fmt.Errorf("failed to get core client: %w", err)
	}

	for _, pvcName := range pvcNames {
		pvc, err := coreClient.PersistentVolumeClaims(vm.Namespace).Get(context.Background(), pvcName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// PVC doesn't exist yet (might be a DataVolume that hasn't created PVC yet)
				p.Log.Infof("[vm-backup] PVC %s/%s not found, skipping", vm.Namespace, pvcName)
				continue
			}
			// Other errors (RBAC, timeout, etc.) should be propagated
			return nil, fmt.Errorf("failed to get PVC %s/%s: %w", vm.Namespace, pvcName, err)
		}

		// Convert PVC to unstructured for volumehelper
		pvcMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pvc)
		if err != nil {
			return nil, fmt.Errorf("failed to convert PVC to unstructured: %w", err)
		}
		pvcUnstructured := &unstructured.Unstructured{Object: pvcMap}

		// Check if this PVC has custom kubevirt datamover policy
		shouldUseKubevirtDm, err := vh.ShouldPerformCustomAction(
			pvcUnstructured,
			kuberesource.PersistentVolumeClaims,
			kubevirtCustomPolicy,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to check volume policy for PVC %s: %w", pvcName, err)
		}

		// Return first PVC matching kubevirt dm custom policy action
		if shouldUseKubevirtDm {
			return pvc, nil
		}
	}

	return nil, fmt.Errorf("no PVC eligible for kubevirt datamover found for VirtualMachine %s/%s", vm.Namespace, vm.Name)
}

// createDataUpload creates a DataUpload CR for the kubevirt datamover.
func (p *BackupPlugin) createDataUpload(vm *kvcore.VirtualMachine, backup *velerov1.Backup, operationID string, sourcePVC *corev1.PersistentVolumeClaim) (*velerov2alpha1.DataUpload, error) {
	dataUploadName := generateDataUploadName(backup.Name, vm.Name)

	// Get the operation timeout from backup or use default (4 hours)
	// Use ItemOperationTimeout (not CSISnapshotTimeout which is only 10 minutes)
	operationTimeout := metav1.Duration{Duration: 4 * time.Hour}
	if backup.Spec.ItemOperationTimeout.Duration > 0 {
		operationTimeout = backup.Spec.ItemOperationTimeout
	}

	dataUpload := &velerov2alpha1.DataUpload{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "velero.io/v2alpha1",
			Kind:       "DataUpload",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataUploadName,
			Namespace: backup.Namespace,
			Labels: map[string]string{
				velerov1.BackupNameLabel: controllercommon.SafeLabelValue(backup.Name),
			},
			Annotations: buildDataUploadAnnotations(vm, operationID),
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "velero.io/v1",
					Kind:       "Backup",
					Name:       backup.Name,
					UID:        backup.UID,
				},
			},
		},
		Spec: velerov2alpha1.DataUploadSpec{
			// SnapshotType is set to CSI because upstream Velero currently only accepts
			// CSI as a valid enum value. Once Velero adds "kubevirt" as a supported
			// SnapshotType, this should be changed to use a kubevirt-specific value.
			// The DataMover field ("kubevirt") indicates that the kubevirt-datamover-controller
			// should handle this DataUpload rather than Velero's built-in datamover.
			SnapshotType:          velerov2alpha1.SnapshotType(controllercommon.SnapshotTypeCSI),
			DataMover:             controllercommon.DataMoverKubeVirt,
			BackupStorageLocation: backup.Spec.StorageLocation,
			SourceNamespace:       vm.Namespace,
			OperationTimeout:      operationTimeout,
			Cancel:                false,
			// SourcePVC is set to the first PVC with kubevirt volume policy
			SourcePVC: sourcePVC.Name,
		},
	}

	// Create the DataUpload using the Velero client
	if err := p.createDataUploadResource(dataUpload); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("failed to create DataUpload resource: %w", err)
		}
		// A DataUpload with this deterministic name already exists: a prior
		// Execute() call for this same (backup, VM) must have created it, and
		// Velero re-invoked us (e.g. after a transient RPC error). Adopt it
		// instead of erroring, but only after confirming it actually targets the
		// same source PVC we were about to create it for -- a name collision
		// against something else (shouldn't happen given the hash, but cheap to
		// check) must not be silently treated as "our" operation.
		existing, getErr := p.getDataUploadByName(dataUploadName, backup.Namespace)
		if getErr != nil {
			return nil, fmt.Errorf("DataUpload %s/%s already exists but could not be re-fetched: %w", backup.Namespace, dataUploadName, getErr)
		}
		if existing.Spec.SourcePVC != sourcePVC.Name {
			return nil, fmt.Errorf("existing DataUpload %s/%s has SourcePVC %s, not %s -- refusing to reuse it",
				backup.Namespace, dataUploadName, existing.Spec.SourcePVC, sourcePVC.Name)
		}
		p.Log.Infof("[vm-backup] DataUpload %s/%s already exists for this backup+VM, reusing it instead of creating a duplicate",
			backup.Namespace, dataUploadName)
		return existing, nil
	}

	return dataUpload, nil
}

// getDataUploadByName fetches a single DataUpload by name, used to adopt an
// existing one when createDataUploadResource reports AlreadyExists.
func (p *BackupPlugin) getDataUploadByName(name, namespace string) (*velerov2alpha1.DataUpload, error) {
	config, err := clients.GetInClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	dynamicClient, err := getDynamicClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvr := schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v2alpha1",
		Resource: "datauploads",
	}

	item, err := dynamicClient.Resource(gvr).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get DataUpload: %w", err)
	}

	du := &velerov2alpha1.DataUpload{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, du); err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to DataUpload: %w", err)
	}
	return du, nil
}

// createDataUploadResource creates the DataUpload CR in the cluster.
// This is extracted to a method for easier testing/mocking.
var createDataUploadResource = func(p *BackupPlugin, du *velerov2alpha1.DataUpload) error {
	config, err := clients.GetInClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	// Create unstructured client for DataUpload
	duMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(du)
	if err != nil {
		return fmt.Errorf("failed to convert DataUpload to unstructured: %w", err)
	}

	unstructuredDU := &unstructured.Unstructured{Object: duMap}
	unstructuredDU.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "velero.io",
		Version: "v2alpha1",
		Kind:    "DataUpload",
	})

	// Use dynamic client to create the resource
	dynamicClient, err := getDynamicClient(config)
	if err != nil {
		return fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvr := schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v2alpha1",
		Resource: "datauploads",
	}

	_, err = dynamicClient.Resource(gvr).Namespace(du.Namespace).Create(
		context.Background(),
		unstructuredDU,
		metav1.CreateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to create DataUpload: %w", err)
	}

	return nil
}

func (p *BackupPlugin) createDataUploadResource(du *velerov2alpha1.DataUpload) error {
	return createDataUploadResource(p, du)
}

// getDataUploadByOperationID retrieves a DataUpload by its operation ID.
func (p *BackupPlugin) getDataUploadByOperationID(operationID string, backup *velerov1.Backup) (*velerov2alpha1.DataUpload, error) {
	config, err := clients.GetInClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	dynamicClient, err := getDynamicClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvr := schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v2alpha1",
		Resource: "datauploads",
	}

	// List DataUploads in the backup namespace with matching label
	list, err := dynamicClient.Resource(gvr).Namespace(backup.Namespace).List(
		context.Background(),
		metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", velerov1.BackupNameLabel, controllercommon.SafeLabelValue(backup.Name)),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list DataUploads: %w", err)
	}

	for _, item := range list.Items {
		annotations := item.GetAnnotations()
		if annotations != nil && annotations[controllercommon.AnnotationOperationID] == operationID {
			du := &velerov2alpha1.DataUpload{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, du); err != nil {
				return nil, fmt.Errorf("failed to convert unstructured to DataUpload: %w", err)
			}
			return du, nil
		}
	}

	return nil, nil
}

// getOrCreateVolumeHelper returns a VolumeHelper with lazy per-namespace caching.
// The VolumeHelper uses the pvcPodCache which is populated lazily as namespaces are encountered.
// Callers should use ensurePVCPodCacheForNamespace before calling methods that need
// PVC-to-Pod lookups for a specific namespace.
// Since plugin instances are unique per backup (created via newPluginManager and
// cleaned up via CleanupClients at backup completion), we can safely cache this.
func (p *PluginPVCPodCache) GetOrCreateVolumeHelper(backup *velerov1.Backup, crClient crclient.Client, log logrus.FieldLogger) (vhutil.VolumeHelper, error) {
	// Initialize the PVC-to-Pod cache if needed
	if p.pvcPodCache == nil {
		p.pvcPodCache = podvolumeutil.NewPVCPodCache()
	}

	// Return the VolumeHelper with our lazily-built cache
	// The cache will be populated incrementally as namespaces are encountered
	return p.getVolumeHelperWithCache(backup, crClient, log)
}

// getVolumeHelperWithCache creates a VolumeHelper using the pre-built PVC-to-Pod cache.
// The cache should be ensured for the relevant namespace(s) before calling this.
func (p *PluginPVCPodCache) getVolumeHelperWithCache(backup *velerov1.Backup, crClient crclient.Client, log logrus.FieldLogger) (vhutil.VolumeHelper, error) {
	// Create VolumeHelper with our lazy-built cache
	vh, err := volumehelper.NewVolumeHelperWithCache(
		*backup,
		crClient,
		log,
		p.pvcPodCache,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create VolumeHelper: %w", err)
	}
	return vh, nil
}

// cancelPatchBackoff bounds the retry attempts for updateDataUpload's cancel
// patch: Velero's own operation-timeout enforcement
// (backup_operations_controller.go) calls Cancel() at most once and discards
// its returned error, so a single transient failure here would otherwise leave
// the DataUpload (and its upload pod) running with no other mechanism to ever
// retry the cancellation.
var cancelPatchBackoff = wait.Backoff{
	Steps:    3,
	Duration: 200 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.1,
}

// updateDataUpload patches the DataUpload's Spec.Cancel field in the cluster.
// A scoped merge patch (rather than a full Update of the locally-fetched object)
// is used deliberately: the kubevirt datamover controller concurrently
// reconciles this same DataUpload and updates its Status via a full object
// Update, so replacing the whole object from a possibly-stale local copy risks
// clobbering the controller's in-flight status changes.
func (p *BackupPlugin) updateDataUpload(du *velerov2alpha1.DataUpload) error {
	config, err := clients.GetInClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	dynamicClient, err := getDynamicClient(config)
	if err != nil {
		return fmt.Errorf("failed to get dynamic client: %w", err)
	}

	patch, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{
			"cancel": du.Spec.Cancel,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to build DataUpload cancel patch: %w", err)
	}

	gvr := schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v2alpha1",
		Resource: "datauploads",
	}

	retryErr := retry.OnError(cancelPatchBackoff, func(err error) bool {
		// Retrying a not-found or invalid patch can't succeed; only retry
		// errors that are plausibly transient (server hiccup, timeout, etc).
		return !apierrors.IsNotFound(err) && !apierrors.IsInvalid(err)
	}, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
		defer cancel()
		_, patchErr := dynamicClient.Resource(gvr).Namespace(du.Namespace).Patch(
			ctx,
			du.Name,
			types.MergePatchType,
			patch,
			metav1.PatchOptions{},
		)
		return patchErr
	})
	if retryErr != nil {
		return fmt.Errorf("failed to update DataUpload: %w", retryErr)
	}

	return nil
}

// buildDataUploadAnnotations constructs the annotation map for a DataUpload CR.
// It includes required controller annotations and propagates optional per-VM
// annotations (like backup-pvc-size) from the VM to the DataUpload.
func buildDataUploadAnnotations(vm *kvcore.VirtualMachine, operationID string) map[string]string {
	annotations := map[string]string{
		controllercommon.AnnotationVMName:      vm.Name,
		controllercommon.AnnotationVMNamespace: vm.Namespace,
		controllercommon.AnnotationOperationID: operationID,
	}

	// Propagate backup-pvc-size from VM annotation to DataUpload so the
	// controller can use it to size the temporary backup PVC per-VM.
	if vm.Annotations != nil {
		if size := vm.Annotations[controllercommon.AnnotationBackupPVCSize]; size != "" {
			annotations[controllercommon.AnnotationBackupPVCSize] = size
		}
	}

	return annotations
}

// deterministicSuffix returns an 8-character hex digest of parts, joined by "/".
// Used in place of a random UUID for names/IDs that must be stable across a
// retried Execute() call for the same (backup, VM) pair: Velero may re-invoke
// a BackupItemAction after a transient error, and a random suffix would make
// each attempt mint a distinct DataUpload/operationID instead of converging
// on the same one, defeating the AlreadyExists-based idempotency check below.
func deterministicSuffix(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "/")))
	return hex.EncodeToString(sum[:4])
}

// generateOperationID creates a deterministic operation ID for tracking async
// backup operations: the same (backupName, namespace, vmName) always yields
// the same ID, so a retried Execute() call for the same item converges on the
// same DataUpload's annotation rather than one Progress/Cancel can't find.
func generateOperationID(backupName, namespace, vmName string) string {
	return fmt.Sprintf("%s-%s-%s-%s", backupName, namespace, vmName, deterministicSuffix(backupName, namespace, vmName))
}

// generateDataUploadName creates a name for the DataUpload CR.
// The name format is: du-<backupName>-<vmName>-<hash8>, where hash8 is
// deterministic (see deterministicSuffix) rather than random, so a retried
// Execute() call for the same (backup, VM) reproduces the same name and
// Create() surfaces AlreadyExists instead of minting a duplicate.
// If the total length exceeds 253 characters (Kubernetes limit),
// the backupName and vmName are truncated while preserving the hash suffix.
func generateDataUploadName(backupName, vmName string) string {
	suffix := deterministicSuffix(backupName, vmName)
	// Reserve space for: "du-" (3) + "-" (1) + "-" (1) + hash (8) = 13 chars
	const fixedLen = 13
	maxBodyLen := 253 - fixedLen // 240 chars available for backupName + vmName

	// Truncate names if needed, preserving the hash suffix for uniqueness
	totalBodyLen := len(backupName) + len(vmName)
	if totalBodyLen > maxBodyLen {
		// Truncate proportionally, giving slight preference to backupName
		halfMax := maxBodyLen / 2
		if len(backupName) > halfMax && len(vmName) > halfMax {
			// Both are long, truncate both
			backupName = backupName[:halfMax]
			vmName = vmName[:maxBodyLen-halfMax]
		} else if len(backupName) > halfMax {
			// Only backupName is long
			backupName = backupName[:maxBodyLen-len(vmName)]
		} else {
			// Only vmName is long
			vmName = vmName[:maxBodyLen-len(backupName)]
		}
	}

	return fmt.Sprintf("du-%s-%s-%s", backupName, vmName, suffix)
}

// getDynamicClient returns a dynamic client for working with unstructured resources.
// This variable can be overridden for testing.
var getDynamicClient = func(config interface{}) (dynamicClientInterface, error) {
	restConfig, ok := config.(*rest.Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *rest.Config")
	}
	return dynamic.NewForConfig(restConfig)
}

// dynamicClientInterface defines the interface for dynamic client operations.
// This interface matches the dynamic.Interface from k8s.io/client-go/dynamic.
type dynamicClientInterface interface {
	Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface
}

// SetDynamicClientFunc allows overriding the dynamic client creation for testing.
func SetDynamicClientFunc(fn func(config interface{}) (dynamicClientInterface, error)) {
	getDynamicClient = fn
}
