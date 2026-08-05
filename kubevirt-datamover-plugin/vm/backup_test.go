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

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	velerov2alpha1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"

	kvcore "kubevirt.io/api/core/v1"

	controllercommon "github.com/migtools/kubevirt-datamover-controller/pkg/common"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/clients"
)

const (
	testNamespace  = "test-namespace"
	testVMName     = "test-vm"
	testBackupName = "test-backup"
)

// newTestLogger creates a logger for tests that discards output.
func newTestLogger() logrus.FieldLogger {
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	return logrus.NewEntry(logger)
}

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool {
	return &b
}

func TestBackupPlugin_AppliesTo(t *testing.T) {
	fakeClient := getFakeClient()
	plugin, err := NewBackupPlugin(newTestLogger(), fakeClient)
	require.NoError(t, err)

	selector, err := plugin.AppliesTo()

	require.NoError(t, err)
	assert.Equal(t, velero.ResourceSelector{
		IncludedResources: []string{"virtualmachines.kubevirt.io"},
	}, selector)
}

func TestBackupPlugin_Name(t *testing.T) {
	fakeClient := getFakeClient()
	plugin, err := NewBackupPlugin(newTestLogger(), fakeClient)
	require.NoError(t, err)

	name := plugin.Name()

	assert.Equal(t, "kubevirt-vm-backup-plugin", name)
}

func TestBackupPlugin_Execute_NilBackup(t *testing.T) {
	fakeClient := getFakeClient()
	plugin, err := NewBackupPlugin(newTestLogger(), fakeClient)
	require.NoError(t, err)

	vm := createTestVM(testNamespace, testVMName, kvcore.VirtualMachineStatusRunning)
	item := vmToUnstructured(t, vm)

	_, _, _, _, err = plugin.Execute(item, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup object is nil")
}

func TestBackupPlugin_Execute_SnapshotMoveDataNotEnabled(t *testing.T) {
	fakeClient := getFakeClient()
	plugin, err := NewBackupPlugin(newTestLogger(), fakeClient)
	require.NoError(t, err)

	vm := createTestVM(testNamespace, testVMName, kvcore.VirtualMachineStatusRunning)
	item := vmToUnstructured(t, vm)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testBackupName,
			Namespace: "velero",
		},
		Spec: velerov1.BackupSpec{
			SnapshotMoveData: boolPtr(false),
		},
	}

	result, additionalItems, operationID, _, err := plugin.Execute(item, backup)

	require.NoError(t, err)
	assert.NotNil(t, result, "should return item even if not eligible")
	assert.Empty(t, additionalItems, "should not return additional items")
	assert.Empty(t, operationID, "should not return operation ID")
}

func TestBackupPlugin_Execute_VMNotRunning(t *testing.T) {
	fakeClient := getFakeClient()
	plugin, err := NewBackupPlugin(newTestLogger(), fakeClient)
	require.NoError(t, err)

	vm := createTestVM(testNamespace, testVMName, kvcore.VirtualMachineStatusStopped)
	item := vmToUnstructured(t, vm)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testBackupName,
			Namespace: "velero",
		},
		Spec: velerov1.BackupSpec{
			SnapshotMoveData: boolPtr(true),
		},
	}

	result, additionalItems, operationID, _, err := plugin.Execute(item, backup)

	require.NoError(t, err)
	assert.NotNil(t, result, "should return item even if not eligible")
	assert.Empty(t, additionalItems, "should not return additional items when VM not running")
	assert.Empty(t, operationID, "should not return operation ID when VM not running")
}

