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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	velerov2alpha1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"

	controllercommon "github.com/migtools/kubevirt-datamover-controller/pkg/common"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/clients"
)

// RestorePlugin is a RestoreItemActionV2 plugin for PersistentVolumeClaim resources
// that triggers kubevirt datamover restore via DataDownload CRs.
type RestorePlugin struct {
	Log      logrus.FieldLogger
	crClient crclient.Client

	// backupMu guards cachedBackup. A RestorePlugin instance lives for the
	// duration of a single restore (mirrors vm/backup.go's per-backup
	// PluginPVCPodCache), so the Backup object -- constant for that whole
	// restore -- only needs to be fetched once instead of once per PVC.
	backupMu     sync.Mutex
	cachedBackup *velerov1.Backup
}

// apiCallTimeout bounds each individual Kubernetes API call made by this
// plugin so a stuck API server can't hang a Velero restore indefinitely.
const apiCallTimeout = 30 * time.Second

// NewRestorePlugin creates a new RestorePlugin instance.
func NewRestorePlugin(log logrus.FieldLogger, client crclient.Client) (*RestorePlugin, error) {
	crClient := client
	var err error
	if crClient == nil {
		crClient, err = clients.CRClient()
		if err != nil {
			return nil, fmt.Errorf("failed to get controller-runtime client: %w", err)
		}
	}

	return &RestorePlugin{Log: log, crClient: crClient}, nil
}

// Name returns the plugin name.
func (p *RestorePlugin) Name() string {
	return "kubevirt-pvc-restore-plugin"
}

// AppliesTo returns a ResourceSelector that determines which resources
// this plugin applies to. This plugin handles PersistentVolumeClaim resources.
func (p *RestorePlugin) AppliesTo() (velero.ResourceSelector, error) {
	return velero.ResourceSelector{
		IncludedResources: []string{"persistentvolumeclaims"},
	}, nil
}

