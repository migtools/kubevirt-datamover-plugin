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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	velerov2alpha1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"

	kvcore "kubevirt.io/api/core/v1"

	controllercommon "github.com/migtools/kubevirt-datamover-controller/pkg/common"
)

const (
	testRestoreVMName = "test-restore-vm"
	testRestoreName2  = "test-restore"
)

func newRestoreTestVM(namespace, name string, annotations map[string]string) *kvcore.VirtualMachine {
	return &kvcore.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kubevirt.io/v1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
	}
}

func dataDownloadUnstructured(t *testing.T, dd *velerov2alpha1.DataDownload) *unstructured.Unstructured {
	ddMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dd)
	require.NoError(t, err)
	u := &unstructured.Unstructured{Object: ddMap}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "velero.io", Version: "v2alpha1", Kind: "DataDownload"})
	return u
}

func datamoverAnnotations(extra map[string]string) map[string]string {
	annotations := map[string]string{
		velerov1.DataUploadNameAnnotation: "du-some-vm",
	}
	for k, v := range extra {
		annotations[k] = v
	}
	return annotations
}

func TestVMRestorePlugin_AppliesTo(t *testing.T) {
	plugin := NewRestorePlugin(newTestLogger())

	selector, err := plugin.AppliesTo()

	require.NoError(t, err)
	assert.Equal(t, velero.ResourceSelector{
		IncludedResources: []string{"virtualmachines.kubevirt.io"},
	}, selector)
}

func TestVMRestorePlugin_Name(t *testing.T) {
	plugin := NewRestorePlugin(newTestLogger())
	assert.Equal(t, "kubevirt-vm-restore-plugin", plugin.Name())
}

func TestVMRestorePlugin_Execute_NilRestore(t *testing.T) {
	plugin := NewRestorePlugin(newTestLogger())
	vmItem := vmToUnstructured(t, newRestoreTestVM(testNamespace, testRestoreVMName, nil))

	_, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{Item: vmItem, Restore: nil})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "restore object is nil")
}

func TestVMRestorePlugin_Execute_NotDatamoverVM(t *testing.T) {
	plugin := NewRestorePlugin(newTestLogger())

	vmObj := newRestoreTestVM(testNamespace, testRestoreVMName, nil)
	vmObj.Spec.RunStrategy = ptr.To(kvcore.RunStrategyAlways)
	vmItem := vmToUnstructured(t, vmObj)
	expected := vmItem.DeepCopyObject().(runtime.Unstructured)

	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"},
	}

	output, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{Item: vmItem, Restore: restore})

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Equal(t, expected, output.UpdatedItem, "a VM with no DataUpload annotation was never backed up via the kubevirt datamover and must be left untouched")
	assert.Equal(t, expected, vmItem, "Execute must not mutate the passed-in Item")
	assert.Empty(t, output.OperationID)
}

func TestVMRestorePlugin_Execute_NotAutoStarting(t *testing.T) {
	testCases := []struct {
		name string
		mod  func(vm *kvcore.VirtualMachine)
	}{
		{name: "RunStrategyHalted", mod: func(vm *kvcore.VirtualMachine) { vm.Spec.RunStrategy = ptr.To(kvcore.RunStrategyHalted) }},
		{name: "RunStrategyManual", mod: func(vm *kvcore.VirtualMachine) { vm.Spec.RunStrategy = ptr.To(kvcore.RunStrategyManual) }},
		{name: "Running=false", mod: func(vm *kvcore.VirtualMachine) { vm.Spec.Running = ptr.To(false) }},
		{name: "neither field set", mod: func(vm *kvcore.VirtualMachine) {}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			plugin := NewRestorePlugin(newTestLogger())

			vmObj := newRestoreTestVM(testNamespace, testRestoreVMName, datamoverAnnotations(nil))
			tc.mod(vmObj)
			vmItem := vmToUnstructured(t, vmObj)
			expected := vmItem.DeepCopyObject().(runtime.Unstructured)

			restore := &velerov1.Restore{ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"}}

			output, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{Item: vmItem, Restore: restore})

			require.NoError(t, err)
			require.NotNil(t, output)
			assert.Equal(t, expected, output.UpdatedItem, "a VM that would not auto-start its VMI cannot race a launcher pod into existence, so Execute must leave it untouched")
			assert.Empty(t, output.OperationID)
		})
	}
}