// TestControllerValidateVMIsRunning tests the controller's ValidateVMIsRunning function
// to ensure it performs the expected validation.
func TestControllerValidateVMIsRunning(t *testing.T) {
	testCases := []struct {
		name        string
		status      kvcore.VirtualMachinePrintableStatus
		expectError bool
	}{
		{
			name:        "Running VM",
			status:      kvcore.VirtualMachineStatusRunning,
			expectError: false,
		},
		{
			name:        "Starting VM - not allowed by controller validation",
			status:      kvcore.VirtualMachineStatusStarting,
			expectError: true, // Controller only allows Running, not Starting
		},
		{
			name:        "Stopped VM",
			status:      kvcore.VirtualMachineStatusStopped,
			expectError: true,
		},
		{
			name:        "Stopping VM",
			status:      kvcore.VirtualMachineStatusStopping,
			expectError: true,
		},
		{
			name:        "Paused VM",
			status:      kvcore.VirtualMachineStatusPaused,
			expectError: true,
		},
		{
			name:        "Migrating VM",
			status:      kvcore.VirtualMachineStatusMigrating,
			expectError: true,
		},
		{
			name:        "Provisioning VM",
			status:      kvcore.VirtualMachineStatusProvisioning,
			expectError: true,
		},
		{
			name:        "Terminating VM",
			status:      kvcore.VirtualMachineStatusTerminating,
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			vm := createTestVM(testNamespace, testVMName, tc.status)
			err := controllercommon.ValidateVMIsRunning(vm)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestControllerValidateCBTEnabled tests the controller's ValidateCBTEnabled function
// to ensure it performs the expected validation using vm.Status.ChangedBlockTracking.
func TestControllerValidateCBTEnabled(t *testing.T) {
	testCases := []struct {
		name        string
		vm          *kvcore.VirtualMachine
		expectError bool
	}{
		{
			name: "CBT enabled via ChangedBlockTracking status",
			vm: &kvcore.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testVMName,
					Namespace: testNamespace,
				},
				Status: kvcore.VirtualMachineStatus{
					ChangedBlockTracking: &kvcore.ChangedBlockTrackingStatus{
						State: kvcore.ChangedBlockTrackingEnabled,
					},
				},
			},
			expectError: false,
		},
		{
			name: "CBT disabled - no ChangedBlockTracking status",
			vm: &kvcore.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testVMName,
					Namespace: testNamespace,
				},
				Status: kvcore.VirtualMachineStatus{},
			},
			expectError: true,
		},
		{
			name: "CBT disabled - ChangedBlockTracking state is not Enabled",
			vm: &kvcore.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testVMName,
					Namespace: testNamespace,
				},
				Status: kvcore.VirtualMachineStatus{
					ChangedBlockTracking: &kvcore.ChangedBlockTrackingStatus{
						State: kvcore.ChangedBlockTrackingDisabled,
					},
				},
			},
			expectError: true,
		},
		{
			name:        "nil VM",
			vm:          nil,
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := controllercommon.ValidateCBTEnabled(tc.vm)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestControllerGetVolumesForVm tests the controller's GetVolumesForVm function
// to ensure it properly extracts PVC names from VirtualMachine volumes.
// Full test coverage is in the controller package; this verifies integration.
func TestControllerGetVolumesForVm(t *testing.T) {
	testCases := []struct {
		name     string
		vm       *kvcore.VirtualMachine
		expected []string
	}{
		{
			name: "VM with PVC volume",
			vm: &kvcore.VirtualMachine{
				Spec: kvcore.VirtualMachineSpec{
					Template: &kvcore.VirtualMachineInstanceTemplateSpec{
						Spec: kvcore.VirtualMachineInstanceSpec{
							Volumes: []kvcore.Volume{
								{
									Name: "disk0",
									VolumeSource: kvcore.VolumeSource{
										PersistentVolumeClaim: &kvcore.PersistentVolumeClaimVolumeSource{
											PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
												ClaimName: "pvc-1",
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: []string{"pvc-1"},
		},
		{
			name: "VM with DataVolume",
			vm: &kvcore.VirtualMachine{
				Spec: kvcore.VirtualMachineSpec{
					Template: &kvcore.VirtualMachineInstanceTemplateSpec{
						Spec: kvcore.VirtualMachineInstanceSpec{
							Volumes: []kvcore.Volume{
								{
									Name: "disk0",
									VolumeSource: kvcore.VolumeSource{
										DataVolume: &kvcore.DataVolumeSource{
											Name: "dv-1",
										},
									},
								},
							},
						},
					},
				},
			},
			expected: []string{"dv-1"},
		},
		{
			name: "VM with multiple volumes",
			vm: &kvcore.VirtualMachine{
				Spec: kvcore.VirtualMachineSpec{
					Template: &kvcore.VirtualMachineInstanceTemplateSpec{
						Spec: kvcore.VirtualMachineInstanceSpec{
							Volumes: []kvcore.Volume{
								{
									Name: "disk0",
									VolumeSource: kvcore.VolumeSource{
										PersistentVolumeClaim: &kvcore.PersistentVolumeClaimVolumeSource{
											PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
												ClaimName: "pvc-1",
											},
										},
									},
								},
								{
									Name: "disk1",
									VolumeSource: kvcore.VolumeSource{
										DataVolume: &kvcore.DataVolumeSource{
											Name: "dv-1",
										},
									},
								},
							},
						},
					},
				},
			},
			expected: []string{"pvc-1", "dv-1"},
		},
		{
			name: "VM with no Template",
			vm: &kvcore.VirtualMachine{
				Spec: kvcore.VirtualMachineSpec{
					Template: nil,
				},
			},
			expected: []string{},
		},
		{
			name: "VM with empty volumes",
			vm: &kvcore.VirtualMachine{
				Spec: kvcore.VirtualMachineSpec{
					Template: &kvcore.VirtualMachineInstanceTemplateSpec{
						Spec: kvcore.VirtualMachineInstanceSpec{
							Volumes: []kvcore.Volume{},
						},
					},
				},
			},
			expected: []string{},
		},
		{
			name: "VM with non-PVC volumes only",
			vm: &kvcore.VirtualMachine{
				Spec: kvcore.VirtualMachineSpec{
					Template: &kvcore.VirtualMachineInstanceTemplateSpec{
						Spec: kvcore.VirtualMachineInstanceSpec{
							Volumes: []kvcore.Volume{
								{
									Name: "cloudinit",
									VolumeSource: kvcore.VolumeSource{
										CloudInitNoCloud: &kvcore.CloudInitNoCloudSource{
											UserData: "test",
										},
									},
								},
							},
						},
					},
				},
			},
			expected: []string{},
		},
		{
			name:     "nil VM",
			vm:       nil,
			expected: []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := controllercommon.GetVolumesForVm(tc.vm)
			// Use ElementsMatch to handle nil vs empty slice difference
			assert.ElementsMatch(t, tc.expected, result)
		})
	}
}

func TestBackupPlugin_checkPreconditions(t *testing.T) {
	fakeClient := getFakeClient()
	plugin, err := NewBackupPlugin(newTestLogger(), fakeClient)
	require.NoError(t, err)

	testCases := []struct {
		name             string
		vm               *kvcore.VirtualMachine
		backup           *velerov1.Backup
		expectedEligible bool
		expectedReason   string
	}{
		{
			name: "SnapshotMoveData not enabled",
			vm:   createTestVMWithCBT(testNamespace, testVMName, kvcore.VirtualMachineStatusRunning),
			backup: &velerov1.Backup{
				Spec: velerov1.BackupSpec{
					SnapshotMoveData: boolPtr(false),
				},
			},
			expectedEligible: false,
			expectedReason:   "backup.Spec.SnapshotMoveData is not enabled",
		},
		{
			name: "SnapshotMoveData nil",
			vm:   createTestVMWithCBT(testNamespace, testVMName, kvcore.VirtualMachineStatusRunning),
			backup: &velerov1.Backup{
				Spec: velerov1.BackupSpec{
					SnapshotMoveData: nil,
				},
			},
			expectedEligible: false,
			expectedReason:   "backup.Spec.SnapshotMoveData is not enabled",
		},
		{
			name: "VM not running",
			vm:   createTestVMWithCBT(testNamespace, testVMName, kvcore.VirtualMachineStatusStopped),
			backup: &velerov1.Backup{
				Spec: velerov1.BackupSpec{
					SnapshotMoveData: boolPtr(true),
				},
			},
			expectedEligible: false,
			expectedReason:   "not running",
		},
		{
			name: "CBT not enabled",
			vm: &kvcore.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testVMName,
					Namespace: testNamespace,
				},
				Status: kvcore.VirtualMachineStatus{
					PrintableStatus: kvcore.VirtualMachineStatusRunning,
					// No ChangedBlockTracking status
				},
			},
			backup: &velerov1.Backup{
				Spec: velerov1.BackupSpec{
					SnapshotMoveData: boolPtr(true),
				},
			},
			expectedEligible: false,
			expectedReason:   "ChangedBlockTracking",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			vh, err := plugin.pluginPVCPodCache.GetOrCreateVolumeHelper(tc.backup, plugin.crClient, plugin.Log)
			require.NoError(t, err)
			eligible, reason, err := CheckPreconditions(tc.vm, tc.backup, plugin.Log, vh)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedEligible, eligible)
			if !tc.expectedEligible {
				assert.Contains(t, reason, tc.expectedReason)
			}
		})
	}
}

func getFakeClient() crclient.Client {
	scheme := runtime.NewScheme()
	//_ = velerov2alpha1.AddToScheme(scheme)
	_ = kvcore.AddToScheme(scheme)
	builder := fake.NewClientBuilder().
		WithScheme(scheme)
	fakeClient := builder.Build()
	return fakeClient
}

// newDataUploadDynamicClient returns a fake dynamic client seeded with objects,
// scoped to the velerov2alpha1 scheme so DataUpload GVK operations resolve.
func newDataUploadDynamicClient(t *testing.T, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	require.NoError(t, velerov2alpha1.AddToScheme(scheme))
	return dynamicfake.NewSimpleDynamicClient(scheme, objects...)
}

// withFakeDynamicClient overrides the package-level getDynamicClient and the
// clients package's in-cluster config for the duration of t, restoring both
// via t.Cleanup. Because it mutates package-level state, tests using it must
// not run with t.Parallel().
func withFakeDynamicClient(t *testing.T, fakeDynamic *dynamicfake.FakeDynamicClient) {
	t.Helper()
	original := getDynamicClient
	SetDynamicClientFunc(func(config interface{}) (dynamicClientInterface, error) {
		return fakeDynamic, nil
	})
	t.Cleanup(func() { getDynamicClient = original })

	clients.SetInClusterConfig(&rest.Config{Host: "https://fake"})
	t.Cleanup(func() { clients.SetInClusterConfig(nil) })
}

// withFastCancelBackoff overrides the package-level cancelPatchBackoff with a
// minimal backoff for the duration of t, restoring the original via
// t.Cleanup. Used by Cancel retry tests so they don't pay the real ~600ms of
// sleep the production backoff (200ms, factor 2, 3 steps) would incur.
func withFastCancelBackoff(t *testing.T) {
	t.Helper()
	original := cancelPatchBackoff
	cancelPatchBackoff = wait.Backoff{Steps: 3, Duration: time.Millisecond, Factor: 1.0}
	t.Cleanup(func() { cancelPatchBackoff = original })
}

func dataUploadUnstructured(t *testing.T, du *velerov2alpha1.DataUpload) *unstructured.Unstructured {
	duMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(du)
	require.NoError(t, err)
	u := &unstructured.Unstructured{Object: duMap}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "velero.io", Version: "v2alpha1", Kind: "DataUpload"})
	return u
}

func TestBackupPlugin_CreateDataUpload_Create(t *testing.T) {
	vm := &kvcore.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: testVMName, Namespace: testNamespace}}
	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero", UID: "backup-uid"},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	sourcePVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: testNamespace}}
	const operationID = "op-create-1"

	fakeDynamic := newDataUploadDynamicClient(t)
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	result, err := plugin.createDataUpload(vm, backup, operationID, sourcePVC)

	require.NoError(t, err)
	require.NotNil(t, result)
	expectedName := generateDataUploadName(backup.Name, vm.Namespace, vm.Name)
	assert.Equal(t, expectedName, result.Name)

	gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datauploads"}
	created, err := fakeDynamic.Resource(gvr).Namespace(backup.Namespace).Get(context.Background(), expectedName, metav1.GetOptions{})
	require.NoError(t, err)
	createdDU := &velerov2alpha1.DataUpload{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(created.Object, createdDU))

	assert.Equal(t, controllercommon.SafeLabelValue(backup.Name), createdDU.Labels[velerov1.BackupNameLabel])
	assert.Equal(t, operationID, createdDU.Annotations[controllercommon.AnnotationOperationID])
	require.Len(t, createdDU.OwnerReferences, 1)
	assert.Equal(t, backup.Name, createdDU.OwnerReferences[0].Name)
	assert.Equal(t, backup.UID, createdDU.OwnerReferences[0].UID)
	require.NotNil(t, createdDU.OwnerReferences[0].Controller)
	assert.True(t, *createdDU.OwnerReferences[0].Controller)
	assert.Equal(t, result, createdDU, "createDataUpload must return the object it actually created")
}

