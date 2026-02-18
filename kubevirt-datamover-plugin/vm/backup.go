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
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	velerov2alpha1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/kuberesource"
	"github.com/vmware-tanzu/velero/pkg/plugin/utils/volumehelper"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"

	kvcore "kubevirt.io/api/core/v1"

	controllercommon "github.com/migtools/kubevirt-datamover-controller/pkg/common"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/clients"
)

// BackupPlugin is a BackupItemAction plugin for VirtualMachine resources
// that handles kubevirt incremental backup via DataUpload CRs.
type BackupPlugin struct {
	Log logrus.FieldLogger
}

// NewBackupPlugin creates a new BackupPlugin instance.
func NewBackupPlugin(log logrus.FieldLogger) *BackupPlugin {
	return &BackupPlugin{Log: log}
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

	// Check preconditions
	eligible, reason, err := p.checkPreconditions(vm, backup)
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
	sourcePVC, err := p.getFirstKubevirtPVC(vm, backup)
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

	// Add DataUpload annotation to VM
	if vm.Annotations == nil {
		vm.Annotations = make(map[string]string)
	}
	vm.Annotations[velerov1.DataUploadNameAnnotation] = dataUpload.Name
	vm.Annotations[controllercommon.AnnotationOperationID] = operationID

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

	return &unstructured.Unstructured{Object: vmMap}, additionalItems, operationID, nil, nil
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

// checkPreconditions verifies that the VirtualMachine meets all requirements
// for kubevirt datamover backup.
func (p *BackupPlugin) checkPreconditions(vm *kvcore.VirtualMachine, backup *velerov1.Backup) (bool, string, error) {
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
	// - At least one PVC must have "skip" action (triggers kubevirt datamover)
	// - No PVC can have "snapshot" action (would conflict with kubevirt datamover)
	// TODO(https://github.com/migtools/kubevirt-datamover-plugin/issues/4): Once upstream
	// Velero changes are merged, update this to check for "custom" action type and inspect
	// the action parameters map to verify it's for kubevirt.
	hasKubevirtPolicy, hasConflictingPolicy, err := p.checkVolumePolicies(vm, backup)
	if err != nil {
		return false, "", fmt.Errorf("failed to check volume policies: %w", err)
	}

	if hasConflictingPolicy {
		return false, "VirtualMachine has PVCs with snapshot volume policy which conflicts with kubevirt datamover", nil
	}

	if !hasKubevirtPolicy {
		return false, "no PVCs with skip volume policy found for kubevirt datamover", nil
	}

	return true, "", nil
}

// checkVolumePolicies examines the volume policies for all PVCs associated with the VM.
// Uses Velero's volumehelper to check resource policies from the configmap.
// Returns:
//   - hasKubevirtPolicy: true if VM is eligible for kubevirt datamover
//   - hasConflictingPolicy: true if any PVC has "snapshot" action (conflicts with kubevirt datamover)
//   - error: if there was an error checking policies
//
// TODO(https://github.com/migtools/kubevirt-datamover-plugin/issues/4): Once upstream
// Velero changes are merged, update hasKubevirtPolicy to check for "custom" action type
// with kubevirt-specific parameter. For now, we assume kubevirt policy is true if there
// are PVCs and none have snapshot policy.
func (p *BackupPlugin) checkVolumePolicies(vm *kvcore.VirtualMachine, backup *velerov1.Backup) (bool, bool, error) {
	// Get all PVCs associated with this VM using controller common function
	pvcNames := controllercommon.GetVolumesForVm(vm)
	if len(pvcNames) == 0 {
		p.Log.Infof("[vm-backup] VirtualMachine %s/%s has no PVCs", vm.Namespace, vm.Name)
		return false, false, nil
	}

	// Get controller-runtime client for volumehelper
	crClient, err := clients.CRClient()
	if err != nil {
		return false, false, fmt.Errorf("failed to get controller-runtime client: %w", err)
	}

	coreClient, err := clients.CoreClient()
	if err != nil {
		return false, false, fmt.Errorf("failed to get core client: %w", err)
	}

	hasConflictingPolicy := false

	for _, pvcName := range pvcNames {
		// Get the PVC
		pvc, err := coreClient.PersistentVolumeClaims(vm.Namespace).Get(context.Background(), pvcName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// PVC doesn't exist yet (might be a DataVolume that hasn't created PVC yet)
				p.Log.Infof("[vm-backup] PVC %s/%s not found, skipping", vm.Namespace, pvcName)
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
		shouldSnapshot, err := volumehelper.ShouldPerformSnapshotWithBackup(
			pvcUnstructured,
			kuberesource.PersistentVolumeClaims,
			*backup,
			crClient,
			p.Log,
		)
		if err != nil {
			return false, false, fmt.Errorf("failed to check volume policy for PVC %s: %w", pvcName, err)
		}

		if shouldSnapshot {
			// This PVC has snapshot policy - conflicts with kubevirt datamover
			hasConflictingPolicy = true
			p.Log.Warnf("[vm-backup] PVC %s/%s has snapshot policy which conflicts with kubevirt datamover", vm.Namespace, pvcName)
		}
	}

	// For now, assume kubevirt policy is true if we have PVCs
	// TODO(https://github.com/migtools/kubevirt-datamover-plugin/issues/4): After upstream
	// changes merge, check for "custom" action with kubevirt-specific parameter here.
	hasKubevirtPolicy := true

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
func (p *BackupPlugin) getFirstKubevirtPVC(vm *kvcore.VirtualMachine, backup *velerov1.Backup) (*corev1.PersistentVolumeClaim, error) {
	pvcNames := controllercommon.GetVolumesForVm(vm)
	if len(pvcNames) == 0 {
		return nil, fmt.Errorf("no PVCs found for VirtualMachine %s/%s", vm.Namespace, vm.Name)
	}

	coreClient, err := clients.CoreClient()
	if err != nil {
		return nil, fmt.Errorf("failed to get core client: %w", err)
	}

	crClient, err := clients.CRClient()
	if err != nil {
		return nil, fmt.Errorf("failed to get controller-runtime client: %w", err)
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

		// Check if this PVC has snapshot policy
		shouldSnapshot, err := volumehelper.ShouldPerformSnapshotWithBackup(
			pvcUnstructured,
			kuberesource.PersistentVolumeClaims,
			*backup,
			crClient,
			p.Log,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to check volume policy for PVC %s: %w", pvcName, err)
		}

		// Return first PVC without snapshot policy
		if !shouldSnapshot {
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
				velerov1.BackupNameLabel: backup.Name,
			},
			Annotations: map[string]string{
				// Use controller common constants for annotations that the controller reads
				controllercommon.AnnotationVMName:      vm.Name,
				controllercommon.AnnotationVMNamespace: vm.Namespace,
				controllercommon.AnnotationOperationID: operationID,
			},
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
		return nil, fmt.Errorf("failed to create DataUpload resource: %w", err)
	}

	return dataUpload, nil
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
			LabelSelector: fmt.Sprintf("%s=%s", velerov1.BackupNameLabel, backup.Name),
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

// updateDataUpload updates a DataUpload CR in the cluster.
func (p *BackupPlugin) updateDataUpload(du *velerov2alpha1.DataUpload) error {
	config, err := clients.GetInClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	dynamicClient, err := getDynamicClient(config)
	if err != nil {
		return fmt.Errorf("failed to get dynamic client: %w", err)
	}

	duMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(du)
	if err != nil {
		return fmt.Errorf("failed to convert DataUpload to unstructured: %w", err)
	}

	unstructuredDU := &unstructured.Unstructured{Object: duMap}

	gvr := schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v2alpha1",
		Resource: "datauploads",
	}

	_, err = dynamicClient.Resource(gvr).Namespace(du.Namespace).Update(
		context.Background(),
		unstructuredDU,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to update DataUpload: %w", err)
	}

	return nil
}

// generateOperationID creates a unique operation ID for tracking async operations.
func generateOperationID(backupName, namespace, vmName string) string {
	return fmt.Sprintf("%s-%s-%s-%s", backupName, namespace, vmName, uuid.New().String()[:8])
}

// generateDataUploadName creates a name for the DataUpload CR.
func generateDataUploadName(backupName, vmName string) string {
	// Ensure name doesn't exceed Kubernetes limits (253 characters)
	name := fmt.Sprintf("du-%s-%s-%s", backupName, vmName, uuid.New().String()[:8])
	if len(name) > 253 {
		name = name[:253]
	}
	return name
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
