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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	velerov2alpha1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	riav2 "github.com/vmware-tanzu/velero/pkg/plugin/velero/restoreitemaction/v2"

	kvcore "kubevirt.io/api/core/v1"

	controllercommon "github.com/migtools/kubevirt-datamover-controller/pkg/common"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/clients"
)

// Annotations stashed on the VirtualMachine at restore time so the
// kubevirt-datamover-controller can restore the exact pre-restore run state
// once this VM's DataDownload(s) complete.
//
// These are plugin-local (not yet promoted to controllercommon) pending
// agreement with the kdm-controller side on the contract -- the string
// values below ARE that contract and must not change without updating the
// controller's flip-back logic in lockstep.
const (
	AnnotationOriginalRunStrategy       = "kubevirt-datamover.io/original-run-strategy"
	AnnotationOriginalRunStrategySource = "kubevirt-datamover.io/original-run-strategy-source"

	runStrategySourceRunStrategy = "runStrategy"
	runStrategySourceRunning     = "running"
)

// dataDownloadGVR identifies the Velero DataDownload custom resource that
// getDataDownloadsForVM and patchDataDownloadCancel operate on via the
// dynamic client.
var dataDownloadGVR = schema.GroupVersionResource{
	Group:    "velero.io",
	Version:  "v2alpha1",
	Resource: "datadownloads",
}

// firstDataDownloadGracePeriod bounds how long Progress will wait, from the
// restore's own start time, for this VM's first sibling DataDownload to
// appear before giving up. An empty list usually just means the sibling PVC
// plugin hasn't gotten to this VM's PVCs yet, but it can also mean none of
// them ended up in this restore at all (e.g. excluded by resource/namespace
// filtering) -- in which case no DataDownload will ever appear, and waiting
// out Velero's own (much longer) per-operation timeout instead would delay
// surfacing that failure far more than necessary.
const firstDataDownloadGracePeriod = 10 * time.Minute

// newDataDownloadClient builds a dynamic client for DataDownload access.
// Callers that need more than one DataDownload API call within a single
// Progress/Cancel invocation (e.g. Cancel's per-sibling patch loop) should
// build it once and reuse it, rather than rebuilding it per call.
func newDataDownloadClient() (dynamicClientInterface, error) {
	config, err := clients.GetInClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}
	return getDynamicClient(config)
}

// RestorePlugin is a RestoreItemActionV2 plugin for VirtualMachine resources
// that halts a kubevirt-datamover-backed VM at restore time. This closes a
// race where KubeVirt's virt-controller creates a virt-launcher pod the
// instant the restored VM comes up running, which can consume its PVC and
// let a WaitForFirstConsumer StorageClass bind a wrong PV before this VM's
// DataDownload(s) have rebound it -- the datamover controller's
// handleAccepted then rejects the PVC as already-bound. The
// kubevirt-datamover-controller flips the VM back to its stashed original
// run state once all of this VM's DataDownloads reach Completed.
type RestorePlugin struct {
	Log logrus.FieldLogger
}

// Compile-time assertion that RestorePlugin implements the full
// RestoreItemActionV2 interface, so a signature drift on any method fails
// the build instead of surfacing as a runtime plugin-registration error.
var _ riav2.RestoreItemAction = &RestorePlugin{}

// NewRestorePlugin creates a new VirtualMachine RestorePlugin instance.
func NewRestorePlugin(log logrus.FieldLogger) *RestorePlugin {
	return &RestorePlugin{Log: log}
}

// Name returns the plugin name.
func (p *RestorePlugin) Name() string {
	return "kubevirt-vm-restore-plugin"
}

// AppliesTo returns a ResourceSelector that determines which resources this
// plugin applies to. This plugin handles VirtualMachine resources.
func (p *RestorePlugin) AppliesTo() (velero.ResourceSelector, error) {
	return velero.ResourceSelector{
		IncludedResources: []string{"virtualmachines.kubevirt.io"},
	}, nil
}