func TestBackupPlugin_CreateDataUpload_AlreadyExists_Reuse(t *testing.T) {
	vm := &kvcore.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: testVMName, Namespace: testNamespace}}
	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero", UID: "backup-uid"},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	sourcePVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: testNamespace}}

	// Precompute the exact name/operationID createDataUpload will derive, and
	// seed the fake dynamic client with a matching DataUpload already present --
	// this simulates a prior Execute() call for this same (backup, VM) having
	// already created it (e.g. Velero retried after a transient RPC error).
	operationID := generateOperationID(backup.Name, vm.Namespace, vm.Name)
	existingName := generateDataUploadName(backup.Name, vm.Namespace, vm.Name)

	existing := &velerov2alpha1.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      existingName,
			Namespace: backup.Namespace,
			Labels: map[string]string{
				velerov1.BackupNameLabel: controllercommon.SafeLabelValue(backup.Name),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationVMName:      vm.Name,
				controllercommon.AnnotationVMNamespace: vm.Namespace,
				controllercommon.AnnotationOperationID: operationID,
			},
			OwnerReferences: []metav1.OwnerReference{{UID: backup.UID}},
		},
		Spec: velerov2alpha1.DataUploadSpec{
			SourcePVC:       sourcePVC.Name,
			SourceNamespace: vm.Namespace,
			// Distinguishes the fetched-from-cluster object from the locally
			// built one, so this test actually proves the adoption path
			// returns what's in the cluster rather than trivially matching
			// locally-derived name/operationID values that would match either way.
			BackupStorageLocation: "seeded-bsl",
		},
	}

	fakeDynamic := newDataUploadDynamicClient(t, dataUploadUnstructured(t, existing))
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	result, err := plugin.createDataUpload(vm, backup, operationID, sourcePVC)

	require.NoError(t, err, "createDataUpload must reuse the existing DataUpload instead of erroring on AlreadyExists")
	assert.Equal(t, existingName, result.Name)
	assert.Equal(t, operationID, result.Annotations[controllercommon.AnnotationOperationID])
	assert.Equal(t, "seeded-bsl", result.Spec.BackupStorageLocation,
		"createDataUpload must return the object fetched from the cluster, not the locally built one")

	gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datauploads"}
	list, err := fakeDynamic.Resource(gvr).Namespace(backup.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, list.Items, 1, "createDataUpload must not create a duplicate DataUpload for a retried call")
}

