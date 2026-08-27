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

func newTestPlugin(t *testing.T, client crclient.Client) *BackupPlugin {
	t.Helper()
	c := client
	plugin, err := NewBackupPlugin(newTestLogger(), &c)
	require.NoError(t, err)
	plugin.checkKubeVirtInstalled = func() (bool, error) { return true, nil }
	return plugin
}

func testBackup() *velerov1.Backup {
	return &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-backup",
			Namespace: "velero",
		},
	}
}

type stubServerGroups struct {
	groups []string
	err    error
}

func (s stubServerGroups) ServerGroups() (*metav1.APIGroupList, error) {
	if s.err != nil {
		return nil, s.err
	}
	list := &metav1.APIGroupList{}
	for _, name := range s.groups {
		list.Groups = append(list.Groups, metav1.APIGroup{Name: name})
	}
	return list, nil
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
	plugin := newTestPlugin(t, getFakeClient())

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

func TestKubevirtGroupInstalled(t *testing.T) {
	testCases := []struct {
		name    string
		stub    stubServerGroups
		want    bool
		wantErr bool
	}{
		{name: "kubevirt.io present", stub: stubServerGroups{groups: []string{"apps", kvcore.GroupVersion.Group}}, want: true},
		{name: "kubevirt.io absent", stub: stubServerGroups{groups: []string{"apps"}}, want: false},
		{name: "no groups", stub: stubServerGroups{}, want: false},
		{name: "discovery error", stub: stubServerGroups{err: fmt.Errorf("discovery failed")}, wantErr: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := kubevirtGroupInstalled(tc.stub)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBackupPlugin_Execute_KubeVirtAPIMissing(t *testing.T) {
	plugin := newTestPlugin(t, getFakeClient())
	plugin.checkKubeVirtInstalled = func() (bool, error) { return false, nil }

	item := pvcToUnstructured(t, createTestPVC(testNamespace, testPVCName))
	result, additionalItems, operationID, _, err := plugin.Execute(item, testBackup())

	require.NoError(t, err)
	assert.Equal(t, item, result)
	assert.Nil(t, additionalItems)
	assert.Empty(t, operationID)
	assert.Empty(t, result.(*unstructured.Unstructured).GetAnnotations()[controllercommon.AnnotationVMName])
}

func TestBackupPlugin_Execute_KubeVirtDiscoveryError(t *testing.T) {
	plugin := newTestPlugin(t, getFakeClient())
	plugin.checkKubeVirtInstalled = func() (bool, error) { return false, fmt.Errorf("discovery failed") }

	_, _, _, _, err := plugin.Execute(pvcToUnstructured(t, createTestPVC(testNamespace, testPVCName)), testBackup())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to detect kubevirt API")
	assert.Contains(t, err.Error(), "discovery failed")
	assert.Equal(t, kubevirtInstallUnknown, plugin.kubevirtInstall)
}

func TestBackupPlugin_Execute_ListVMsOtherError(t *testing.T) {
	intercept := &interceptListClient{
		Client:  getFakeClient(),
		listErr: fmt.Errorf("connection refused"),
	}
	plugin := newTestPlugin(t, intercept)

	_, _, _, _, err := plugin.Execute(pvcToUnstructured(t, createTestPVC(testNamespace, testPVCName)), testBackup())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get PVC list")
	assert.Contains(t, err.Error(), "failed to list VMs")
	assert.Contains(t, err.Error(), "connection refused")
}

func TestBackupPlugin_Execute_KubeVirtAPIMissingCachedAcrossNamespaces(t *testing.T) {
	checks := 0
	plugin := newTestPlugin(t, getFakeClient())
	plugin.checkKubeVirtInstalled = func() (bool, error) {
		checks++
		return false, nil
	}

	_, _, _, _, err := plugin.Execute(pvcToUnstructured(t, createTestPVC(testNamespace, testPVCName)), testBackup())
	require.NoError(t, err)
	_, _, _, _, err = plugin.Execute(pvcToUnstructured(t, createTestPVC("other-namespace", "other-pvc")), testBackup())
	require.NoError(t, err)

	assert.Equal(t, 1, checks)
	assert.Equal(t, kubevirtInstallMissing, plugin.kubevirtInstall)
}

func TestShouldListVMsLocked(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		plugin := &BackupPlugin{Log: newTestLogger()}
		plugin.checkKubeVirtInstalled = func() (bool, error) { return false, nil }

		list, err := plugin.shouldListVMsLocked()

		require.NoError(t, err)
		assert.False(t, list)
		assert.Equal(t, kubevirtInstallMissing, plugin.kubevirtInstall)
	})

	t.Run("present", func(t *testing.T) {
		plugin := &BackupPlugin{Log: newTestLogger()}
		plugin.checkKubeVirtInstalled = func() (bool, error) { return true, nil }

		list, err := plugin.shouldListVMsLocked()

		require.NoError(t, err)
		assert.True(t, list)
		assert.Equal(t, kubevirtInstallPresent, plugin.kubevirtInstall)
	})

	t.Run("discovery error", func(t *testing.T) {
		plugin := &BackupPlugin{Log: newTestLogger()}
		plugin.checkKubeVirtInstalled = func() (bool, error) { return false, fmt.Errorf("boom") }

		_, err := plugin.shouldListVMsLocked()

		require.Error(t, err)
		assert.Equal(t, kubevirtInstallUnknown, plugin.kubevirtInstall)
	})

	t.Run("cached missing skips rediscovery", func(t *testing.T) {
		checks := 0
		plugin := &BackupPlugin{
			Log:             newTestLogger(),
			kubevirtInstall: kubevirtInstallMissing,
			checkKubeVirtInstalled: func() (bool, error) {
				checks++
				return true, nil
			},
		}

		list, err := plugin.shouldListVMsLocked()

		require.NoError(t, err)
		assert.False(t, list)
		assert.Equal(t, 0, checks)
	})
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
	plugin := newTestPlugin(t, fakeCrClient)
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

// interceptListClient wraps a client and optionally forces List() to fail.
type interceptListClient struct {
	crclient.Client
	listErr   error
	listCalls int
}

func (c *interceptListClient) List(ctx context.Context, list crclient.ObjectList, opts ...crclient.ListOption) error {
	c.listCalls++
	if c.listErr != nil {
		return c.listErr
	}
	return c.Client.List(ctx, list, opts...)
}