// Execute is called for each PersistentVolumeClaim resource during restore.
// It recognizes PVCs that belong to a kubevirt datamover backup (via the
// AnnotationVMName annotation stamped by pvc/backup.go's BackupItemAction)
// and creates a DataDownload CR to trigger the kubevirt datamover controller.
//
// input.ItemFromBackup is used (rather than input.Item) to read the PVC's
// original namespace and annotations: Velero's restore loop calls
// RestoreItemAction.Execute *before* remapping the item's namespace to the
// restore target (pkg/restore/restore.go: itemFromBackup is captured, then
// actions run, then obj.SetNamespace(targetNamespace) happens afterward), so
// both input.Item and input.ItemFromBackup still carry the original,
// pre-remap namespace at this point. The target namespace is computed the
// same way Velero itself resolves it: via restore.Spec.NamespaceMapping.
func (p *RestorePlugin) Execute(input *velero.RestoreItemActionExecuteInput) (*velero.RestoreItemActionExecuteOutput, error) {
	p.Log.Info("[pvc-restore] Executing PersistentVolumeClaim restore plugin for kubevirt datamover")

	restore := input.Restore
	if restore == nil {
		return nil, fmt.Errorf("restore object is nil")
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(input.ItemFromBackup.UnstructuredContent(), pvc); err != nil {
		return nil, fmt.Errorf("failed to convert item to PersistentVolumeClaim: %w", err)
	}

	p.Log.Infof("[pvc-restore] Processing PersistentVolumeClaim %s/%s", pvc.Namespace, pvc.Name)

	vmName := ""
	if pvc.Annotations != nil {
		vmName = pvc.Annotations[controllercommon.AnnotationVMName]
	}
	if vmName == "" {
		p.Log.Infof("[pvc-restore] PersistentVolumeClaim %s/%s has no %s annotation, not a kubevirt datamover PVC",
			pvc.Namespace, pvc.Name, controllercommon.AnnotationVMName)
		return velero.NewRestoreItemActionExecuteOutput(input.Item), nil
	}

	originalNamespace := pvc.Namespace
	targetNamespace := originalNamespace
	if mapped, ok := restore.Spec.NamespaceMapping[originalNamespace]; ok && mapped != "" {
		targetNamespace = mapped
	}

	// Computed before any cluster-mutating call below so a conversion failure
	// here can't leave an already-created DataDownload behind with no
	// OperationID for Velero to ever track or retry.
	updatedItem, err := clearPVCBinding(input.Item)
	if err != nil {
		return nil, err
	}

	backup, err := p.getBackup(restore)
	if err != nil {
		return nil, err
	}
	if backup.Spec.StorageLocation == "" {
		return nil, fmt.Errorf("backup %s/%s has no spec.storageLocation; cannot create DataDownload for PersistentVolumeClaim %s/%s",
			restore.Namespace, restore.Spec.BackupName, pvc.Namespace, pvc.Name)
	}

	operationID := generateOperationID(restore.Name, originalNamespace, pvc.Name)
	p.Log.Infof("[pvc-restore] Generated operation ID: %s", operationID)

	dataDownload, err := p.createDataDownload(restore, backup, pvc, vmName, originalNamespace, targetNamespace, operationID)
	if err != nil {
		return nil, fmt.Errorf("failed to create DataDownload: %w", err)
	}

	p.Log.Infof("[pvc-restore] Created DataDownload %s/%s for PersistentVolumeClaim %s/%s",
		dataDownload.Namespace, dataDownload.Name, targetNamespace, pvc.Name)

	// Read the operationID back off the returned object rather than trusting the
	// locally-generated value: on the idempotent-reuse path (see createDataDownload)
	// this is an existing DataDownload that could in principle have been created by
	// an older plugin build with a different ID scheme, and Progress/Cancel must be
	// given whatever ID actually matches what's stored on the object.
	returnedOperationID := operationID
	if stored := dataDownload.Annotations[controllercommon.AnnotationOperationID]; stored != "" {
		returnedOperationID = stored
	}

	// No AdditionalItems here: unlike the backup-side AdditionalItems (which tells
	// Velero to also *back up* a live cluster resource), RestoreItemAction's
	// AdditionalItems tells Velero to also *restore* an item already present in the
	// backup archive. This DataDownload was created directly against the live
	// cluster above, not sourced from the backup, so there's nothing there for
	// Velero to find. Velero's own built-in CSI PVC restore action follows the same
	// rule: it leaves AdditionalItems empty for the DataDownload it creates, only
	// populating it for the (unrelated) VolumeSnapshot restore path.
	return velero.NewRestoreItemActionExecuteOutput(updatedItem).WithOperationID(returnedOperationID), nil
}

// clearPVCBinding returns a copy of item with the binding-implying fields
// cleared. The backed-up PVC was already bound to its pre-backup PV, but that
// PV is not restored by this flow -- the datamover controller rebinds a new
// scratch PV onto this PVC once the DataDownload completes. Leaving
// Spec.VolumeName/Status.Phase intact would make Velero create the restored
// PVC still carrying that stale binding, which the datamover controller's
// handleAccepted rejects outright ("already bound or requests volume ...,
// which conflicts with restore rebinding") since it can never safely rebind
// an already-bound PVC. Velero's own built-in CSI PVC restore action clears
// the same fields for the analogous reason.
func clearPVCBinding(item runtime.Unstructured) (runtime.Unstructured, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.UnstructuredContent(), pvc); err != nil {
		return nil, fmt.Errorf("failed to convert item to PersistentVolumeClaim: %w", err)
	}

	pvc.Spec.VolumeName = ""
	pvc.Status = corev1.PersistentVolumeClaimStatus{}

	pvcMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pvc)
	if err != nil {
		return nil, fmt.Errorf("failed to convert PersistentVolumeClaim to unstructured: %w", err)
	}
	return &unstructured.Unstructured{Object: pvcMap}, nil
}