func TestVMRestorePlugin_Execute_HaltsRunStrategy(t *testing.T) {
	plugin := NewRestorePlugin(newTestLogger())

	vmObj := newRestoreTestVM(testNamespace, testRestoreVMName, datamoverAnnotations(nil))
	vmObj.Spec.RunStrategy = ptr.To(kvcore.RunStrategyAlways)
	vmItem := vmToUnstructured(t, vmObj)
	originalItem := vmItem.DeepCopyObject().(runtime.Unstructured)

	restore := &velerov1.Restore{ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"}}

	output, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{Item: vmItem, Restore: restore})

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Equal(t, originalItem, vmItem, "Execute must not mutate the passed-in Item")
	assert.NotEmpty(t, output.OperationID)

	content := output.UpdatedItem.UnstructuredContent()
	runStrategy, found, err := unstructured.NestedString(content, "spec", "runStrategy")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, string(kvcore.RunStrategyHalted), runStrategy)

	_, found, err = unstructured.NestedBool(content, "spec", "running")
	require.NoError(t, err)
	assert.False(t, found, "spec.running must not be set alongside spec.runStrategy")

	source, found, err := unstructured.NestedString(content, "metadata", "annotations", AnnotationOriginalRunStrategySource)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, runStrategySourceRunStrategy, source)

	value, found, err := unstructured.NestedString(content, "metadata", "annotations", AnnotationOriginalRunStrategy)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, string(kvcore.RunStrategyAlways), value)

	namespace, vmName, err := parseVMRestoreOperationID(output.OperationID, testRestoreName2)
	require.NoError(t, err)
	assert.Equal(t, testNamespace, namespace)
	assert.Equal(t, testRestoreVMName, vmName)
}

func TestVMRestorePlugin_Execute_HaltsDeprecatedRunningBool(t *testing.T) {
	plugin := NewRestorePlugin(newTestLogger())

	vmObj := newRestoreTestVM(testNamespace, testRestoreVMName, datamoverAnnotations(nil))
	vmObj.Spec.Running = ptr.To(true)
	vmItem := vmToUnstructured(t, vmObj)

	restore := &velerov1.Restore{ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"}}

	output, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{Item: vmItem, Restore: restore})

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.NotEmpty(t, output.OperationID)

	content := output.UpdatedItem.UnstructuredContent()
	runStrategy, found, err := unstructured.NestedString(content, "spec", "runStrategy")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, string(kvcore.RunStrategyHalted), runStrategy)

	_, found, err = unstructured.NestedBool(content, "spec", "running")
	require.NoError(t, err)
	assert.False(t, found, "spec.running must be removed so it cannot conflict with spec.runStrategy")

	source, found, err := unstructured.NestedString(content, "metadata", "annotations", AnnotationOriginalRunStrategySource)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, runStrategySourceRunning, source, "controller must flip back via spec.running, not spec.runStrategy, to avoid silently migrating the user off the deprecated field")

	value, found, err := unstructured.NestedString(content, "metadata", "annotations", AnnotationOriginalRunStrategy)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, string(kvcore.RunStrategyAlways), value, "the bool case must be normalized to the RunStrategy-shaped string, not literal \"true\", per the agreed kdm-controller contract")
}