func TestBackupPlugin_CreateDataUpload_AlreadyExists_Mismatch(t *testing.T) {
	vm := &kvcore.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: testVMName, Namespace: testNamespace}}
	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero"},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	sourcePVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: testNamespace}}

	operationID := generateOperationID(backup.Name, vm.Namespace, vm.Name)
	existingName := generateDataUploadName(backup.Name, vm.Namespace, vm.Name)

	// Same deterministic name, but a different SourcePVC -- must not be mistaken
	// for "our" operation and silently reused.
	existing := &velerov2alpha1.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      existingName,
			Namespace: backup.Namespace,
		},
		Spec: velerov2alpha1.DataUploadSpec{
			SourcePVC:       "some-other-pvc",
			SourceNamespace: vm.Namespace,
		},
	}

	fakeDynamic := newDataUploadDynamicClient(t, dataUploadUnstructured(t, existing))
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	_, err := plugin.createDataUpload(vm, backup, operationID, sourcePVC)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "some-other-pvc", "must reject on the source PVC mismatch branch")
	assert.Contains(t, err.Error(), "refusing to reuse")
}

func TestBackupPlugin_CreateDataUpload_AlreadyExists_NamespaceMismatch(t *testing.T) {
	vm := &kvcore.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: testVMName, Namespace: testNamespace}}
	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero"},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	sourcePVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: testNamespace}}

	operationID := generateOperationID(backup.Name, vm.Namespace, vm.Name)
	existingName := generateDataUploadName(backup.Name, vm.Namespace, vm.Name)

	// Same deterministic name and the same SourcePVC name, but a different
	// SourceNamespace -- e.g. a same-named VM backed up from a different
	// namespace. Must not be mistaken for "our" operation and silently reused.
	existing := &velerov2alpha1.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      existingName,
			Namespace: backup.Namespace,
		},
		Spec: velerov2alpha1.DataUploadSpec{
			SourcePVC:       sourcePVC.Name,
			SourceNamespace: "some-other-namespace",
		},
	}

	fakeDynamic := newDataUploadDynamicClient(t, dataUploadUnstructured(t, existing))
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	_, err := plugin.createDataUpload(vm, backup, operationID, sourcePVC)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "some-other-namespace", "must reject on the source namespace mismatch branch")
	assert.Contains(t, err.Error(), "refusing to reuse")
}