// Progress returns the progress of an async restore operation.
// It monitors the DataDownload CR status to report progress.
func (p *RestorePlugin) Progress(operationID string, restore *velerov1.Restore) (velero.OperationProgress, error) {
	p.Log.Infof("[pvc-restore] Checking progress for operation %s", operationID)

	if restore == nil {
		return velero.OperationProgress{}, fmt.Errorf("restore object is nil")
	}

	progress := velero.OperationProgress{
		Completed:      false,
		OperationUnits: "bytes",
		Description:    "Kubevirt datamover restore in progress",
		Started:        time.Now(),
		Updated:        time.Now(),
	}

	dataDownload, err := p.getDataDownloadByOperationID(operationID, restore)
	if err != nil {
		p.Log.Warnf("[pvc-restore] Failed to get DataDownload for operation %s: %v", operationID, err)
		return progress, err
	}

	if dataDownload == nil {
		p.Log.Errorf("[pvc-restore] DataDownload not found for operation %s", operationID)
		progress.Completed = true
		progress.Err = fmt.Sprintf("DataDownload not found for operation %s", operationID)
		progress.Description = "DataDownload not found"
		return progress, nil
	}

	switch dataDownload.Status.Phase {
	case "", velerov2alpha1.DataDownloadPhaseNew:
		progress.Description = "DataDownload created, waiting for processing"
	case velerov2alpha1.DataDownloadPhaseAccepted:
		progress.Description = "DataDownload accepted, preparing for restore"
	case velerov2alpha1.DataDownloadPhasePrepared:
		progress.Description = "DataDownload prepared, starting data transfer"
	case velerov2alpha1.DataDownloadPhaseInProgress:
		progress.Description = "Data transfer in progress"
		progress.NTotal = dataDownload.Status.Progress.TotalBytes
		progress.NCompleted = dataDownload.Status.Progress.BytesDone
	case velerov2alpha1.DataDownloadPhaseCompleted:
		progress.Completed = true
		progress.Description = "Restore completed successfully"
		progress.NTotal = dataDownload.Status.Progress.TotalBytes
		progress.NCompleted = dataDownload.Status.Progress.TotalBytes
	case velerov2alpha1.DataDownloadPhaseCanceled:
		progress.Completed = true
		progress.Err = "Restore was canceled"
		progress.Description = "Restore canceled"
	case velerov2alpha1.DataDownloadPhaseCanceling:
		progress.Description = "Restore is being canceled"
	case velerov2alpha1.DataDownloadPhaseFailed:
		progress.Completed = true
		progress.Err = dataDownload.Status.Message
		if progress.Err == "" {
			progress.Err = fmt.Sprintf("DataDownload %s/%s failed without a status message", dataDownload.Namespace, dataDownload.Name)
		}
		progress.Description = "Restore failed"
	default:
		p.Log.Warnf("[pvc-restore] DataDownload %s/%s has unrecognized phase %q",
			dataDownload.Namespace, dataDownload.Name, dataDownload.Status.Phase)
	}

	if dataDownload.Status.StartTimestamp != nil {
		progress.Started = dataDownload.Status.StartTimestamp.Time
	} else {
		// Before the controller stamps Status.StartTimestamp (New/Accepted/
		// Prepared), fall back to the DataDownload's own creation time rather
		// than time.Now(): the latter would make progress.Started drift forward
		// on every poll instead of reporting a stable operation start.
		progress.Started = dataDownload.CreationTimestamp.Time
	}
	progress.Updated = time.Now()

	p.Log.Infof("[pvc-restore] Operation %s progress: phase=%s, completed=%v, bytes=%d/%d",
		operationID, dataDownload.Status.Phase, progress.Completed, progress.NCompleted, progress.NTotal)

	return progress, nil
}