func TestHaltVM_PreservesUnknownFields(t *testing.T) {
	vmObj := newRestoreTestVM(testNamespace, testRestoreVMName, map[string]string{"some-annotation": "keep-me"})
	item := vmToUnstructured(t, vmObj)
	require.NoError(t, unstructured.SetNestedField(item.UnstructuredContent(), "some-value", "spec", "someFutureField"))

	cleaned, err := haltVM(item, runStrategySourceRunStrategy, string(kvcore.RunStrategyAlways))
	require.NoError(t, err)

	futureField, found, err := unstructured.NestedString(cleaned.UnstructuredContent(), "spec", "someFutureField")
	require.NoError(t, err)
	assert.True(t, found, "unknown fields must be preserved, not silently dropped")
	assert.Equal(t, "some-value", futureField)

	annotations, _, err := unstructured.NestedStringMap(cleaned.UnstructuredContent(), "metadata", "annotations")
	require.NoError(t, err)
	assert.Equal(t, "keep-me", annotations["some-annotation"], "unrelated annotations must survive")

	_, found, err = unstructured.NestedString(item.UnstructuredContent(), "spec", "runStrategy")
	require.NoError(t, err)
	assert.False(t, found, "haltVM must not mutate the passed-in item")
}

type nonUnstructuredCopyVMItem struct {
	*unstructured.Unstructured
}

func (n *nonUnstructuredCopyVMItem) DeepCopyObject() runtime.Object {
	return &kvcore.VirtualMachine{}
}

func TestHaltVM_DeepCopyNotUnstructured(t *testing.T) {
	vmObj := newRestoreTestVM(testNamespace, testRestoreVMName, nil)
	item := vmToUnstructured(t, vmObj).(*unstructured.Unstructured)
	badItem := &nonUnstructuredCopyVMItem{Unstructured: item}

	_, err := haltVM(badItem, runStrategySourceRunStrategy, string(kvcore.RunStrategyAlways))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to copy restore item")
}

func TestOriginalRunState(t *testing.T) {
	testCases := []struct {
		name           string
		mod            func(vm *kvcore.VirtualMachine)
		expectedSource string
		expectedValue  string
		expectedRun    bool
	}{
		{name: "RunStrategyAlways", mod: func(vm *kvcore.VirtualMachine) { vm.Spec.RunStrategy = ptr.To(kvcore.RunStrategyAlways) }, expectedSource: runStrategySourceRunStrategy, expectedValue: "Always", expectedRun: true},
		{name: "RunStrategyRerunOnFailure", mod: func(vm *kvcore.VirtualMachine) { vm.Spec.RunStrategy = ptr.To(kvcore.RunStrategyRerunOnFailure) }, expectedSource: runStrategySourceRunStrategy, expectedValue: "RerunOnFailure", expectedRun: true},
		{name: "RunStrategyOnce", mod: func(vm *kvcore.VirtualMachine) { vm.Spec.RunStrategy = ptr.To(kvcore.RunStrategyOnce) }, expectedSource: runStrategySourceRunStrategy, expectedValue: "Once", expectedRun: true},
		{name: "RunStrategyHalted", mod: func(vm *kvcore.VirtualMachine) { vm.Spec.RunStrategy = ptr.To(kvcore.RunStrategyHalted) }, expectedSource: runStrategySourceRunStrategy, expectedValue: "Halted", expectedRun: false},
		{name: "RunStrategyManual", mod: func(vm *kvcore.VirtualMachine) { vm.Spec.RunStrategy = ptr.To(kvcore.RunStrategyManual) }, expectedSource: runStrategySourceRunStrategy, expectedValue: "Manual", expectedRun: false},
		{name: "RunStrategyWaitAsReceiver", mod: func(vm *kvcore.VirtualMachine) { vm.Spec.RunStrategy = ptr.To(kvcore.RunStrategyWaitAsReceiver) }, expectedSource: runStrategySourceRunStrategy, expectedValue: "WaitAsReceiver", expectedRun: false},
		{name: "Running=true", mod: func(vm *kvcore.VirtualMachine) { vm.Spec.Running = ptr.To(true) }, expectedSource: runStrategySourceRunning, expectedValue: "Always", expectedRun: true},
		{name: "Running=false", mod: func(vm *kvcore.VirtualMachine) { vm.Spec.Running = ptr.To(false) }, expectedSource: runStrategySourceRunning, expectedValue: "Halted", expectedRun: false},
		{name: "neither set", mod: func(vm *kvcore.VirtualMachine) {}, expectedSource: "", expectedValue: "", expectedRun: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			vmObj := newRestoreTestVM(testNamespace, testRestoreVMName, nil)
			tc.mod(vmObj)

			source, value, running := originalRunState(vmObj)

			assert.Equal(t, tc.expectedSource, source)
			assert.Equal(t, tc.expectedValue, value)
			assert.Equal(t, tc.expectedRun, running)
		})
	}
}

