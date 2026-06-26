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
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	kvcore "kubevirt.io/api/core/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"

	controllercommon "github.com/migtools/kubevirt-datamover-controller/pkg/common"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/clients"
)

const (
	testNamespace = "test-namespace"
	testPVCName   = "test-pvc"
)

// newTestLogger creates a logger for tests that discards output.
func newTestLogger() logrus.FieldLogger {
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	return logrus.NewEntry(logger)
}

func TestBackupPlugin_AppliesTo(t *testing.T) {
	fakeClient := getFakeClient()
	plugin, err := NewBackupPlugin(newTestLogger(), &fakeClient)
	require.NoError(t, err)

	selector, err := plugin.AppliesTo()

	require.NoError(t, err)
	assert.Equal(t, velero.ResourceSelector{
		IncludedResources: []string{"persistentvolumeclaims"},
	}, selector)
}

func TestBackupPlugin_Name(t *testing.T) {
	fakeClient := getFakeClient()
	plugin, err := NewBackupPlugin(newTestLogger(), &fakeClient)
	require.NoError(t, err)

	name := plugin.Name()

	assert.Equal(t, "kubevirt-pvc-backup-plugin", name)
}

func TestBackupPlugin_Execute_NilBackup(t *testing.T) {
	fakeClient := getFakeClient()
	plugin, err := NewBackupPlugin(newTestLogger(), &fakeClient)
	require.NoError(t, err)

	pvc := createTestPVC(testNamespace, testPVCName)
	item := pvcToUnstructured(t, pvc)

	_, _, _, _, err = plugin.Execute(item, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup object is nil")
}

func TestBackupPlugin_Execute_PVCWithoutVM(t *testing.T) {
	fakeClient := getFakeClient()
	plugin, err := NewBackupPlugin(newTestLogger(), &fakeClient)
	require.NoError(t, err)

	// Setup fake client
	scheme := runtime.NewScheme()
	require.NoError(t, kvcore.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeCrClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	clients.SetCRClient(fakeCrClient)
	defer clients.SetCRClient(nil)

	fakeCoreClientset := k8sfake.NewSimpleClientset()
	clients.SetCoreClient(fakeCoreClientset.CoreV1())
	defer clients.SetCoreClient(nil)

	pvc := createTestPVC(testNamespace, testPVCName)
	item := pvcToUnstructured(t, pvc)
	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-backup",
			Namespace: "velero",
		},
	}

	result, additionalItems, operationID, _, err := plugin.Execute(item, backup)

	require.NoError(t, err)
	assert.Equal(t, item, result, "should return item as-is")
	assert.Nil(t, additionalItems)
	assert.Empty(t, operationID)
}

func TestBackupPlugin_Execute_PVCWithCbtVm(t *testing.T) {
	//fakeClient := getFakeClient()

	// This is a unit test that uses a fake client.
	const vmName = "test-vm-for-pvc-backup"
	const pvName = "test-pv"
	vm := createTestVMWithCBT(testNamespace, vmName, testPVCName)
	pvc := createTestPVC(testNamespace, testPVCName)
	pvc.UID = "test-pvc-uid"
	pvc.Spec.VolumeName = pvName
	pvc.Status.Phase = corev1.ClaimBound
	pv := createTestPV(pvName, pvc)

	// Create the volume policy ConfigMap
	const volumePolicyYAML = `
# currently only supports v1 version
version: v1
volumePolicies:
- conditions: {}
  action:
    type: custom
    parameters:
      datamover: kubevirt
`
	volumePolicyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "volume-policy",
			Namespace: "velero", // Velero's namespace from the backup object
		},
		Data: map[string]string{
			"volume-policy.yaml": volumePolicyYAML,
		},
	}

	// Setup fake clients
	scheme := runtime.NewScheme()
	require.NoError(t, kvcore.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	var fakeCrClient crclient.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm, pv, pvc, volumePolicyCM).Build()
	clients.SetCRClient(fakeCrClient)
	defer clients.SetCRClient(nil) // Cleanup

	fakeCoreClientset := k8sfake.NewSimpleClientset(pvc, pv, volumePolicyCM)
	clients.SetCoreClient(fakeCoreClientset.CoreV1())
	plugin, err := NewBackupPlugin(newTestLogger(), &fakeCrClient)
	require.NoError(t, err)
	defer clients.SetCoreClient(nil)

	// Setup for plugin execution
	item := pvcToUnstructured(t, pvc)
	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-backup",
			Namespace: "velero",
		},
		Spec: velerov1.BackupSpec{
			SnapshotMoveData: boolPtr(true),
			ResourcePolicy: &corev1.TypedLocalObjectReference{
				Kind: "configmap",
				Name: "volume-policy",
			},
		},
	}

	// Execute plugin and assert
	result, _, _, _, err := plugin.Execute(item, backup)
	require.NoError(t, err)

	annotations := result.(*unstructured.Unstructured).GetAnnotations()
	require.NotNil(t, annotations)
	assert.Equal(t, vmName, annotations[controllercommon.AnnotationVMName])
}

// Helper functions

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool {
	return &b
}

// createTestPVC creates a test PVC.
func createTestPVC(namespace, name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
}

func pvcToUnstructured(t *testing.T, pvc *corev1.PersistentVolumeClaim) runtime.Unstructured {
	pvcMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pvc)
	require.NoError(t, err)
	return &unstructured.Unstructured{Object: pvcMap}
}

// createTestVM creates a test VM.
func createTestVM(namespace, name string, status kvcore.VirtualMachinePrintableStatus) *kvcore.VirtualMachine {
	runStrategy := kvcore.RunStrategyRerunOnFailure
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
			RunStrategy: &runStrategy,
			Template: &kvcore.VirtualMachineInstanceTemplateSpec{
				Spec: kvcore.VirtualMachineInstanceSpec{
					Volumes: []kvcore.Volume{},
				},
			},
		},
		Status: kvcore.VirtualMachineStatus{
			PrintableStatus: status,
			Created:         true,
		},
	}
}

// createTestVMWithCBT creates a test VM with ChangedBlockTracking enabled and a reference to a PVC.
func createTestVMWithCBT(namespace, name, pvcName string) *kvcore.VirtualMachine {
	vm := createTestVM(namespace, name, kvcore.VirtualMachineStatusRunning)
	vm.Status.ChangedBlockTracking = &kvcore.ChangedBlockTrackingStatus{
		State: kvcore.ChangedBlockTrackingEnabled,
	}
	vm.Spec.Template.Spec.Volumes = []kvcore.Volume{
		{
			Name: "disk0",
			VolumeSource: kvcore.VolumeSource{
				PersistentVolumeClaim: &kvcore.PersistentVolumeClaimVolumeSource{
					PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcName,
					},
				},
			},
		},
	}
	return vm
}

// createTestPV creates a test PV bound to a PVC.
func createTestPV(name string, pvc *corev1.PersistentVolumeClaim) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "PersistentVolume",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{
				Namespace: pvc.Namespace,
				Name:      pvc.Name,
				UID:       pvc.UID,
			},
			Capacity:                      pvc.Spec.Resources.Requests,
			AccessModes:                   pvc.Spec.AccessModes,
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		},
		Status: corev1.PersistentVolumeStatus{
			Phase: corev1.VolumeBound,
		},
	}
}
func getFakeClient() crclient.Client {
	scheme := runtime.NewScheme()
	_ = kvcore.AddToScheme(scheme)
	builder := fake.NewClientBuilder().
		WithScheme(scheme)
	fakeClient := builder.Build()
	return fakeClient
}