// Cancel requests cancellation of an async restore operation.
func (p *RestorePlugin) Cancel(operationID string, restore *velerov1.Restore) error {
	p.Log.Infof("[pvc-restore] Canceling operation %s", operationID)

	if restore == nil {
		return fmt.Errorf("restore object is nil")
	}

	dataDownload, err := p.getDataDownloadByOperationID(operationID, restore)
	if err != nil {
		return fmt.Errorf("failed to get DataDownload for cancellation: %w", err)
	}

	if dataDownload == nil {
		p.Log.Warnf("[pvc-restore] DataDownload not found for operation %s, nothing to cancel", operationID)
		return nil
	}

	dataDownload.Spec.Cancel = true

	if err := p.updateDataDownload(dataDownload); err != nil {
		return fmt.Errorf("failed to update DataDownload for cancellation: %w", err)
	}

	p.Log.Infof("[pvc-restore] Requested cancellation for DataDownload %s/%s", dataDownload.Namespace, dataDownload.Name)
	return nil
}

// AreAdditionalItemsReady is not used by this plugin: completion is tracked
// via the async OperationID/Progress mechanism instead of WaitForAdditionalItems.
func (p *RestorePlugin) AreAdditionalItemsReady(additionalItems []velero.ResourceIdentifier, restore *velerov1.Restore) (bool, error) {
	return true, nil
}

// getBackup returns the Backup referenced by restore.Spec.BackupName, fetching
// it at most once per RestorePlugin instance. A restore's BackupName is fixed
// for its whole lifetime, and one plugin instance handles every item in a
// single restore (mirrors vm/backup.go's per-backup caching precedent), so a
// live Get per PVC is pure repeated work otherwise.
func (p *RestorePlugin) getBackup(restore *velerov1.Restore) (*velerov1.Backup, error) {
	p.backupMu.Lock()
	defer p.backupMu.Unlock()

	if p.cachedBackup != nil && p.cachedBackup.Name == restore.Spec.BackupName && p.cachedBackup.Namespace == restore.Namespace {
		return p.cachedBackup, nil
	}

	backup := &velerov1.Backup{}
	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()
	if err := p.crClient.Get(ctx, crclient.ObjectKey{Name: restore.Spec.BackupName, Namespace: restore.Namespace}, backup); err != nil {
		return nil, fmt.Errorf("failed to get Backup %s/%s: %w", restore.Namespace, restore.Spec.BackupName, err)
	}

	p.cachedBackup = backup
	return backup, nil
}