func TestGenerateAndParseVMRestoreOperationID(t *testing.T) {
	id := generateVMRestoreOperationID(testRestoreName2, testNamespace, testRestoreVMName)

	assert.Equal(t, id, generateVMRestoreOperationID(testRestoreName2, testNamespace, testRestoreVMName),
		"the operation ID must be deterministic so it survives a Velero server restart mid-restore")
	assert.NotEqual(t, id, generateVMRestoreOperationID(testRestoreName2, testNamespace, "other-vm"),
		"distinct VMs in one restore must get distinct operation IDs")

	namespace, vmName, err := parseVMRestoreOperationID(id, testRestoreName2)
	require.NoError(t, err)
	assert.Equal(t, testNamespace, namespace)
	assert.Equal(t, testRestoreVMName, vmName)

	_, _, err = parseVMRestoreOperationID(id, "some-other-restore")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not belong to restore")

	_, _, err = parseVMRestoreOperationID("malformed-id", testRestoreName2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed")
}

func newTestDataDownload(name, veleroNS, backupName, restoreName, vmNamespace, vmName string, phase velerov2alpha1.DataDownloadPhase) *velerov2alpha1.DataDownload {
	return &velerov2alpha1.DataDownload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: veleroNS,
			Labels: map[string]string{
				controllercommon.LabelVeleroBackupName:  controllercommon.SafeLabelValue(backupName),
				controllercommon.LabelVeleroRestoreName: controllercommon.SafeLabelValue(restoreName),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationVMName:      vmName,
				controllercommon.AnnotationVMNamespace: vmNamespace,
			},
		},
		Status: velerov2alpha1.DataDownloadStatus{Phase: phase},
	}
}

func TestVMRestorePlugin_Progress_NilRestore(t *testing.T) {
	plugin := NewRestorePlugin(newTestLogger())
	_, err := plugin.Progress("some-op", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restore object is nil")
}

func TestVMRestorePlugin_Progress_MalformedOperationID(t *testing.T) {
	plugin := NewRestorePlugin(newTestLogger())
	restore := &velerov1.Restore{ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"}}

	progress, err := plugin.Progress("not-a-valid-id", restore)

	require.NoError(t, err)
	assert.True(t, progress.Completed)
	assert.NotEmpty(t, progress.Err)
}

func TestVMRestorePlugin_Progress_ListError(t *testing.T) {
	fakeDynamic := newDataUploadDynamicClient(t)
	fakeDynamic.PrependReactor("list", "datadownloads", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated list failure")
	})
	withFakeDynamicClient(t, fakeDynamic)

	plugin := NewRestorePlugin(newTestLogger())
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"},
		Spec:       velerov1.RestoreSpec{BackupName: testBackupName},
	}
	operationID := generateVMRestoreOperationID(testRestoreName2, testNamespace, testRestoreVMName)

	progress, err := plugin.Progress(operationID, restore)

	require.Error(t, err)
	assert.False(t, progress.Completed)
}