// Execute is called for each VirtualMachine resource during restore. It
// halts VMs that were backed up via the kubevirt datamover (identified by
// the DataUpload annotation vm/backup.go stamps on success) and were
// running at backup time, stashing their original run state in annotations
// for the controller to restore once the sibling DataDownload(s) complete.
func (p *RestorePlugin) Execute(input *velero.RestoreItemActionExecuteInput) (*velero.RestoreItemActionExecuteOutput, error) {
	p.Log.Info("[vm-restore] Executing VirtualMachine restore plugin for kubevirt datamover")

	restore := input.Restore
	if restore == nil {
		return nil, fmt.Errorf("restore object is nil")
	}

	vm := &kvcore.VirtualMachine{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(input.Item.UnstructuredContent(), vm); err != nil {
		return nil, fmt.Errorf("failed to convert item to VirtualMachine: %w", err)
	}

	p.Log.Infof("[vm-restore] Processing VirtualMachine %s/%s", vm.Namespace, vm.Name)

	// Only intervene for VMs that were actually backed up via the kubevirt
	// datamover VM BackupItemAction (vm/backup.go stamps this annotation on
	// success). A VM restored via an ordinary CSI snapshot has no datamover
	// DataDownload to ever wait for, so halting it here would strand it
	// halted forever with nothing to flip it back.
	if vm.Annotations == nil || vm.Annotations[velerov1.DataUploadNameAnnotation] == "" {
		p.Log.Infof("[vm-restore] VirtualMachine %s/%s has no %s annotation, not a kubevirt datamover VM",
			vm.Namespace, vm.Name, velerov1.DataUploadNameAnnotation)
		return velero.NewRestoreItemActionExecuteOutput(input.Item), nil
	}

	source, value, running := originalRunState(vm)
	if !running {
		// VM was already not auto-starting at backup time: nothing can race a
		// launcher pod into existence, so there's nothing for this plugin to
		// protect against.
		p.Log.Infof("[vm-restore] VirtualMachine %s/%s was not set to auto-start at backup time, no restore-time hold needed",
			vm.Namespace, vm.Name)
		return velero.NewRestoreItemActionExecuteOutput(input.Item), nil
	}

	updatedItem, err := haltVM(input.Item, source, value)
	if err != nil {
		return nil, err
	}

	operationID := generateVMRestoreOperationID(restore.Name, vm.Namespace, vm.Name)
	p.Log.Infof("[vm-restore] Generated operation ID: %s", operationID)

	return velero.NewRestoreItemActionExecuteOutput(updatedItem).WithOperationID(operationID), nil
}

// autoStarts reports whether a VirtualMachineRunStrategy causes KubeVirt's
// virt-controller to automatically create a VirtualMachineInstance (and
// thus a virt-launcher pod) without any further user action. Manual and
// Halted never auto-start; WaitAsReceiver is a migration-target state, not
// a restore-relevant one.
func autoStarts(strategy kvcore.VirtualMachineRunStrategy) bool {
	switch strategy {
	case kvcore.RunStrategyAlways, kvcore.RunStrategyRerunOnFailure, kvcore.RunStrategyOnce:
		return true
	default:
		return false
	}
}

// originalRunState reports which field the VM used to control its run state
// pre-backup (RunStrategy takes precedence -- the two are mutually
// exclusive per the KubeVirt API), that field's original value, and whether
// the value means a VirtualMachineInstance would be auto-created.
//
// The deprecated bool spec.Running is normalized to the RunStrategy-shaped
// strings "Always"/"Halted" rather than "true"/"false" -- this is the
// agreed contract with the kdm-controller flip-back side (source picks
// which field to write, value is always one of the RunStrategy enum
// strings), so the controller never needs a second parsing branch for the
// bool case.
func originalRunState(vm *kvcore.VirtualMachine) (source, value string, running bool) {
	if vm.Spec.RunStrategy != nil {
		strategy := *vm.Spec.RunStrategy
		return runStrategySourceRunStrategy, string(strategy), autoStarts(strategy)
	}
	if vm.Spec.Running != nil {
		if *vm.Spec.Running {
			return runStrategySourceRunning, string(kvcore.RunStrategyAlways), true
		}
		return runStrategySourceRunning, string(kvcore.RunStrategyHalted), false
	}
	// Neither field set: KubeVirt defaults an unset VM to not running.
	return "", "", false
}

// haltVM returns a copy of item with the VirtualMachine forced to
// RunStrategyHalted and its pre-restore run state stashed in annotations,
// so the kubevirt-datamover-controller can restore the exact original state
// once this VM's DataDownload(s) complete.
//
// This operates on the raw unstructured content (mirrors pvc/restore.go's
// clearPVCBinding) rather than a typed round-trip through kvcore.
// VirtualMachine: a typed round-trip would silently drop any field the
// vendored kubevirt type doesn't know about, whereas setting/removing just
// the fields that need to change preserves everything else exactly as
// Velero handed it to us.
func haltVM(item runtime.Unstructured, source, originalValue string) (runtime.Unstructured, error) {
	copied := item.DeepCopyObject()
	cleaned, ok := copied.(runtime.Unstructured)
	if !ok {
		return nil, fmt.Errorf("failed to copy restore item: unexpected type %T", copied)
	}

	content := cleaned.UnstructuredContent()

	if err := unstructured.SetNestedField(content, string(kvcore.RunStrategyHalted), "spec", "runStrategy"); err != nil {
		return nil, fmt.Errorf("failed to set spec.runStrategy: %w", err)
	}
	// Running and RunStrategy are mutually exclusive in the KubeVirt API;
	// clear the deprecated bool unconditionally so the two fields can never
	// disagree on the restored object.
	unstructured.RemoveNestedField(content, "spec", "running")

	if err := unstructured.SetNestedField(content, source, "metadata", "annotations", AnnotationOriginalRunStrategySource); err != nil {
		return nil, fmt.Errorf("failed to set %s annotation: %w", AnnotationOriginalRunStrategySource, err)
	}
	if err := unstructured.SetNestedField(content, originalValue, "metadata", "annotations", AnnotationOriginalRunStrategy); err != nil {
		return nil, fmt.Errorf("failed to set %s annotation: %w", AnnotationOriginalRunStrategy, err)
	}

	cleaned.SetUnstructuredContent(content)
	return cleaned, nil
}

// Progress returns the progress of an async restore operation. Unlike the
// sibling PVC plugin, this plugin never flips the VM's run state back
// itself -- that is the kubevirt-datamover-controller's responsibility,
// done independently of whether Velero keeps polling Progress (so it
// survives a Velero server restart mid-restore). This method only reports
// honest status to Velero by aggregating the VM's sibling DataDownloads.
func (p *RestorePlugin) Progress(operationID string, restore *velerov1.Restore) (velero.OperationProgress, error) {
	p.Log.Infof("[vm-restore] Checking progress for operation %s", operationID)

	if restore == nil {
		return velero.OperationProgress{}, fmt.Errorf("restore object is nil")
	}

	progress := velero.OperationProgress{
		Completed:      false,
		OperationUnits: "bytes",
		Description:    "Kubevirt datamover VM restore in progress",
		Started:        time.Now(),
		Updated:        time.Now(),
	}
	if start := restore.Status.StartTimestamp; start != nil {
		// Anchor Started to the restore's own start time by default, so it
		// doesn't drift forward on every poll before any DataDownload has
		// reported its own (generally earlier) StartTimestamp below.
		progress.Started = start.Time
	}

	namespace, vmName, err := parseVMRestoreOperationID(operationID, restore.Name)
	if err != nil {
		progress.Completed = true
		progress.Err = err.Error()
		return progress, nil
	}

	dynamicClient, err := newDataDownloadClient()
	if err != nil {
		p.Log.Warnf("[vm-restore] Failed to get dynamic client for VM %s/%s: %v", namespace, vmName, err)
		return progress, err
	}

	dataDownloads, err := p.getDataDownloadsForVM(dynamicClient, restore, namespace, vmName)
	if err != nil {
		p.Log.Warnf("[vm-restore] Failed to list DataDownloads for VM %s/%s: %v", namespace, vmName, err)
		return progress, err
	}

	if len(dataDownloads) == 0 {
		// The sibling PVC plugin creates this VM's DataDownload(s)
		// asynchronously, and Execute() only registers this operation for
		// VMs that carried a datamover DataUpload annotation at backup
		// time -- so an empty list here usually means "not created yet".
		// It can also mean this VM's PVCs never made it into the restore
		// at all, in which case no DataDownload will ever appear -- so
		// bound the wait by the restore's own start time instead of
		// relying solely on Velero's much longer per-operation timeout.
		if start := restore.Status.StartTimestamp; start != nil &&
			time.Since(start.Time) > firstDataDownloadGracePeriod {
			progress.Completed = true
			progress.Err = fmt.Sprintf(
				"no kubevirt datamover DataDownload appeared for VM %s/%s within %s of restore start; "+
					"this plugin halted the VM and it will not start automatically -- if the "+
					"kubevirt-datamover-controller does not reconcile it, restore its run state "+
					"manually from the %s/%s annotations on the VM",
				namespace, vmName, firstDataDownloadGracePeriod, AnnotationOriginalRunStrategySource, AnnotationOriginalRunStrategy)
			p.Log.Errorf("[vm-restore] %s", progress.Err)
			return progress, nil
		}
		progress.Description = "Waiting for kubevirt datamover DataDownload(s) to appear for this VM"
		return progress, nil
	}

	allCompleted := true
	var totalBytes, doneBytes int64
	var earliestStart time.Time
	var failMsgs []string

	for _, dd := range dataDownloads {
		switch dd.Status.Phase {
		case velerov2alpha1.DataDownloadPhaseCompleted:
			// Counted toward allCompleted below by not clearing the flag.
		case velerov2alpha1.DataDownloadPhaseFailed:
			msg := dd.Status.Message
			if msg == "" {
				msg = fmt.Sprintf("DataDownload %s/%s failed", dd.Namespace, dd.Name)
			}
			failMsgs = append(failMsgs, msg)
		case velerov2alpha1.DataDownloadPhaseCanceled:
			msg := dd.Status.Message
			if msg == "" {
				msg = fmt.Sprintf("DataDownload %s/%s was canceled", dd.Namespace, dd.Name)
			}
			failMsgs = append(failMsgs, msg)
		default:
			allCompleted = false
		}
		totalBytes += dd.Status.Progress.TotalBytes
		doneBytes += dd.Status.Progress.BytesDone
		if dd.Status.StartTimestamp != nil {
			if earliestStart.IsZero() || dd.Status.StartTimestamp.Time.Before(earliestStart) {
				earliestStart = dd.Status.StartTimestamp.Time
			}
		}
	}

	progress.NTotal = totalBytes
	progress.NCompleted = doneBytes
	if !earliestStart.IsZero() {
		progress.Started = earliestStart
	}
	progress.Updated = time.Now()

	if len(failMsgs) > 0 {
		progress.Completed = true
		progress.Err = strings.Join(failMsgs, "; ")
		progress.Description = "One or more DataDownloads failed"
		return progress, nil
	}

	if allCompleted {
		progress.Completed = true
		progress.Description = "All DataDownloads for this VM completed"
		return progress, nil
	}

	progress.Description = fmt.Sprintf("Waiting for %d DataDownload(s) to complete", len(dataDownloads))
	return progress, nil
}

// Cancel requests cancellation of an async restore operation by propagating
// Spec.Cancel to every sibling DataDownload for this VM.
func (p *RestorePlugin) Cancel(operationID string, restore *velerov1.Restore) error {
	p.Log.Infof("[vm-restore] Canceling operation %s", operationID)

	if restore == nil {
		return fmt.Errorf("restore object is nil")
	}

	namespace, vmName, err := parseVMRestoreOperationID(operationID, restore.Name)
	if err != nil {
		return err
	}

	dynamicClient, err := newDataDownloadClient()
	if err != nil {
		return fmt.Errorf("failed to get dynamic client for cancellation: %w", err)
	}

	dataDownloads, err := p.getDataDownloadsForVM(dynamicClient, restore, namespace, vmName)
	if err != nil {
		return fmt.Errorf("failed to get DataDownloads for cancellation: %w", err)
	}

	var cancelErrs []error
	for _, dd := range dataDownloads {
		if err := p.patchDataDownloadCancel(dynamicClient, dd); err != nil {
			cancelErrs = append(cancelErrs, fmt.Errorf("failed to cancel DataDownload %s/%s: %w", dd.Namespace, dd.Name, err))
		}
	}

	return errors.Join(cancelErrs...)
}

// AreAdditionalItemsReady is not used by this plugin: completion is tracked
// via the async OperationID/Progress mechanism instead of WaitForAdditionalItems.
func (p *RestorePlugin) AreAdditionalItemsReady(additionalItems []velero.ResourceIdentifier, restore *velerov1.Restore) (bool, error) {
	return true, nil
}

// generateVMRestoreOperationID creates a deterministic operation ID that
// encodes (restoreName, namespace, vmName) directly, so Progress/Cancel can
// recover them without any in-memory state -- state that would not survive
// a Velero server restart mid-restore, which the async-operation mechanism
// exists to tolerate.
//
// "/" is used as the delimiter deliberately (unlike the sibling PVC
// plugin's "-"-joined opaque IDs, which are never parsed back): Kubernetes
// namespace/name/restore-name are all DNS-1123 values that can never
// contain "/", so splitting on it is unambiguous, whereas "-" is a legal,
// common character in all three and would make splitting ambiguous.
func generateVMRestoreOperationID(restoreName, namespace, vmName string) string {
	return fmt.Sprintf("%s/%s/%s/%s", restoreName, namespace, vmName, deterministicSuffix(restoreName, namespace, vmName))
}

// parseVMRestoreOperationID recovers the (namespace, vmName) pair encoded in
// an operationID minted by generateVMRestoreOperationID.
func parseVMRestoreOperationID(operationID, expectedRestoreName string) (namespace, vmName string, err error) {
	parts := strings.SplitN(operationID, "/", 4)
	if len(parts) != 4 {
		return "", "", fmt.Errorf("malformed VM restore operation ID %q", operationID)
	}
	if parts[0] != expectedRestoreName {
		return "", "", fmt.Errorf("operation ID %q does not belong to restore %q", operationID, expectedRestoreName)
	}
	return parts[1], parts[2], nil
}

// getDataDownloadsForVM lists the DataDownloads belonging to this restore
// (narrowed server-side by the same backup/restore-name labels the PVC
// plugin stamps in pvc/restore.go) and filters client-side to the ones
// correlated to the given VM via the AnnotationVMName/AnnotationVMNamespace
// annotations that same plugin stamps on every DataDownload it creates.
//
// The List call leaves ListOptions.Limit unset, so the API server returns
// the entire matching set in one response rather than paginating via a
// continue token -- deliberately, since this restore-scoped set is one
// DataDownload per PVC across all of a restore's kubevirt VMs, orders of
// magnitude below what would ever force the server to chunk a response.
func (p *RestorePlugin) getDataDownloadsForVM(dynamicClient dynamicClientInterface, restore *velerov1.Restore, namespace, vmName string) ([]velerov2alpha1.DataDownload, error) {
	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()

	list, err := dynamicClient.Resource(dataDownloadGVR).Namespace(restore.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s",
			controllercommon.LabelVeleroBackupName, controllercommon.SafeLabelValue(restore.Spec.BackupName),
			controllercommon.LabelVeleroRestoreName, controllercommon.SafeLabelValue(restore.Name)),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list DataDownloads: %w", err)
	}

	var result []velerov2alpha1.DataDownload
	for _, item := range list.Items {
		annotations := item.GetAnnotations()
		if annotations == nil ||
			annotations[controllercommon.AnnotationVMName] != vmName ||
			annotations[controllercommon.AnnotationVMNamespace] != namespace {
			continue
		}
		dd := &velerov2alpha1.DataDownload{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, dd); err != nil {
			return nil, fmt.Errorf("failed to convert unstructured to DataDownload: %w", err)
		}
		result = append(result, *dd)
	}

	return result, nil
}