// createDataDownload creates a DataDownload CR for the kubevirt datamover.
func (p *RestorePlugin) createDataDownload(
	restore *velerov1.Restore,
	backup *velerov1.Backup,
	pvc *corev1.PersistentVolumeClaim,
	vmName, originalNamespace, targetNamespace, operationID string,
) (*velerov2alpha1.DataDownload, error) {
	dataDownloadName := generateDataDownloadName(restore.Name, originalNamespace, pvc.Name)

	operationTimeout := metav1.Duration{Duration: 4 * time.Hour}
	if restore.Spec.ItemOperationTimeout.Duration > 0 {
		operationTimeout = restore.Spec.ItemOperationTimeout
	}

	dataDownload := &velerov2alpha1.DataDownload{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "velero.io/v2alpha1",
			Kind:       "DataDownload",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataDownloadName,
			Namespace: restore.Namespace,
			Labels: map[string]string{
				controllercommon.LabelVeleroBackupName:  controllercommon.SafeLabelValue(restore.Spec.BackupName),
				controllercommon.LabelVeleroRestoreName: controllercommon.SafeLabelValue(restore.Name),
				// Also carried as a label (in addition to the annotation below) so
				// getDataDownloadByOperationID can filter server-side via the API
				// server's label selector instead of scanning annotations
				// client-side across every DataDownload for this backup+restore
				// (there can be more than one for a multi-disk VM).
				controllercommon.AnnotationOperationID: controllercommon.SafeLabelValue(operationID),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationVMName:      vmName,
				controllercommon.AnnotationVMNamespace: originalNamespace,
				controllercommon.AnnotationOperationID: operationID,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "velero.io/v1",
					Kind:       "Restore",
					Name:       restore.Name,
					UID:        restore.UID,
					Controller: ptr.To(true),
				},
			},
		},
		Spec: velerov2alpha1.DataDownloadSpec{
			DataMover:             controllercommon.DataMoverKubeVirt,
			BackupStorageLocation: backup.Spec.StorageLocation,
			SourceNamespace:       originalNamespace,
			// SnapshotID is a required DataDownloadSpec field, but it's a Velero
			// native/CSI data mover concept (a CSI snapshot handle from the storage
			// backend) that the kubevirt datamover has no equivalent for and never
			// reads (confirmed: no reference to Spec.SnapshotID anywhere in
			// internal/controller). In its place, kubevirt_datadownload_controller.go's
			// handleAccepted resolves what to restore via four fields already
			// required elsewhere in this spec/annotations: BackupStorageLocation
			// (where), AnnotationVMName+AnnotationVMNamespace (which VM), the
			// backup-name label (which of that VM's backups), and TargetVolume.PVC
			// (which disk of that backup, via resolveTargetDiskName -- a VM backup
			// can span multiple disks). Those four together locate the per-VM
			// manifest, its checkpoint chain, and the specific qcow2 files -- a
			// strictly more granular addressing scheme than a single SnapshotID
			// string would be, since a CSI snapshot handle only ever identifies one
			// volume's one snapshot. So this is a genuinely unused, stable
			// placeholder, not an oversight.
			SnapshotID:       "placeholder-not-used",
			OperationTimeout: operationTimeout,
			Cancel:           false,
			TargetVolume: velerov2alpha1.TargetVolumeSpec{
				PVC:       pvc.Name,
				Namespace: targetNamespace,
			},
		},
	}

	if err := p.createDataDownloadResource(dataDownload); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("failed to create DataDownload resource: %w", err)
		}
		// A DataDownload with this deterministic name already exists: a prior
		// Execute() call for this same (restore, PVC) must have created it, and
		// Velero re-invoked us (e.g. after a transient RPC error). Adopt it
		// instead of erroring, but only after confirming it actually targets the
		// same PVC/namespace we were about to create it for -- a name collision
		// against something else (shouldn't happen given the hash, but cheap to
		// check) must not be silently treated as "our" operation.
		existing, getErr := p.getDataDownloadByName(dataDownloadName, restore.Namespace)
		if getErr != nil {
			return nil, fmt.Errorf("DataDownload %s/%s already exists but could not be re-fetched: %w", restore.Namespace, dataDownloadName, getErr)
		}
		if existing.Spec.TargetVolume.PVC != pvc.Name || existing.Spec.TargetVolume.Namespace != targetNamespace {
			return nil, fmt.Errorf("existing DataDownload %s/%s targets %s/%s, not %s/%s -- refusing to reuse it",
				restore.Namespace, dataDownloadName, existing.Spec.TargetVolume.Namespace, existing.Spec.TargetVolume.PVC, targetNamespace, pvc.Name)
		}
		if !hasOwnerUID(existing.OwnerReferences, restore.UID) {
			// The name hash includes restore.Name but not restore.UID: a deleted
			// and recreated Restore with the same name (unusual, but not
			// impossible) would otherwise let this stale object from a prior
			// Restore be silently adopted as if it belonged to the current one.
			return nil, fmt.Errorf("existing DataDownload %s/%s is not owned by Restore %s (UID %s) -- refusing to reuse it",
				restore.Namespace, dataDownloadName, restore.Name, restore.UID)
		}
		if existing.Spec.SourceNamespace != originalNamespace ||
			existing.Annotations[controllercommon.AnnotationVMNamespace] != originalNamespace ||
			existing.Annotations[controllercommon.AnnotationVMName] != vmName {
			return nil, fmt.Errorf("existing DataDownload %s/%s has source namespace %q / VM %s/%s, not %s/%s -- refusing to reuse it",
				restore.Namespace, dataDownloadName, existing.Spec.SourceNamespace,
				existing.Annotations[controllercommon.AnnotationVMNamespace], existing.Annotations[controllercommon.AnnotationVMName],
				originalNamespace, vmName)
		}
		if existing.Spec.DataMover != controllercommon.DataMoverKubeVirt {
			return nil, fmt.Errorf("existing DataDownload %s/%s uses data mover %q, not %q -- refusing to reuse it",
				restore.Namespace, dataDownloadName, existing.Spec.DataMover, controllercommon.DataMoverKubeVirt)
		}
		if existing.Spec.BackupStorageLocation != backup.Spec.StorageLocation {
			return nil, fmt.Errorf("existing DataDownload %s/%s uses backup storage location %q, not %q -- refusing to reuse it",
				restore.Namespace, dataDownloadName, existing.Spec.BackupStorageLocation, backup.Spec.StorageLocation)
		}
		if existing.Annotations[controllercommon.AnnotationOperationID] == "" {
			// Without this annotation, Execute() would fall back to the locally
			// generated operationID, but getDataDownloadByOperationID (used by
			// Progress/Cancel) requires an exact annotation match to that ID --
			// an existing object with none would never be found again, silently
			// stranding this operation instead of tracking it.
			return nil, fmt.Errorf("existing DataDownload %s/%s has no %s annotation; refusing to reuse it since progress and cancellation could not track it",
				restore.Namespace, dataDownloadName, controllercommon.AnnotationOperationID)
		}
		// getDataDownloadByOperationID narrows server-side with these three
		// labels before its annotation re-check, so a missing or divergent
		// label would make the object unfindable even though the annotation
		// above matches.
		wantLabels := map[string]string{
			controllercommon.LabelVeleroBackupName:  controllercommon.SafeLabelValue(restore.Spec.BackupName),
			controllercommon.LabelVeleroRestoreName: controllercommon.SafeLabelValue(restore.Name),
			controllercommon.AnnotationOperationID:  controllercommon.SafeLabelValue(existing.Annotations[controllercommon.AnnotationOperationID]),
		}
		for key, want := range wantLabels {
			if existing.Labels[key] != want {
				return nil, fmt.Errorf("existing DataDownload %s/%s has label %s=%q, expected %q; refusing to reuse it since progress and cancellation could not find it",
					restore.Namespace, dataDownloadName, key, existing.Labels[key], want)
			}
		}
		p.Log.Infof("[pvc-restore] DataDownload %s/%s already exists for this restore+PVC, reusing it instead of creating a duplicate",
			restore.Namespace, dataDownloadName)
		return existing, nil
	}

	return dataDownload, nil
}