func TestVMRestorePlugin_Progress_NoDataDownloadsYet(t *testing.T) {
	testCases := []struct {
		name              string
		restoreStarted    *metav1.Time
		expectedCompleted bool
		expectErr         bool
	}{
		{name: "no restore start timestamp recorded", restoreStarted: nil, expectedCompleted: false},
		{name: "within grace period", restoreStarted: ptr.To(metav1.NewTime(time.Now().Add(-time.Minute))), expectedCompleted: false},
		{name: "grace period expired", restoreStarted: ptr.To(metav1.NewTime(time.Now().Add(-(firstDataDownloadGracePeriod + time.Minute)))), expectedCompleted: true, expectErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fakeDynamic := newDataUploadDynamicClient(t)
			withFakeDynamicClient(t, fakeDynamic)

			plugin := NewRestorePlugin(newTestLogger())
			restore := &velerov1.Restore{
				ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"},
				Spec:       velerov1.RestoreSpec{BackupName: testBackupName},
				Status:     velerov1.RestoreStatus{StartTimestamp: tc.restoreStarted},
			}
			operationID := generateVMRestoreOperationID(testRestoreName2, testNamespace, testRestoreVMName)

			progress, err := plugin.Progress(operationID, restore)

			require.NoError(t, err)
			assert.Equal(t, tc.expectedCompleted, progress.Completed, "an empty list means the sibling PVC plugin hasn't created the DataDownload(s) yet -- until the grace period since restore start elapses")
			if tc.expectErr {
				assert.NotEmpty(t, progress.Err)
			} else {
				assert.Equal(t, "Waiting for kubevirt datamover DataDownload(s) to appear for this VM", progress.Description)
				assert.Empty(t, progress.Err)
			}
		})
	}
}

func TestVMRestorePlugin_Progress_Aggregation(t *testing.T) {
	testCases := []struct {
		name              string
		phases            []velerov2alpha1.DataDownloadPhase
		message           string
		expectedCompleted bool
		expectErr         bool
		expectedErrText   string
	}{
		{name: "all completed", phases: []velerov2alpha1.DataDownloadPhase{velerov2alpha1.DataDownloadPhaseCompleted, velerov2alpha1.DataDownloadPhaseCompleted}, expectedCompleted: true},
		{name: "one still in progress", phases: []velerov2alpha1.DataDownloadPhase{velerov2alpha1.DataDownloadPhaseCompleted, velerov2alpha1.DataDownloadPhaseInProgress}, expectedCompleted: false},
		{name: "one failed", phases: []velerov2alpha1.DataDownloadPhase{velerov2alpha1.DataDownloadPhaseCompleted, velerov2alpha1.DataDownloadPhaseFailed}, expectedCompleted: true, expectErr: true},
		{name: "one canceled", phases: []velerov2alpha1.DataDownloadPhase{velerov2alpha1.DataDownloadPhaseCompleted, velerov2alpha1.DataDownloadPhaseCanceled}, expectedCompleted: true, expectErr: true},
		// A failure must short-circuit the aggregation even while a sibling is
		// still running, so Velero stops polling instead of waiting out the
		// per-operation timeout.
		{name: "failed alongside in progress", phases: []velerov2alpha1.DataDownloadPhase{velerov2alpha1.DataDownloadPhaseInProgress, velerov2alpha1.DataDownloadPhaseFailed}, expectedCompleted: true, expectErr: true},
		{name: "failed with controller message", phases: []velerov2alpha1.DataDownloadPhase{velerov2alpha1.DataDownloadPhaseFailed}, message: "repository unreachable", expectedCompleted: true, expectErr: true, expectedErrText: "repository unreachable"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			var wantTotal, wantDone int64
			base := metav1.NewTime(time.Now().Truncate(time.Second))
			var wantEarliest time.Time
			for i, phase := range tc.phases {
				dd := newTestDataDownload(
					"dd-vm-progress", "velero", testBackupName, testRestoreName2, testNamespace, testRestoreVMName, phase)
				dd.Name = dd.Name + string(rune('a'+i))
				dd.Status.Progress.TotalBytes = int64(100 * (i + 1))
				dd.Status.Progress.BytesDone = int64(50 * (i + 1))
				dd.Status.Message = tc.message
				// Later entries start later, so the first entry is always the
				// earliest -- tests that progress.Started picks the minimum
				// across all matching DataDownloads, not just the last one seen.
				started := metav1.NewTime(base.Add(time.Duration(i) * time.Minute))
				dd.Status.StartTimestamp = &started
				if wantEarliest.IsZero() || started.Time.Before(wantEarliest) {
					wantEarliest = started.Time
				}
				wantTotal += dd.Status.Progress.TotalBytes
				wantDone += dd.Status.Progress.BytesDone
				objs = append(objs, dataDownloadUnstructured(t, dd))
			}
			// Decoy DataDownload for a different VM in the same restore: must not
			// affect this VM's aggregation. Its byte counts and start time are
			// deliberately far outside the matching entries' range, so
			// accidentally including it would be obvious in the assertions below.
			decoy := newTestDataDownload("dd-decoy", "velero", testBackupName, testRestoreName2, testNamespace, "some-other-vm", velerov2alpha1.DataDownloadPhaseInProgress)
			decoy.Status.Progress.TotalBytes = 987654
			decoy.Status.Progress.BytesDone = 123456
			decoyStarted := metav1.NewTime(base.Add(-time.Hour))
			decoy.Status.StartTimestamp = &decoyStarted
			objs = append(objs, dataDownloadUnstructured(t, decoy))

			fakeDynamic := newDataUploadDynamicClient(t, objs...)
			withFakeDynamicClient(t, fakeDynamic)

			plugin := NewRestorePlugin(newTestLogger())
			restore := &velerov1.Restore{
				ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"},
				Spec:       velerov1.RestoreSpec{BackupName: testBackupName},
			}
			operationID := generateVMRestoreOperationID(testRestoreName2, testNamespace, testRestoreVMName)

			progress, err := plugin.Progress(operationID, restore)

			require.NoError(t, err)
			assert.Equal(t, tc.expectedCompleted, progress.Completed)
			assert.Equal(t, wantTotal, progress.NTotal, "NTotal must sum only this VM's matching DataDownloads, excluding the decoy")
			assert.Equal(t, wantDone, progress.NCompleted, "NCompleted must sum only this VM's matching DataDownloads, excluding the decoy")
			assert.True(t, progress.Started.Equal(wantEarliest), "Started must be the earliest StartTimestamp among this VM's matching DataDownloads, excluding the decoy")
			if tc.expectErr {
				assert.NotEmpty(t, progress.Err)
				if tc.expectedErrText != "" {
					assert.Contains(t, progress.Err, tc.expectedErrText,
						"the controller's own failure message must reach the operator, not the synthesized fallback")
				}
			} else {
				assert.Empty(t, progress.Err)
			}
		})
	}
}