func TestBackupPlugin_CreateDataUpload_AlreadyExists_OwnerUIDMismatch(t *testing.T) {
	vm := &kvcore.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: testVMName, Namespace: testNamespace}}
	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero", UID: "current-backup-uid"},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	sourcePVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: testNamespace}}

	operationID := generateOperationID(backup.Name, vm.Namespace, vm.Name)
	existingName := generateDataUploadName(backup.Name, vm.Namespace, vm.Name)

	// Source matches, but the object's owner reference belongs to a prior
	// Backup of the same name that was deleted and recreated.
	existing := &velerov2alpha1.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Name:            existingName,
			Namespace:       backup.Namespace,
			OwnerReferences: []metav1.OwnerReference{{UID: "stale-backup-uid"}},
		},
		Spec: velerov2alpha1.DataUploadSpec{
			SourcePVC:       sourcePVC.Name,
			SourceNamespace: vm.Namespace,
		},
	}

	fakeDynamic := newDataUploadDynamicClient(t, dataUploadUnstructured(t, existing))
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	_, err := plugin.createDataUpload(vm, backup, operationID, sourcePVC)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not owned by Backup")
}

func TestBackupPlugin_CreateDataUpload_AlreadyExists_NoOperationIDAnnotation(t *testing.T) {
	vm := &kvcore.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: testVMName, Namespace: testNamespace}}
	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero", UID: "backup-uid"},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	sourcePVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: testNamespace}}

	operationID := generateOperationID(backup.Name, vm.Namespace, vm.Name)
	existingName := generateDataUploadName(backup.Name, vm.Namespace, vm.Name)

	// Source and owner match, but Progress and Cancel could never find this
	// object without the operation-ID annotation.
	existing := &velerov2alpha1.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Name:            existingName,
			Namespace:       backup.Namespace,
			OwnerReferences: []metav1.OwnerReference{{UID: backup.UID}},
		},
		Spec: velerov2alpha1.DataUploadSpec{
			SourcePVC:       sourcePVC.Name,
			SourceNamespace: vm.Namespace,
		},
	}

	fakeDynamic := newDataUploadDynamicClient(t, dataUploadUnstructured(t, existing))
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	_, err := plugin.createDataUpload(vm, backup, operationID, sourcePVC)

	require.Error(t, err)
	assert.Contains(t, err.Error(), controllercommon.AnnotationOperationID)
}