// getDataDownloadByName fetches a single DataDownload by name, used to adopt
// an existing one when createDataDownloadResource reports AlreadyExists.
func (p *RestorePlugin) getDataDownloadByName(name, namespace string) (*velerov2alpha1.DataDownload, error) {
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
		Resource: "datadownloads",
	}

	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()

	item, err := dynamicClient.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get DataDownload: %w", err)
	}

	dd := &velerov2alpha1.DataDownload{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, dd); err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to DataDownload: %w", err)
	}
	return dd, nil
}

// createDataDownloadResource creates the DataDownload CR in the cluster.
func (p *RestorePlugin) createDataDownloadResource(dd *velerov2alpha1.DataDownload) error {
	config, err := clients.GetInClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	ddMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dd)
	if err != nil {
		return fmt.Errorf("failed to convert DataDownload to unstructured: %w", err)
	}

	unstructuredDD := &unstructured.Unstructured{Object: ddMap}
	unstructuredDD.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "velero.io",
		Version: "v2alpha1",
		Kind:    "DataDownload",
	})

	dynamicClient, err := getDynamicClient(config)
	if err != nil {
		return fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvr := schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v2alpha1",
		Resource: "datadownloads",
	}

	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()

	_, err = dynamicClient.Resource(gvr).Namespace(dd.Namespace).Create(
		ctx,
		unstructuredDD,
		metav1.CreateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to create DataDownload: %w", err)
	}

	return nil
}

