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
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"

	kvcore "kubevirt.io/api/core/v1"

	controllercommon "github.com/migtools/kubevirt-datamover-controller/pkg/common"
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
	plugin := NewBackupPlugin(newTestLogger())

	selector, err := plugin.AppliesTo()

	require.NoError(t, err)
	assert.Equal(t, velero.ResourceSelector{
		IncludedResources: []string{"virtualmachines.kubevirt.io"},
	}, selector)
}

func TestBackupPlugin_Name(t *testing.T) {
	plugin := NewBackupPlugin(newTestLogger())

	name := plugin.Name()

	assert.Equal(t, "kubevirt-vm-backup-plugin", name)
}

func TestBackupPlugin_Execute_NilBackup(t *testing.T) {
	plugin := NewBackupPlugin(newTestLogger())

	vm := createTestVM(testNamespace, testVMName, kvcore.VirtualMachineStatusRunning)
	item := vmToUnstructured(t, vm)

	_, _, _, _, err := plugin.Execute(item, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup object is nil")
}

func TestBackupPlugin_Execute_SnapshotMoveDataNotEnabled(t *testing.T) {
	plugin := NewBackupPlugin(newTestLogger())

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
	plugin := NewBackupPlugin(newTestLogger())

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
	plugin := NewBackupPlugin(newTestLogger())

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
			eligible, reason, err := plugin.checkPreconditions(tc.vm, tc.backup)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedEligible, eligible)
			if !tc.expectedEligible {
				assert.Contains(t, reason, tc.expectedReason)
			}
		})
	}
}

func TestGenerateOperationID(t *testing.T) {
	operationID := generateOperationID("backup-1", "namespace-1", "vm-1")

	assert.NotEmpty(t, operationID)
	assert.Contains(t, operationID, "backup-1")
	assert.Contains(t, operationID, "namespace-1")
	assert.Contains(t, operationID, "vm-1")

	// Verify uniqueness
	operationID2 := generateOperationID("backup-1", "namespace-1", "vm-1")
	assert.NotEqual(t, operationID, operationID2)
}

func TestGenerateDataUploadName(t *testing.T) {
	name := generateDataUploadName("backup-1", "vm-1")

	assert.NotEmpty(t, name)
	assert.Contains(t, name, "du-")
	assert.Contains(t, name, "backup-1")
	assert.Contains(t, name, "vm-1")
	assert.LessOrEqual(t, len(name), 253)

	// Test with very long names
	longBackupName := make([]byte, 200)
	for i := range longBackupName {
		longBackupName[i] = 'a'
	}
	longVMName := make([]byte, 100)
	for i := range longVMName {
		longVMName[i] = 'b'
	}

	longName := generateDataUploadName(string(longBackupName), string(longVMName))
	assert.LessOrEqual(t, len(longName), 253, "generated name should not exceed 253 characters")
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