func TestBackupPlugin_CreateDataUpload_AlreadyExists_BackupNameLabelMismatch(t *testing.T) {
	vm := &kvcore.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: testVMName, Namespace: testNamespace}}
	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero", UID: "backup-uid"},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	sourcePVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: testNamespace}}

	operationID := generateOperationID(backup.Name, vm.Namespace, vm.Name)
	existingName := generateDataUploadName(backup.Name, vm.Namespace, vm.Name)

	// Source, owner, and operationID annotation all match, but the
	// BackupNameLabel is missing -- getDataUploadByOperationID selects on it
	// server-side, so this object would be unreachable for Progress/Cancel.
	existing := &velerov2alpha1.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Name:            existingName,
			Namespace:       backup.Namespace,
			OwnerReferences: []metav1.OwnerReference{{UID: backup.UID}},
			Annotations: map[string]string{
				controllercommon.AnnotationOperationID: operationID,
			},
		},
		Spec: velerov2alpha1.DataUploadSpec{
			SourcePVC:       sourcePVC.Name,
			SourceNamespace: vm.Namespace,
		},
	}

	fakeDynamic := newDataUploadDynamicClient(t, dataUploadUnstructured(t, existing))
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	_, err := plugin.createDataUpload(vm, backup, operationID, sourcePVC)

	require.Error(t, err)
	assert.Contains(t, err.Error(), velerov1.BackupNameLabel)
}

func TestBackupPlugin_Cancel(t *testing.T) {
	const operationID = "op-cancel-1"
	backup := &velerov1.Backup{ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero"}}

	du := &velerov2alpha1.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "du-cancel",
			Namespace: backup.Namespace,
			Labels: map[string]string{
				velerov1.BackupNameLabel: controllercommon.SafeLabelValue(backup.Name),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationOperationID: operationID,
			},
		},
		Spec: velerov2alpha1.DataUploadSpec{Cancel: false},
	}

	// A decoy DataUpload sharing the same backup label (as a second VM backed up
	// in the same backup would) but a different operationID: proves Cancel only
	// patches the DataUpload matching the supplied operationID and leaves the
	// decoy's Spec.Cancel untouched.
	decoy := &velerov2alpha1.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "du-cancel-decoy",
			Namespace: backup.Namespace,
			Labels: map[string]string{
				velerov1.BackupNameLabel: controllercommon.SafeLabelValue(backup.Name),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationOperationID: "op-cancel-decoy",
			},
		},
		Spec: velerov2alpha1.DataUploadSpec{Cancel: false},
	}

	fakeDynamic := newDataUploadDynamicClient(t, dataUploadUnstructured(t, du), dataUploadUnstructured(t, decoy))
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	err := plugin.Cancel(operationID, backup)
	require.NoError(t, err)

	gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datauploads"}
	updated, err := fakeDynamic.Resource(gvr).Namespace(backup.Namespace).Get(context.Background(), "du-cancel", metav1.GetOptions{})
	require.NoError(t, err)
	updatedDU := &velerov2alpha1.DataUpload{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(updated.Object, updatedDU))
	assert.True(t, updatedDU.Spec.Cancel)

	decoyAfter, err := fakeDynamic.Resource(gvr).Namespace(backup.Namespace).Get(context.Background(), "du-cancel-decoy", metav1.GetOptions{})
	require.NoError(t, err)
	decoyDU := &velerov2alpha1.DataUpload{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(decoyAfter.Object, decoyDU))
	assert.False(t, decoyDU.Spec.Cancel, "Cancel must not touch DataUploads for other operations")
}

func TestBackupPlugin_Cancel_RetriesTransientPatchFailure(t *testing.T) {
	withFastCancelBackoff(t)

	const operationID = "op-cancel-retry-1"
	backup := &velerov1.Backup{ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero"}}

	du := &velerov2alpha1.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "du-cancel-retry",
			Namespace: backup.Namespace,
			Labels: map[string]string{
				velerov1.BackupNameLabel: controllercommon.SafeLabelValue(backup.Name),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationOperationID: operationID,
			},
		},
		Spec: velerov2alpha1.DataUploadSpec{Cancel: false},
	}

	fakeDynamic := newDataUploadDynamicClient(t, dataUploadUnstructured(t, du))
	attempts := 0
	fakeDynamic.PrependReactor("patch", "datauploads", func(action k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts < 3 {
			return true, nil, fmt.Errorf("simulated transient patch failure")
		}
		// Unhandled: let the request fall through to the tracker's default
		// reactor, which actually applies the patch.
		return false, nil, nil
	})
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	err := plugin.Cancel(operationID, backup)

	require.NoError(t, err, "Cancel must retry past transient patch failures rather than giving up after one attempt -- Velero's own timeout enforcement calls Cancel exactly once and discards the error, so this is the only retry path that exists")
	assert.Equal(t, 3, attempts)

	gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datauploads"}
	updated, err := fakeDynamic.Resource(gvr).Namespace(backup.Namespace).Get(context.Background(), "du-cancel-retry", metav1.GetOptions{})
	require.NoError(t, err)
	updatedDU := &velerov2alpha1.DataUpload{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(updated.Object, updatedDU))
	assert.True(t, updatedDU.Spec.Cancel)
}