// getDataDownloadByOperationID retrieves a DataDownload by its operation ID.
func (p *RestorePlugin) getDataDownloadByOperationID(operationID string, restore *velerov1.Restore) (*velerov2alpha1.DataDownload, error) {
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
		Resource: "datadownloads",
	}

	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()

	list, err := dynamicClient.Resource(gvr).Namespace(restore.Namespace).List(
		ctx,
		metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s=%s",
				controllercommon.LabelVeleroBackupName, controllercommon.SafeLabelValue(restore.Spec.BackupName),
				controllercommon.LabelVeleroRestoreName, controllercommon.SafeLabelValue(restore.Name),
				controllercommon.AnnotationOperationID, controllercommon.SafeLabelValue(operationID)),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list DataDownloads: %w", err)
	}

	// The label selector above already narrows to the exact operation, but the
	// annotation holds the untruncated operationID (SafeLabelValue can hash-truncate
	// long values for the label) -- re-check it as a defensive exact match rather
	// than trusting the (extremely unlikely) possibility of a truncation collision.
	for _, item := range list.Items {
		annotations := item.GetAnnotations()
		if annotations != nil && annotations[controllercommon.AnnotationOperationID] == operationID {
			dd := &velerov2alpha1.DataDownload{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, dd); err != nil {
				return nil, fmt.Errorf("failed to convert unstructured to DataDownload: %w", err)
			}
			return dd, nil
		}
	}

	return nil, nil
}

// cancelPatchBackoff bounds the retry attempts for updateDataDownload's cancel
// patch: Velero's own operation-timeout enforcement
// (restore_operations_controller.go) calls Cancel() at most once and discards
// its returned error, so a single transient failure here would otherwise leave
// the DataDownload (and its downloader pod) running with no other mechanism
// to ever retry the cancellation.
var cancelPatchBackoff = wait.Backoff{
	Steps:    3,
	Duration: 200 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.1,
}

// cancelPatchTotalTimeout bounds the entire updateDataDownload retry loop
// (all attempts and backoff sleeps combined), not just each individual patch
// call: since Velero calls Cancel() synchronously and only once, an unbounded
// total (worst case: 3 attempts x apiCallTimeout each, plus backoff sleeps)
// would leave that single call hanging far longer than a cancellation should
// reasonably take.
const cancelPatchTotalTimeout = 45 * time.Second

// updateDataDownload patches the DataDownload's Spec.Cancel field in the cluster.
// A scoped merge patch (rather than a full Update of the locally-fetched object) is
// used deliberately: the kubevirt datamover controller concurrently reconciles this
// same DataDownload and updates its Status via a full object Update, so replacing
// the whole object from a possibly-stale local copy risks clobbering the
// controller's in-flight status changes.
func (p *RestorePlugin) updateDataDownload(dd *velerov2alpha1.DataDownload) error {
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
			"cancel": dd.Spec.Cancel,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to build DataDownload cancel patch: %w", err)
	}

	gvr := schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v2alpha1",
		Resource: "datadownloads",
	}

	parentCtx, cancelParent := context.WithTimeout(context.Background(), cancelPatchTotalTimeout)
	defer cancelParent()

	retryErr := retry.OnError(cancelPatchBackoff, func(err error) bool {
		// Retrying a not-found or invalid patch can't succeed; only retry
		// errors that are plausibly transient (server hiccup, timeout, etc).
		return !apierrors.IsNotFound(err) && !apierrors.IsInvalid(err)
	}, func() error {
		ctx, cancel := context.WithTimeout(parentCtx, apiCallTimeout)
		defer cancel()
		_, patchErr := dynamicClient.Resource(gvr).Namespace(dd.Namespace).Patch(
			ctx,
			dd.Name,
			types.MergePatchType,
			patch,
			metav1.PatchOptions{},
		)
		return patchErr
	})
	if retryErr != nil {
		return fmt.Errorf("failed to update DataDownload: %w", retryErr)
	}

	return nil
}