func TestVMRestorePlugin_Cancel_NilRestore(t *testing.T) {
	plugin := NewRestorePlugin(newTestLogger())
	err := plugin.Cancel("some-op", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restore object is nil")
}

func TestVMRestorePlugin_Cancel_NoDataDownloads(t *testing.T) {
	fakeDynamic := newDataUploadDynamicClient(t)
	withFakeDynamicClient(t, fakeDynamic)

	plugin := NewRestorePlugin(newTestLogger())
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"},
		Spec:       velerov1.RestoreSpec{BackupName: testBackupName},
	}
	operationID := generateVMRestoreOperationID(testRestoreName2, testNamespace, testRestoreVMName)

	err := plugin.Cancel(operationID, restore)
	assert.NoError(t, err)
}

func TestVMRestorePlugin_Cancel_PatchesOnlyMatchingVM(t *testing.T) {
	dd := newTestDataDownload("dd-vm-cancel", "velero", testBackupName, testRestoreName2, testNamespace, testRestoreVMName, velerov2alpha1.DataDownloadPhaseInProgress)
	decoy := newTestDataDownload("dd-vm-cancel-decoy", "velero", testBackupName, testRestoreName2, testNamespace, "some-other-vm", velerov2alpha1.DataDownloadPhaseInProgress)

	fakeDynamic := newDataUploadDynamicClient(t, dataDownloadUnstructured(t, dd), dataDownloadUnstructured(t, decoy))
	withFakeDynamicClient(t, fakeDynamic)

	plugin := NewRestorePlugin(newTestLogger())
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"},
		Spec:       velerov1.RestoreSpec{BackupName: testBackupName},
	}
	operationID := generateVMRestoreOperationID(testRestoreName2, testNamespace, testRestoreVMName)

	err := plugin.Cancel(operationID, restore)
	require.NoError(t, err)

	gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datadownloads"}

	updated, err := fakeDynamic.Resource(gvr).Namespace("velero").Get(context.Background(), "dd-vm-cancel", metav1.GetOptions{})
	require.NoError(t, err)
	updatedDD := &velerov2alpha1.DataDownload{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(updated.Object, updatedDD))
	assert.True(t, updatedDD.Spec.Cancel)

	decoyAfter, err := fakeDynamic.Resource(gvr).Namespace("velero").Get(context.Background(), "dd-vm-cancel-decoy", metav1.GetOptions{})
	require.NoError(t, err)
	decoyDD := &velerov2alpha1.DataDownload{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(decoyAfter.Object, decoyDD))
	assert.False(t, decoyDD.Spec.Cancel, "Cancel must not touch DataDownloads for other VMs")
}