func TestBackupPlugin_Cancel_PatchFails(t *testing.T) {
	withFastCancelBackoff(t)

	const operationID = "op-cancel-fail-1"
	backup := &velerov1.Backup{ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero"}}

	du := &velerov2alpha1.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "du-cancel-fail",
			Namespace: backup.Namespace,
			Labels: map[string]string{
				velerov1.BackupNameLabel: controllercommon.SafeLabelValue(backup.Name),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationOperationID: operationID,
			},
		},
		Spec: velerov2alpha1.DataUploadSpec{Cancel: false},
	}

	fakeDynamic := newDataUploadDynamicClient(t, dataUploadUnstructured(t, du))
	attempts := 0
	fakeDynamic.PrependReactor("patch", "datauploads", func(action k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		return true, nil, fmt.Errorf("simulated patch failure")
	})
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	err := plugin.Cancel(operationID, backup)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to update DataUpload for cancellation")
	assert.Equal(t, cancelPatchBackoff.Steps, attempts, "Cancel must exhaust the configured retry attempts")
}

func TestBackupPlugin_Cancel_DoesNotRetryNonRetryableError(t *testing.T) {
	withFastCancelBackoff(t)

	const operationID = "op-cancel-nonretryable-1"
	backup := &velerov1.Backup{ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero"}}

	du := &velerov2alpha1.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "du-cancel-nonretryable",
			Namespace: backup.Namespace,
			Labels: map[string]string{
				velerov1.BackupNameLabel: controllercommon.SafeLabelValue(backup.Name),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationOperationID: operationID,
			},
		},
		Spec: velerov2alpha1.DataUploadSpec{Cancel: false},
	}

	fakeDynamic := newDataUploadDynamicClient(t, dataUploadUnstructured(t, du))
	attempts := 0
	fakeDynamic.PrependReactor("patch", "datauploads", func(action k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "velero.io", Resource: "datauploads"}, "du-cancel-nonretryable")
	})
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	err := plugin.Cancel(operationID, backup)

	require.Error(t, err)
	assert.Equal(t, 1, attempts, "a NotFound patch error is not retryable and must stop after a single attempt")
}

func TestBackupPlugin_Cancel_NotFound(t *testing.T) {
	backup := &velerov1.Backup{ObjectMeta: metav1.ObjectMeta{Name: testBackupName, Namespace: "velero"}}

	fakeDynamic := newDataUploadDynamicClient(t)
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &BackupPlugin{Log: newTestLogger()}

	err := plugin.Cancel("missing-op", backup)
	assert.NoError(t, err)
}

func TestGenerateOperationID(t *testing.T) {
	operationID := generateOperationID("backup-1", "namespace-1", "vm-1")

	assert.NotEmpty(t, operationID)
	assert.Contains(t, operationID, "backup-1")
	assert.Contains(t, operationID, "namespace-1")
	assert.Contains(t, operationID, "vm-1")

	// Deterministic by design (see deterministicSuffix doc comment): a retried
	// Execute() call for the same (backup, namespace, vm) must converge on the
	// same operation ID so it can find the DataUpload it already created.
	operationID2 := generateOperationID("backup-1", "namespace-1", "vm-1")
	assert.Equal(t, operationID, operationID2, "same inputs must produce the same operation ID")

	operationID3 := generateOperationID("backup-2", "namespace-1", "vm-1")
	assert.NotEqual(t, operationID, operationID3, "different inputs must produce a different operation ID")

	operationID4 := generateOperationID("backup-1", "namespace-2", "vm-1")
	assert.NotEqual(t, operationID, operationID4, "a different namespace must produce a different operation ID")

	operationID5 := generateOperationID("backup-1", "namespace-1", "vm-2")
	assert.NotEqual(t, operationID, operationID5, "a different VM name must produce a different operation ID")
}