// hasOwnerUID reports whether any of ownerReferences has the given UID.
// Used to confirm an adopted object actually belongs to the Restore/Backup
// being processed, not to a prior object of the same name left behind by a
// deleted-and-recreated Restore/Backup.
func hasOwnerUID(ownerReferences []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range ownerReferences {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// Used in place of a random UUID for names/IDs that must be stable across a
// retried Execute() call for the same (restore, PVC) pair: Velero may re-invoke
// a RestoreItemAction after a transient error, and a random suffix would make
// each attempt mint a distinct DataDownload/operationID instead of converging
// on the same one, defeating the AlreadyExists-based idempotency check below.
func deterministicSuffix(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "/")))
	return hex.EncodeToString(sum[:4])
}

// generateOperationID creates a deterministic operation ID for tracking async
// restore operations: the same (restoreName, originalNamespace, pvcName)
// always yields the same ID, so a retried Execute() call for the same item
// converges on the same DataDownload's annotation rather than one
// Progress/Cancel can't find. originalNamespace (not the restore-remapped
// target namespace) is used so identity is tied to the actual source PVC,
// not to a NamespaceMapping value the plugin doesn't control the injectivity of.
func generateOperationID(restoreName, namespace, pvcName string) string {
	return fmt.Sprintf("%s-%s-%s-%s", restoreName, namespace, pvcName, deterministicSuffix(restoreName, namespace, pvcName))
}

// distributeTruncationBudget returns, for each of parts, the max byte length
// it may occupy so the sum is at most maxLen. Parts that already fit keep
// their full length; any deficit is taken evenly off the longest part(s),
// repeated until the total fits. Used instead of a fixed halfway split so the
// same logic works regardless of how many name components are involved.
func distributeTruncationBudget(maxLen int, parts []string) []int {
	budgets := make([]int, len(parts))
	for i, p := range parts {
		budgets[i] = len(p)
	}
	for {
		total := 0
		for _, b := range budgets {
			total += b
		}
		over := total - maxLen
		if over <= 0 {
			return budgets
		}
		largest := 0
		for _, b := range budgets {
			if b > largest {
				largest = b
			}
		}
		if largest == 0 {
			return budgets
		}
		for i := range budgets {
			if over <= 0 {
				break
			}
			if budgets[i] == largest {
				budgets[i]--
				over--
			}
		}
	}
}

// generateDataDownloadName creates a name for the DataDownload CR.
// The name format is: dd-<restoreName>-<namespace>-<pvcName>-<hash8>, where
// hash8 is deterministic (see deterministicSuffix) rather than random, so a
// retried Execute() call for the same (restore, namespace, PVC) reproduces
// the same name and Create() surfaces AlreadyExists instead of minting a
// duplicate. namespace is the PVC's original (pre-remap) source namespace,
// not the restore-remapped target namespace -- using the source identity
// disambiguates same-named PVCs restored from different source namespaces
// within one restore without depending on restore.Spec.NamespaceMapping
// being injective (which nothing enforces).
// If the total length exceeds 253 characters (Kubernetes limit), the three
// components are truncated (see distributeTruncationBudget) while preserving
// the hash suffix.
func generateDataDownloadName(restoreName, namespace, pvcName string) string {
	suffix := deterministicSuffix(restoreName, namespace, pvcName)
	// Reserve space for: "dd-" (3) + "-" + "-" + "-" (3) + hash (8) = 14 chars
	const fixedLen = 14
	maxBodyLen := 253 - fixedLen

	parts := []string{restoreName, namespace, pvcName}
	totalBodyLen := len(restoreName) + len(namespace) + len(pvcName)
	if totalBodyLen > maxBodyLen {
		budgets := distributeTruncationBudget(maxBodyLen, parts)
		for i, b := range budgets {
			if b < len(parts[i]) {
				// A truncation boundary landing mid-name can leave a trailing "-"
				// or "." (invalid at a DNS-1123 segment boundary); trim it,
				// matching the same truncation-hygiene rule the controller's
				// SafeResourceName helper uses.
				parts[i] = strings.TrimRight(parts[i][:b], "-.")
			}
		}
		restoreName, namespace, pvcName = parts[0], parts[1], parts[2]
	}

	return fmt.Sprintf("dd-%s-%s-%s-%s", restoreName, namespace, pvcName, suffix)
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