func TestVMRestorePlugin_Cancel_AttemptsAllAndAggregatesErrors(t *testing.T) {
	withFastCancelBackoff(t)

	ok := newTestDataDownload("dd-vm-cancel-ok", "velero", testBackupName, testRestoreName2, testNamespace, testRestoreVMName, velerov2alpha1.DataDownloadPhaseInProgress)
	failing := newTestDataDownload("dd-vm-cancel-fail", "velero", testBackupName, testRestoreName2, testNamespace, testRestoreVMName, velerov2alpha1.DataDownloadPhaseInProgress)

	fakeDynamic := newDataUploadDynamicClient(t, dataDownloadUnstructured(t, ok), dataDownloadUnstructured(t, failing))
	fakeDynamic.PrependReactor("patch", "datadownloads", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.PatchAction).GetName() == failing.Name {
			return true, nil, fmt.Errorf("simulated patch failure")
		}
		return false, nil, nil
	})
	withFakeDynamicClient(t, fakeDynamic)

	plugin := NewRestorePlugin(newTestLogger())
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"},
		Spec:       velerov1.RestoreSpec{BackupName: testBackupName},
	}
	operationID := generateVMRestoreOperationID(testRestoreName2, testNamespace, testRestoreVMName)

	err := plugin.Cancel(operationID, restore)
	require.Error(t, err)
	assert.Contains(t, err.Error(), failing.Name, "aggregated error must identify the DataDownload that failed to cancel")

	gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datadownloads"}
	updatedOK, getErr := fakeDynamic.Resource(gvr).Namespace("velero").Get(context.Background(), ok.Name, metav1.GetOptions{})
	require.NoError(t, getErr)
	updatedOKDD := &velerov2alpha1.DataDownload{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(updatedOK.Object, updatedOKDD))
	assert.True(t, updatedOKDD.Spec.Cancel, "Cancel must still patch the other sibling DataDownload even though one patch failed")
}

func TestVMRestorePlugin_Cancel_AlreadyGoneIsNotAnError(t *testing.T) {
	withFastCancelBackoff(t)

	dd := newTestDataDownload("dd-vm-cancel-gone", "velero", testBackupName, testRestoreName2, testNamespace, testRestoreVMName, velerov2alpha1.DataDownloadPhaseInProgress)

	fakeDynamic := newDataUploadDynamicClient(t, dataDownloadUnstructured(t, dd))
	fakeDynamic.PrependReactor("patch", "datadownloads", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "velero.io", Resource: "datadownloads"}, dd.Name)
	})
	withFakeDynamicClient(t, fakeDynamic)

	plugin := NewRestorePlugin(newTestLogger())
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName2, Namespace: "velero"},
		Spec:       velerov1.RestoreSpec{BackupName: testBackupName},
	}
	operationID := generateVMRestoreOperationID(testRestoreName2, testNamespace, testRestoreVMName)

	err := plugin.Cancel(operationID, restore)
	assert.NoError(t, err, "a DataDownload that's already gone (e.g. completed and garbage-collected concurrently) has nothing left to cancel")
}

func TestVMRestorePlugin_AreAdditionalItemsReady(t *testing.T) {
	plugin := NewRestorePlugin(newTestLogger())
	ready, err := plugin.AreAdditionalItemsReady(nil, &velerov1.Restore{})
	require.NoError(t, err)
	assert.True(t, ready)
}