func TestGenerateDataUploadName(t *testing.T) {
	name := generateDataUploadName("backup-1", "namespace-1", "vm-1")

	assert.NotEmpty(t, name)
	assert.Contains(t, name, "du-")
	assert.Contains(t, name, "backup-1")
	assert.Contains(t, name, "namespace-1")
	assert.Contains(t, name, "vm-1")
	assert.LessOrEqual(t, len(name), 253)
	assert.Empty(t, validation.IsDNS1123Subdomain(name), "name must be a valid object name")

	// Same backup+VM name but a different namespace must produce a different
	// DataUpload name -- this is what prevents same-named VMs backed up from
	// different namespaces within one backup from colliding on both name and
	// operationID (see generateDataUploadName's doc comment).
	otherNSName := generateDataUploadName("backup-1", "namespace-2", "vm-1")
	assert.NotEqual(t, name, otherNSName, "different namespace must produce a different DataUpload name")

	// Test with very long names
	longBackupName := make([]byte, 200)
	for i := range longBackupName {
		longBackupName[i] = 'a'
	}
	longVMName := make([]byte, 100)
	for i := range longVMName {
		longVMName[i] = 'b'
	}

	longName := generateDataUploadName(string(longBackupName), "namespace-1", string(longVMName))
	assert.LessOrEqual(t, len(longName), 253, "generated name should not exceed 253 characters")
	assert.Empty(t, validation.IsDNS1123Subdomain(longName), "truncated name must remain a valid object name")

	// Two long backup names sharing the same truncated prefix (only the very
	// last byte differs, past where truncation would cut) must still produce
	// distinct DataUpload names -- the hash suffix is computed over the full,
	// untruncated inputs, so it's what actually guarantees uniqueness once
	// truncation collapses the visible prefix to the same bytes.
	otherLongBackupName := string(longBackupName[:199]) + "c"
	otherLongName := generateDataUploadName(otherLongBackupName, "namespace-1", string(longVMName))
	assert.NotEqual(t, longName, otherLongName, "long inputs must retain unique DataUpload names")
}

func TestBackupPlugin_Progress(t *testing.T) {
	// Skip this test if not running with proper integration setup
	// The Progress function requires kubernetes client setup
	t.Skip("Skipping Progress test - requires integration test setup with kubernetes client")
}

// Helper functions

// createTestVM creates a test VM without CBT status (for backward compatibility tests).
func createTestVM(namespace, name string, status kvcore.VirtualMachinePrintableStatus) *kvcore.VirtualMachine {
	return &kvcore.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kubevirt.io/v1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: kvcore.VirtualMachineSpec{
			Template: &kvcore.VirtualMachineInstanceTemplateSpec{
				Spec: kvcore.VirtualMachineInstanceSpec{
					Volumes: []kvcore.Volume{},
				},
			},
		},
		Status: kvcore.VirtualMachineStatus{
			PrintableStatus: status,
			Created:         status == kvcore.VirtualMachineStatusRunning || status == kvcore.VirtualMachineStatusStarting,
		},
	}
}

// createTestVMWithCBT creates a test VM with ChangedBlockTracking enabled.
func createTestVMWithCBT(namespace, name string, status kvcore.VirtualMachinePrintableStatus) *kvcore.VirtualMachine {
	vm := createTestVM(namespace, name, status)
	vm.Status.ChangedBlockTracking = &kvcore.ChangedBlockTrackingStatus{
		State: kvcore.ChangedBlockTrackingEnabled,
	}
	return vm
}

func vmToUnstructured(t *testing.T, vm *kvcore.VirtualMachine) runtime.Unstructured {
	vmMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(vm)
	require.NoError(t, err)
	return &unstructured.Unstructured{Object: vmMap}
}

func TestBuildDataUploadAnnotations(t *testing.T) {
	operationID := "test-op-123"

	t.Run("includes required annotations", func(t *testing.T) {
		vm := &kvcore.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-vm",
				Namespace: "my-ns",
			},
		}

		annotations := buildDataUploadAnnotations(vm, operationID)

		assert.Equal(t, "my-vm", annotations[controllercommon.AnnotationVMName])
		assert.Equal(t, "my-ns", annotations[controllercommon.AnnotationVMNamespace])
		assert.Equal(t, operationID, annotations[controllercommon.AnnotationOperationID])
		assert.NotContains(t, annotations, "kubevirt-datamover.io/backup-pvc-size")
	})

	t.Run("propagates backup-pvc-size from VM", func(t *testing.T) {
		vm := &kvcore.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-vm",
				Namespace: "my-ns",
				Annotations: map[string]string{
					"kubevirt-datamover.io/backup-pvc-size": "50Gi",
				},
			},
		}

		annotations := buildDataUploadAnnotations(vm, operationID)

		assert.Equal(t, "50Gi", annotations["kubevirt-datamover.io/backup-pvc-size"])
		assert.Equal(t, "my-vm", annotations[controllercommon.AnnotationVMName])
	})

	t.Run("ignores empty backup-pvc-size annotation", func(t *testing.T) {
		vm := &kvcore.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-vm",
				Namespace: "my-ns",
				Annotations: map[string]string{
					"kubevirt-datamover.io/backup-pvc-size": "",
				},
			},
		}

		annotations := buildDataUploadAnnotations(vm, operationID)

		assert.NotContains(t, annotations, "kubevirt-datamover.io/backup-pvc-size")
	})
}