// patchDataDownloadCancel patches a single DataDownload's Spec.Cancel field
// to true. A scoped merge patch (rather than a full Update) is used
// deliberately: the kubevirt datamover controller concurrently reconciles
// this same DataDownload and updates its Status via a full object Update,
// so replacing the whole object from a possibly-stale local copy risks
// clobbering the controller's in-flight status changes. Reuses the
// cancelPatchBackoff/cancelPatchTotalTimeout policy defined in backup.go:
// Velero's own operation-timeout enforcement calls Cancel() at most once
// and discards its returned error, so a single transient failure here would
// otherwise leave the DataDownload (and its downloader pod) running with no
// other mechanism to ever retry the cancellation.
func (p *RestorePlugin) patchDataDownloadCancel(dynamicClient dynamicClientInterface, dd velerov2alpha1.DataDownload) error {
	patch, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{
			"cancel": true,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to build DataDownload cancel patch: %w", err)
	}

	parentCtx, cancelParent := context.WithTimeout(context.Background(), cancelPatchTotalTimeout)
	defer cancelParent()

	retryErr := retry.OnError(cancelPatchBackoff, func(err error) bool {
		if parentCtx.Err() != nil {
			// The overall cancelPatchTotalTimeout has elapsed: further
			// attempts would just fail on an already-canceled context, so
			// stop instead of burning the rest of the backoff schedule.
			return false
		}
		return !apierrors.IsNotFound(err) && !apierrors.IsInvalid(err)
	}, func() error {
		ctx, cancel := context.WithTimeout(parentCtx, apiCallTimeout)
		defer cancel()
		_, patchErr := dynamicClient.Resource(dataDownloadGVR).Namespace(dd.Namespace).Patch(
			ctx,
			dd.Name,
			types.MergePatchType,
			patch,
			metav1.PatchOptions{},
		)
		return patchErr
	})
	if retryErr != nil {
		if apierrors.IsNotFound(retryErr) {
			// Already gone (e.g. completed and garbage-collected concurrently):
			// nothing left to cancel, not a failure.
			return nil
		}
		return fmt.Errorf("failed to patch DataDownload spec.cancel: %w", retryErr)
	}

	return nil
}
