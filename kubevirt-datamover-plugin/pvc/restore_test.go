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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/velero/pkg/apis/velero/shared"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	velerov2alpha1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"

	controllercommon "github.com/migtools/kubevirt-datamover-controller/pkg/common"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/clients"
)

const (
	testRestorePVCName = "test-restore-pvc"
	testRestoreName    = "test-restore"
	testOrigBackupName = "test-orig-backup"
	testVeleroNS       = "velero"
	testOrigNamespace  = "orig-ns"
)

func newDataDownloadDynamicClient(t *testing.T, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
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

func newRestorePVC(namespace, name string, annotations map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
}

func restorePVCToUnstructured(t *testing.T, pvc *corev1.PersistentVolumeClaim) runtime.Unstructured {
	pvcMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pvc)
	require.NoError(t, err)
	return &unstructured.Unstructured{Object: pvcMap}
}

func dataDownloadUnstructured(t *testing.T, dd *velerov2alpha1.DataDownload) *unstructured.Unstructured {
	ddMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dd)
	require.NoError(t, err)
	u := &unstructured.Unstructured{Object: ddMap}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "velero.io", Version: "v2alpha1", Kind: "DataDownload"})
	return u
}

func getFakeCRClient(t *testing.T, objects ...crclient.Object) crclient.Client {
	scheme := runtime.NewScheme()
	require.NoError(t, velerov1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestRestorePlugin_AppliesTo(t *testing.T) {
	plugin := &RestorePlugin{Log: newTestLogger()}

	selector, err := plugin.AppliesTo()

	require.NoError(t, err)
	assert.Equal(t, velero.ResourceSelector{
		IncludedResources: []string{"persistentvolumeclaims"},
	}, selector)
}

func TestRestorePlugin_Name(t *testing.T) {
	plugin := &RestorePlugin{Log: newTestLogger()}

	assert.Equal(t, "kubevirt-pvc-restore-plugin", plugin.Name())
}

func TestRestorePlugin_Execute_NilRestore(t *testing.T) {
	plugin := &RestorePlugin{Log: newTestLogger()}

	pvcItem := restorePVCToUnstructured(t, newRestorePVC(testOrigNamespace, testRestorePVCName, nil))

	_, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           pvcItem,
		ItemFromBackup: pvcItem,
		Restore:        nil,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "restore object is nil")
}

func TestRestorePlugin_Execute_NotEligible(t *testing.T) {
	plugin := &RestorePlugin{Log: newTestLogger()}

	pvc := newRestorePVC(testOrigNamespace, testRestorePVCName, nil)
	pvcItem := restorePVCToUnstructured(t, pvc)
	expectedItem := pvcItem.DeepCopyObject().(runtime.Unstructured)
	itemFromBackup := restorePVCToUnstructured(t, pvc)
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	output, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           pvcItem,
		ItemFromBackup: itemFromBackup,
		Restore:        restore,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Equal(t, expectedItem, output.UpdatedItem)
	assert.Equal(t, expectedItem, pvcItem, "Execute must not mutate the passed-in Item")
	assert.Equal(t, expectedItem, itemFromBackup, "Execute must not mutate the passed-in ItemFromBackup")
	assert.Empty(t, output.OperationID)
	assert.Empty(t, output.AdditionalItems)
}

func TestRestorePlugin_Execute_Eligible(t *testing.T) {
	testCases := []struct {
		name                 string
		namespaceMapping     map[string]string
		expectedTargetNS     string
		itemOperationTimeout metav1.Duration
		expectedTimeout      time.Duration
	}{
		{
			name:             "no namespace remap",
			namespaceMapping: nil,
			expectedTargetNS: testOrigNamespace,
			expectedTimeout:  4 * time.Hour,
		},
		{
			name:             "namespace remapped",
			namespaceMapping: map[string]string{testOrigNamespace: "remapped-ns"},
			expectedTargetNS: "remapped-ns",
			expectedTimeout:  4 * time.Hour,
		},
		{
			name:             "namespace mapping present but empty value is treated as no remap",
			namespaceMapping: map[string]string{testOrigNamespace: ""},
			expectedTargetNS: testOrigNamespace,
			expectedTimeout:  4 * time.Hour,
		},
		{
			name:                 "custom ItemOperationTimeout propagated",
			namespaceMapping:     nil,
			expectedTargetNS:     testOrigNamespace,
			itemOperationTimeout: metav1.Duration{Duration: 90 * time.Minute},
			expectedTimeout:      90 * time.Minute,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fakeDynamic := newDataDownloadDynamicClient(t)
			withFakeDynamicClient(t, fakeDynamic)

			backup := &velerov1.Backup{
				ObjectMeta: metav1.ObjectMeta{Name: testOrigBackupName, Namespace: testVeleroNS},
				Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
			}
			fakeCRClient := getFakeCRClient(t, backup)
			plugin, err := NewRestorePlugin(newTestLogger(), &fakeCRClient)
			require.NoError(t, err)

			pvc := newRestorePVC(testOrigNamespace, testRestorePVCName, map[string]string{
				controllercommon.AnnotationVMName: "my-vm",
			})
			itemFromBackup := restorePVCToUnstructured(t, pvc)
			// Item deliberately carries no AnnotationVMName: eligibility and VM
			// identity must be read from ItemFromBackup, never from Item (see the
			// doc comment on Execute). If the code mistakenly read from Item
			// instead, this PVC would be treated as ineligible and this test would
			// fail with an empty OperationID.
			item := restorePVCToUnstructured(t, newRestorePVC(testOrigNamespace, testRestorePVCName, nil))
			expectedItem := item.DeepCopyObject().(runtime.Unstructured)
			restore := &velerov1.Restore{
				ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS, UID: "restore-uid"},
				Spec: velerov1.RestoreSpec{
					BackupName:           testOrigBackupName,
					NamespaceMapping:     tc.namespaceMapping,
					ItemOperationTimeout: tc.itemOperationTimeout,
				},
			}

			output, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{
				Item:           item,
				ItemFromBackup: itemFromBackup,
				Restore:        restore,
			})

			require.NoError(t, err)
			require.NotNil(t, output)
			assert.Equal(t, expectedItem, output.UpdatedItem, "Execute must return input.Item unmodified, not ItemFromBackup")
			assert.Equal(t, expectedItem, item, "Execute must not mutate the passed-in Item")
			assert.NotEmpty(t, output.OperationID)
			assert.Empty(t, output.AdditionalItems, "DataDownload is created live, not sourced from the backup archive, so it must not be an AdditionalItem")

			gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datadownloads"}
			list, err := fakeDynamic.Resource(gvr).Namespace(testVeleroNS).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, list.Items, 1)
			created := list.Items[0]

			dd := &velerov2alpha1.DataDownload{}
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(created.Object, dd))

			assert.True(t, strings.HasPrefix(dd.Name, fmt.Sprintf("dd-%s-%s-%s-", testRestoreName, testOrigNamespace, testRestorePVCName)),
				"DataDownload name must be derived from the restore name, original namespace, and PVC name via generateDataDownloadName, got %q", dd.Name)
			assert.Equal(t, controllercommon.SafeLabelValue(testOrigBackupName), dd.Labels[controllercommon.LabelVeleroBackupName])
			assert.Equal(t, controllercommon.SafeLabelValue(testRestoreName), dd.Labels[controllercommon.LabelVeleroRestoreName])
			assert.Equal(t, "my-vm", dd.Annotations[controllercommon.AnnotationVMName])
			assert.Equal(t, testOrigNamespace, dd.Annotations[controllercommon.AnnotationVMNamespace])
			assert.Equal(t, output.OperationID, dd.Annotations[controllercommon.AnnotationOperationID])
			assert.Equal(t, controllercommon.DataMoverKubeVirt, dd.Spec.DataMover)
			assert.Equal(t, "my-bsl", dd.Spec.BackupStorageLocation)
			assert.Equal(t, testOrigNamespace, dd.Spec.SourceNamespace, "SourceNamespace must stay original, pre-remap")
			assert.Equal(t, testRestorePVCName, dd.Spec.TargetVolume.PVC)
			assert.Equal(t, tc.expectedTargetNS, dd.Spec.TargetVolume.Namespace, "TargetVolume.Namespace must reflect NamespaceMapping")
			assert.Equal(t, "placeholder-not-used", dd.Spec.SnapshotID)
			assert.Equal(t, tc.expectedTimeout, dd.Spec.OperationTimeout.Duration)
			assert.False(t, dd.Spec.Cancel)
			require.Len(t, dd.OwnerReferences, 1)
			assert.Equal(t, "Restore", dd.OwnerReferences[0].Kind)
			assert.Equal(t, testRestoreName, dd.OwnerReferences[0].Name)
			assert.Equal(t, restore.UID, dd.OwnerReferences[0].UID)
		})
	}
}

func TestRestorePlugin_Execute_LongOperationIDTruncatedInLabel(t *testing.T) {
	fakeDynamic := newDataDownloadDynamicClient(t)
	withFakeDynamicClient(t, fakeDynamic)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testOrigBackupName, Namespace: testVeleroNS},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	fakeCRClient := getFakeCRClient(t, backup)
	plugin, err := NewRestorePlugin(newTestLogger(), &fakeCRClient)
	require.NoError(t, err)

	longRestoreName := strings.Repeat("r", 40)
	longPVCName := strings.Repeat("p", 40)
	pvc := newRestorePVC(testOrigNamespace, longPVCName, map[string]string{
		controllercommon.AnnotationVMName: "my-vm",
	})
	pvcItem := restorePVCToUnstructured(t, pvc)
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: longRestoreName, Namespace: testVeleroNS, UID: "restore-uid"},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	output, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           pvcItem,
		ItemFromBackup: pvcItem,
		Restore:        restore,
	})
	require.NoError(t, err)
	require.Greater(t, len(output.OperationID), 63, "test setup should produce an operationID long enough to force label truncation")

	gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datadownloads"}
	list, err := fakeDynamic.Resource(gvr).Namespace(testVeleroNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)

	dd := &velerov2alpha1.DataDownload{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[0].Object, dd))

	assert.Equal(t, output.OperationID, dd.Annotations[controllercommon.AnnotationOperationID], "annotation must keep the full, untruncated operationID")
	assert.LessOrEqual(t, len(dd.Labels[controllercommon.AnnotationOperationID]), 63, "label must be SafeLabelValue-truncated to fit the 63-char label limit")
	assert.Equal(t, controllercommon.SafeLabelValue(output.OperationID), dd.Labels[controllercommon.AnnotationOperationID])

	// Progress must still find it: the annotation exact-match survives even
	// though the label alone is truncated/hashed and not directly comparable
	// to the full operationID.
	progress, err := plugin.Progress(output.OperationID, restore)
	require.NoError(t, err)
	assert.False(t, progress.Completed)
}

func TestRestorePlugin_Execute_MissingBackup(t *testing.T) {
	fakeDynamic := newDataDownloadDynamicClient(t)
	withFakeDynamicClient(t, fakeDynamic)

	// No Backup object seeded into the fake CR client.
	fakeCRClient := getFakeCRClient(t)
	plugin, err := NewRestorePlugin(newTestLogger(), &fakeCRClient)
	require.NoError(t, err)

	pvc := newRestorePVC(testOrigNamespace, testRestorePVCName, map[string]string{
		controllercommon.AnnotationVMName: "my-vm",
	})
	pvcItem := restorePVCToUnstructured(t, pvc)
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	_, err = plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           pvcItem,
		ItemFromBackup: pvcItem,
		Restore:        restore,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get Backup")
}

func TestRestorePlugin_Execute_MissingStorageLocation(t *testing.T) {
	fakeDynamic := newDataDownloadDynamicClient(t)
	withFakeDynamicClient(t, fakeDynamic)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testOrigBackupName, Namespace: testVeleroNS},
		// StorageLocation deliberately left empty.
	}
	fakeCRClient := getFakeCRClient(t, backup)
	plugin, err := NewRestorePlugin(newTestLogger(), &fakeCRClient)
	require.NoError(t, err)

	pvc := newRestorePVC(testOrigNamespace, testRestorePVCName, map[string]string{
		controllercommon.AnnotationVMName: "my-vm",
	})
	pvcItem := restorePVCToUnstructured(t, pvc)
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	_, err = plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           pvcItem,
		ItemFromBackup: pvcItem,
		Restore:        restore,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no spec.storageLocation")
}

func TestRestorePlugin_Execute_CreateDataDownloadFails(t *testing.T) {
	fakeDynamic := newDataDownloadDynamicClient(t)
	fakeDynamic.PrependReactor("create", "datadownloads", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated create failure")
	})
	withFakeDynamicClient(t, fakeDynamic)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testOrigBackupName, Namespace: testVeleroNS},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	fakeCRClient := getFakeCRClient(t, backup)
	plugin, err := NewRestorePlugin(newTestLogger(), &fakeCRClient)
	require.NoError(t, err)

	pvc := newRestorePVC(testOrigNamespace, testRestorePVCName, map[string]string{
		controllercommon.AnnotationVMName: "my-vm",
	})
	pvcItem := restorePVCToUnstructured(t, pvc)
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	_, err = plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           pvcItem,
		ItemFromBackup: pvcItem,
		Restore:        restore,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create DataDownload")
}

func TestRestorePlugin_Execute_AlreadyExists_Reuse(t *testing.T) {
	const pvcName = testRestorePVCName

	// Precompute the exact name/operationID Execute() will derive, and seed the
	// fake dynamic client with a matching DataDownload already present -- this
	// simulates a prior Execute() call for this same (restore, PVC) having
	// already created it (e.g. Velero retried after a transient RPC error).
	existingOperationID := generateOperationID(testRestoreName, testOrigNamespace, pvcName)
	existingName := generateDataDownloadName(testRestoreName, testOrigNamespace, pvcName)

	existing := &velerov2alpha1.DataDownload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      existingName,
			Namespace: testVeleroNS,
			Labels: map[string]string{
				controllercommon.LabelVeleroBackupName:  controllercommon.SafeLabelValue(testOrigBackupName),
				controllercommon.LabelVeleroRestoreName: controllercommon.SafeLabelValue(testRestoreName),
				controllercommon.AnnotationOperationID:  controllercommon.SafeLabelValue(existingOperationID),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationVMName:      "my-vm",
				controllercommon.AnnotationVMNamespace: testOrigNamespace,
				controllercommon.AnnotationOperationID: existingOperationID,
			},
		},
		Spec: velerov2alpha1.DataDownloadSpec{
			DataMover:             controllercommon.DataMoverKubeVirt,
			BackupStorageLocation: "my-bsl",
			SourceNamespace:       testOrigNamespace,
			TargetVolume: velerov2alpha1.TargetVolumeSpec{
				PVC:       pvcName,
				Namespace: testOrigNamespace,
			},
		},
	}

	fakeDynamic := newDataDownloadDynamicClient(t, dataDownloadUnstructured(t, existing))
	withFakeDynamicClient(t, fakeDynamic)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testOrigBackupName, Namespace: testVeleroNS},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	fakeCRClient := getFakeCRClient(t, backup)
	plugin, err := NewRestorePlugin(newTestLogger(), &fakeCRClient)
	require.NoError(t, err)

	pvc := newRestorePVC(testOrigNamespace, pvcName, map[string]string{
		controllercommon.AnnotationVMName: "my-vm",
	})
	pvcItem := restorePVCToUnstructured(t, pvc)
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	output, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           pvcItem,
		ItemFromBackup: pvcItem,
		Restore:        restore,
	})

	require.NoError(t, err, "Execute must reuse the existing DataDownload instead of erroring on AlreadyExists")
	require.NotNil(t, output)
	assert.Equal(t, existingOperationID, output.OperationID)

	gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datadownloads"}
	list, err := fakeDynamic.Resource(gvr).Namespace(testVeleroNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, list.Items, 1, "Execute must not create a duplicate DataDownload for a retried call")
}

func TestRestorePlugin_Execute_AlreadyExists_Mismatch(t *testing.T) {
	const pvcName = testRestorePVCName

	existingName := generateDataDownloadName(testRestoreName, testOrigNamespace, pvcName)

	// Same deterministic name, but targeting a different PVC -- must not be
	// mistaken for "our" operation and silently reused.
	existing := &velerov2alpha1.DataDownload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      existingName,
			Namespace: testVeleroNS,
		},
		Spec: velerov2alpha1.DataDownloadSpec{
			TargetVolume: velerov2alpha1.TargetVolumeSpec{
				PVC:       "some-other-pvc",
				Namespace: testOrigNamespace,
			},
		},
	}

	fakeDynamic := newDataDownloadDynamicClient(t, dataDownloadUnstructured(t, existing))
	withFakeDynamicClient(t, fakeDynamic)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testOrigBackupName, Namespace: testVeleroNS},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	fakeCRClient := getFakeCRClient(t, backup)
	plugin, err := NewRestorePlugin(newTestLogger(), &fakeCRClient)
	require.NoError(t, err)

	pvc := newRestorePVC(testOrigNamespace, pvcName, map[string]string{
		controllercommon.AnnotationVMName: "my-vm",
	})
	pvcItem := restorePVCToUnstructured(t, pvc)
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	_, err = plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           pvcItem,
		ItemFromBackup: pvcItem,
		Restore:        restore,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to reuse")
}

func TestRestorePlugin_Execute_AlreadyExists_NamespaceMismatch(t *testing.T) {
	const pvcName = testRestorePVCName

	existingName := generateDataDownloadName(testRestoreName, testOrigNamespace, pvcName)

	// Same deterministic name and same PVC name, but a different TargetVolume
	// namespace -- must not be mistaken for "our" operation and silently reused.
	existing := &velerov2alpha1.DataDownload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      existingName,
			Namespace: testVeleroNS,
		},
		Spec: velerov2alpha1.DataDownloadSpec{
			TargetVolume: velerov2alpha1.TargetVolumeSpec{
				PVC:       pvcName,
				Namespace: "some-other-ns",
			},
		},
	}

	fakeDynamic := newDataDownloadDynamicClient(t, dataDownloadUnstructured(t, existing))
	withFakeDynamicClient(t, fakeDynamic)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testOrigBackupName, Namespace: testVeleroNS},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	fakeCRClient := getFakeCRClient(t, backup)
	plugin, err := NewRestorePlugin(newTestLogger(), &fakeCRClient)
	require.NoError(t, err)

	pvc := newRestorePVC(testOrigNamespace, pvcName, map[string]string{
		controllercommon.AnnotationVMName: "my-vm",
	})
	pvcItem := restorePVCToUnstructured(t, pvc)
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	_, err = plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           pvcItem,
		ItemFromBackup: pvcItem,
		Restore:        restore,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to reuse")
}

func TestRestorePlugin_Execute_AlreadyExists_MissingOperationIDAnnotation(t *testing.T) {
	const pvcName = testRestorePVCName

	existingName := generateDataDownloadName(testRestoreName, testOrigNamespace, pvcName)

	// Same deterministic name and same target PVC/namespace (so the PVC/namespace
	// match check passes), but missing the operationID annotation entirely -- e.g.
	// created by an older plugin build with a different tracking scheme. Adopting
	// it silently would strand Progress/Cancel: they look up DataDownloads by an
	// exact annotation match against operationID, which this object could never
	// satisfy.
	existing := &velerov2alpha1.DataDownload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      existingName,
			Namespace: testVeleroNS,
			Annotations: map[string]string{
				controllercommon.AnnotationVMName:      "my-vm",
				controllercommon.AnnotationVMNamespace: testOrigNamespace,
			},
		},
		Spec: velerov2alpha1.DataDownloadSpec{
			SourceNamespace: testOrigNamespace,
			TargetVolume: velerov2alpha1.TargetVolumeSpec{
				PVC:       pvcName,
				Namespace: testOrigNamespace,
			},
		},
	}

	fakeDynamic := newDataDownloadDynamicClient(t, dataDownloadUnstructured(t, existing))
	withFakeDynamicClient(t, fakeDynamic)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testOrigBackupName, Namespace: testVeleroNS},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	fakeCRClient := getFakeCRClient(t, backup)
	plugin, err := NewRestorePlugin(newTestLogger(), &fakeCRClient)
	require.NoError(t, err)

	pvc := newRestorePVC(testOrigNamespace, pvcName, map[string]string{
		controllercommon.AnnotationVMName: "my-vm",
	})
	pvcItem := restorePVCToUnstructured(t, pvc)
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	_, err = plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           pvcItem,
		ItemFromBackup: pvcItem,
		Restore:        restore,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to reuse")
	assert.Contains(t, err.Error(), controllercommon.AnnotationOperationID)
}

func TestRestorePlugin_Execute_AlreadyExists_MissingOperationIDLabel(t *testing.T) {
	const pvcName = testRestorePVCName
	const existingOperationID = "some-operation-id"

	existingName := generateDataDownloadName(testRestoreName, testOrigNamespace, pvcName)

	// The operationID annotation is present (so the annotation-missing check
	// passes), but the corresponding label is not. getDataDownloadByOperationID
	// filters server-side via a label selector (backup name, restore name, and
	// operationID) before it ever inspects annotations -- an object missing any
	// of those labels would never be returned by that List call, so Progress and
	// Cancel could never find it even though the annotation matches.
	existing := &velerov2alpha1.DataDownload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      existingName,
			Namespace: testVeleroNS,
			Labels: map[string]string{
				controllercommon.LabelVeleroBackupName:  controllercommon.SafeLabelValue(testOrigBackupName),
				controllercommon.LabelVeleroRestoreName: controllercommon.SafeLabelValue(testRestoreName),
				// AnnotationOperationID label deliberately omitted.
			},
			Annotations: map[string]string{
				controllercommon.AnnotationOperationID: existingOperationID,
				controllercommon.AnnotationVMName:      "my-vm",
				controllercommon.AnnotationVMNamespace: testOrigNamespace,
			},
		},
		Spec: velerov2alpha1.DataDownloadSpec{
			SourceNamespace: testOrigNamespace,
			TargetVolume: velerov2alpha1.TargetVolumeSpec{
				PVC:       pvcName,
				Namespace: testOrigNamespace,
			},
		},
	}

	fakeDynamic := newDataDownloadDynamicClient(t, dataDownloadUnstructured(t, existing))
	withFakeDynamicClient(t, fakeDynamic)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testOrigBackupName, Namespace: testVeleroNS},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	fakeCRClient := getFakeCRClient(t, backup)
	plugin, err := NewRestorePlugin(newTestLogger(), &fakeCRClient)
	require.NoError(t, err)

	pvc := newRestorePVC(testOrigNamespace, pvcName, map[string]string{
		controllercommon.AnnotationVMName: "my-vm",
	})
	pvcItem := restorePVCToUnstructured(t, pvc)
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	_, err = plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           pvcItem,
		ItemFromBackup: pvcItem,
		Restore:        restore,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to reuse")
	assert.Contains(t, err.Error(), controllercommon.AnnotationOperationID)
}

// countingCRClient wraps a crclient.Client and counts Get calls, used to prove
// RestorePlugin.getBackup caches the Backup instead of re-fetching it per PVC.
type countingCRClient struct {
	crclient.Client
	getCount int
}

func (c *countingCRClient) Get(ctx context.Context, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
	if _, ok := obj.(*velerov1.Backup); ok {
		c.getCount++
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestRestorePlugin_Execute_CachesBackupAcrossCalls(t *testing.T) {
	fakeDynamic := newDataDownloadDynamicClient(t)
	withFakeDynamicClient(t, fakeDynamic)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testOrigBackupName, Namespace: testVeleroNS},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	counting := &countingCRClient{Client: getFakeCRClient(t, backup)}
	var crClientIface crclient.Client = counting
	plugin, err := NewRestorePlugin(newTestLogger(), &crClientIface)
	require.NoError(t, err)

	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS, UID: "restore-uid"},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	for _, pvcName := range []string{"pvc-a", "pvc-b", "pvc-c"} {
		pvc := newRestorePVC(testOrigNamespace, pvcName, map[string]string{
			controllercommon.AnnotationVMName: "my-vm",
		})
		pvcItem := restorePVCToUnstructured(t, pvc)
		_, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{
			Item:           pvcItem,
			ItemFromBackup: pvcItem,
			Restore:        restore,
		})
		require.NoError(t, err)
	}

	assert.Equal(t, 1, counting.getCount, "the Backup should be fetched once and cached across multiple Execute calls on the same plugin instance")
}

func TestRestorePlugin_Execute_SameNamedPVCDifferentNamespaces(t *testing.T) {
	fakeDynamic := newDataDownloadDynamicClient(t)
	withFakeDynamicClient(t, fakeDynamic)

	backup := &velerov1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testOrigBackupName, Namespace: testVeleroNS},
		Spec:       velerov1.BackupSpec{StorageLocation: "my-bsl"},
	}
	fakeCRClient := getFakeCRClient(t, backup)
	plugin, err := NewRestorePlugin(newTestLogger(), &fakeCRClient)
	require.NoError(t, err)

	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS, UID: "restore-uid"},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	// Two PVCs with the same name, but from different source namespaces within
	// the same restore -- without namespace in generateDataDownloadName's inputs,
	// these would collide on both DataDownload name and operationID even though
	// they're unrelated Kubernetes objects.
	const sharedPVCName = "shared-pvc-name"
	operationIDs := make([]string, 0, 2)
	for _, ns := range []string{"ns-a", "ns-b"} {
		pvc := newRestorePVC(ns, sharedPVCName, map[string]string{
			controllercommon.AnnotationVMName: "my-vm",
		})
		pvcItem := restorePVCToUnstructured(t, pvc)
		output, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{
			Item:           pvcItem,
			ItemFromBackup: pvcItem,
			Restore:        restore,
		})
		require.NoError(t, err, "same-named PVCs from different source namespaces within one restore must not collide")
		operationIDs = append(operationIDs, output.OperationID)
	}

	gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datadownloads"}
	list, err := fakeDynamic.Resource(gvr).Namespace(testVeleroNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, list.Items, 2, "each source namespace's PVC must get its own DataDownload, not a collided/reused one")
	assert.NotEqual(t, operationIDs[0], operationIDs[1], "same-named PVCs from different source namespaces must get distinct operation IDs")
}

func TestRestorePlugin_Progress(t *testing.T) {
	const operationID = "op-progress-1"

	fixedStart := metav1.NewTime(time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC))
	fixedCreation := metav1.NewTime(time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC))

	testCases := []struct {
		name                string
		phase               velerov2alpha1.DataDownloadPhase
		progress            shared.DataMoveOperationProgress
		message             string
		startTimestamp      *metav1.Time
		expectedCompleted   bool
		expectedErrEmpty    bool
		expectedErrContains string
		expectedNTotal      int64
		expectedNCompleted  int64
	}{
		{name: "Empty phase", phase: velerov2alpha1.DataDownloadPhase(""), expectedCompleted: false, expectedErrEmpty: true},
		{name: "New", phase: velerov2alpha1.DataDownloadPhaseNew, expectedCompleted: false, expectedErrEmpty: true},
		{name: "Accepted", phase: velerov2alpha1.DataDownloadPhaseAccepted, expectedCompleted: false, expectedErrEmpty: true},
		{name: "Prepared", phase: velerov2alpha1.DataDownloadPhasePrepared, expectedCompleted: false, expectedErrEmpty: true},
		{name: "InProgress", phase: velerov2alpha1.DataDownloadPhaseInProgress, progress: shared.DataMoveOperationProgress{TotalBytes: 100, BytesDone: 40}, startTimestamp: &fixedStart, expectedCompleted: false, expectedErrEmpty: true, expectedNTotal: 100, expectedNCompleted: 40},
		{name: "Completed", phase: velerov2alpha1.DataDownloadPhaseCompleted, progress: shared.DataMoveOperationProgress{TotalBytes: 100, BytesDone: 40}, expectedCompleted: true, expectedErrEmpty: true, expectedNTotal: 100, expectedNCompleted: 100},
		{name: "Canceled", phase: velerov2alpha1.DataDownloadPhaseCanceled, expectedCompleted: true, expectedErrEmpty: false},
		{name: "Canceling", phase: velerov2alpha1.DataDownloadPhaseCanceling, expectedCompleted: false, expectedErrEmpty: true},
		{name: "Failed", phase: velerov2alpha1.DataDownloadPhaseFailed, message: "boom", expectedCompleted: true, expectedErrEmpty: false, expectedErrContains: "boom"},
		{name: "Failed with empty message", phase: velerov2alpha1.DataDownloadPhaseFailed, message: "", expectedCompleted: true, expectedErrEmpty: false, expectedErrContains: "failed without a status message"},
		{name: "Unrecognized phase", phase: velerov2alpha1.DataDownloadPhase("SomethingNew"), expectedCompleted: false, expectedErrEmpty: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dd := &velerov2alpha1.DataDownload{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "dd-1",
					Namespace:         testVeleroNS,
					CreationTimestamp: fixedCreation,
					Labels: map[string]string{
						controllercommon.LabelVeleroBackupName:  controllercommon.SafeLabelValue(testOrigBackupName),
						controllercommon.LabelVeleroRestoreName: controllercommon.SafeLabelValue(testRestoreName),
						controllercommon.AnnotationOperationID:  controllercommon.SafeLabelValue(operationID),
					},
					Annotations: map[string]string{
						controllercommon.AnnotationOperationID: operationID,
					},
				},
				Status: velerov2alpha1.DataDownloadStatus{
					Phase:          tc.phase,
					Message:        tc.message,
					Progress:       tc.progress,
					StartTimestamp: tc.startTimestamp,
				},
			}

			// A decoy DataDownload sharing the same backup/restore labels (as a
			// second disk of the same multi-disk VM restore would) but a
			// different operationID: proves Progress disambiguates via the
			// operationID annotation rather than just grabbing the first
			// label-selector match.
			decoy := &velerov2alpha1.DataDownload{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dd-decoy",
					Namespace: testVeleroNS,
					Labels: map[string]string{
						controllercommon.LabelVeleroBackupName:  controllercommon.SafeLabelValue(testOrigBackupName),
						controllercommon.LabelVeleroRestoreName: controllercommon.SafeLabelValue(testRestoreName),
						controllercommon.AnnotationOperationID:  controllercommon.SafeLabelValue("op-progress-decoy"),
					},
					Annotations: map[string]string{
						controllercommon.AnnotationOperationID: "op-progress-decoy",
					},
				},
				Status: velerov2alpha1.DataDownloadStatus{
					Phase: velerov2alpha1.DataDownloadPhaseFailed,
				},
			}

			fakeDynamic := newDataDownloadDynamicClient(t, dataDownloadUnstructured(t, dd), dataDownloadUnstructured(t, decoy))
			withFakeDynamicClient(t, fakeDynamic)

			plugin := &RestorePlugin{Log: newTestLogger()}
			restore := &velerov1.Restore{
				ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
				Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
			}

			progress, err := plugin.Progress(operationID, restore)

			require.NoError(t, err)
			assert.Equal(t, tc.expectedCompleted, progress.Completed)
			assert.Equal(t, tc.expectedNTotal, progress.NTotal)
			assert.Equal(t, tc.expectedNCompleted, progress.NCompleted)
			if tc.startTimestamp != nil {
				assert.True(t, tc.startTimestamp.Time.Equal(progress.Started), "progress.Started should be taken from DataDownload.Status.StartTimestamp when set")
			} else {
				assert.True(t, fixedCreation.Time.Equal(progress.Started),
					"progress.Started must fall back to CreationTimestamp (not time.Now()) so it stays stable across polls")
			}
			if tc.expectedErrEmpty {
				assert.Empty(t, progress.Err)
			} else {
				assert.NotEmpty(t, progress.Err)
				if tc.expectedErrContains != "" {
					assert.Contains(t, progress.Err, tc.expectedErrContains)
				}
			}
		})
	}
}

func TestRestorePlugin_Progress_LookupError(t *testing.T) {
	fakeDynamic := newDataDownloadDynamicClient(t)
	fakeDynamic.PrependReactor("list", "datadownloads", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated list failure")
	})
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &RestorePlugin{Log: newTestLogger()}
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	_, err := plugin.Progress("some-op", restore)

	require.Error(t, err, "a transient lookup failure must be surfaced as a real error, not swallowed into progress.Err")
	assert.Contains(t, err.Error(), "simulated list failure")
}

func TestRestorePlugin_Progress_NotFound(t *testing.T) {
	fakeDynamic := newDataDownloadDynamicClient(t)
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &RestorePlugin{Log: newTestLogger()}
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	progress, err := plugin.Progress("missing-op", restore)

	require.NoError(t, err)
	assert.True(t, progress.Completed)
	assert.NotEmpty(t, progress.Err)
}

func TestRestorePlugin_Progress_NilRestore(t *testing.T) {
	plugin := &RestorePlugin{Log: newTestLogger()}

	_, err := plugin.Progress("some-op", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "restore object is nil")
}

func TestRestorePlugin_Cancel(t *testing.T) {
	const operationID = "op-cancel-1"

	dd := &velerov2alpha1.DataDownload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dd-cancel",
			Namespace: testVeleroNS,
			Labels: map[string]string{
				controllercommon.LabelVeleroBackupName:  controllercommon.SafeLabelValue(testOrigBackupName),
				controllercommon.LabelVeleroRestoreName: controllercommon.SafeLabelValue(testRestoreName),
				controllercommon.AnnotationOperationID:  controllercommon.SafeLabelValue(operationID),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationOperationID: operationID,
			},
		},
		Spec: velerov2alpha1.DataDownloadSpec{Cancel: false},
		Status: velerov2alpha1.DataDownloadStatus{
			Phase:   velerov2alpha1.DataDownloadPhaseInProgress,
			Message: "controller-owned status",
		},
	}

	// A decoy DataDownload sharing the same backup/restore labels (as a second
	// disk of the same multi-disk VM restore would) but a different
	// operationID: proves Cancel only patches the DataDownload matching the
	// supplied operationID and leaves the decoy's Spec.Cancel untouched.
	decoy := &velerov2alpha1.DataDownload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dd-cancel-decoy",
			Namespace: testVeleroNS,
			Labels: map[string]string{
				controllercommon.LabelVeleroBackupName:  controllercommon.SafeLabelValue(testOrigBackupName),
				controllercommon.LabelVeleroRestoreName: controllercommon.SafeLabelValue(testRestoreName),
				controllercommon.AnnotationOperationID:  controllercommon.SafeLabelValue("op-cancel-decoy"),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationOperationID: "op-cancel-decoy",
			},
		},
		Spec: velerov2alpha1.DataDownloadSpec{Cancel: false},
	}

	fakeDynamic := newDataDownloadDynamicClient(t, dataDownloadUnstructured(t, dd), dataDownloadUnstructured(t, decoy))
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &RestorePlugin{Log: newTestLogger()}
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	err := plugin.Cancel(operationID, restore)
	require.NoError(t, err)

	gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datadownloads"}
	updated, err := fakeDynamic.Resource(gvr).Namespace(testVeleroNS).Get(context.Background(), "dd-cancel", metav1.GetOptions{})
	require.NoError(t, err)

	updatedDD := &velerov2alpha1.DataDownload{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(updated.Object, updatedDD))
	assert.True(t, updatedDD.Spec.Cancel)
	assert.Equal(t, velerov2alpha1.DataDownloadPhaseInProgress, updatedDD.Status.Phase,
		"Cancel must patch only spec.cancel and must not clobber the controller-owned Status")
	assert.Equal(t, "controller-owned status", updatedDD.Status.Message)

	decoyAfter, err := fakeDynamic.Resource(gvr).Namespace(testVeleroNS).Get(context.Background(), "dd-cancel-decoy", metav1.GetOptions{})
	require.NoError(t, err)
	decoyDD := &velerov2alpha1.DataDownload{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(decoyAfter.Object, decoyDD))
	assert.False(t, decoyDD.Spec.Cancel, "Cancel must not touch DataDownloads for other operations")
}

func TestRestorePlugin_Cancel_PatchFails(t *testing.T) {
	withFastCancelBackoff(t)

	const operationID = "op-cancel-2"

	dd := &velerov2alpha1.DataDownload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dd-cancel-2",
			Namespace: testVeleroNS,
			Labels: map[string]string{
				controllercommon.LabelVeleroBackupName:  controllercommon.SafeLabelValue(testOrigBackupName),
				controllercommon.LabelVeleroRestoreName: controllercommon.SafeLabelValue(testRestoreName),
				controllercommon.AnnotationOperationID:  controllercommon.SafeLabelValue(operationID),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationOperationID: operationID,
			},
		},
		Spec: velerov2alpha1.DataDownloadSpec{Cancel: false},
	}

	fakeDynamic := newDataDownloadDynamicClient(t, dataDownloadUnstructured(t, dd))
	fakeDynamic.PrependReactor("patch", "datadownloads", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated patch failure")
	})
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &RestorePlugin{Log: newTestLogger()}
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	err := plugin.Cancel(operationID, restore)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to update DataDownload for cancellation")
}

func TestRestorePlugin_Cancel_RetriesTransientPatchFailure(t *testing.T) {
	withFastCancelBackoff(t)

	const operationID = "op-cancel-retry-1"

	dd := &velerov2alpha1.DataDownload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dd-cancel-retry",
			Namespace: testVeleroNS,
			Labels: map[string]string{
				controllercommon.LabelVeleroBackupName:  controllercommon.SafeLabelValue(testOrigBackupName),
				controllercommon.LabelVeleroRestoreName: controllercommon.SafeLabelValue(testRestoreName),
				controllercommon.AnnotationOperationID:  controllercommon.SafeLabelValue(operationID),
			},
			Annotations: map[string]string{
				controllercommon.AnnotationOperationID: operationID,
			},
		},
		Spec: velerov2alpha1.DataDownloadSpec{Cancel: false},
	}

	fakeDynamic := newDataDownloadDynamicClient(t, dataDownloadUnstructured(t, dd))
	attempts := 0
	fakeDynamic.PrependReactor("patch", "datadownloads", func(action k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts < 3 {
			return true, nil, fmt.Errorf("simulated transient patch failure")
		}
		// Unhandled: let the request fall through to the tracker's default
		// reactor, which actually applies the patch.
		return false, nil, nil
	})
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &RestorePlugin{Log: newTestLogger()}
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	err := plugin.Cancel(operationID, restore)

	require.NoError(t, err, "Cancel must retry past transient patch failures rather than giving up after one attempt -- Velero's own timeout enforcement calls Cancel exactly once and discards the error, so this is the only retry path that exists")
	assert.Equal(t, 3, attempts)

	gvr := schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datadownloads"}
	updated, err := fakeDynamic.Resource(gvr).Namespace(testVeleroNS).Get(context.Background(), "dd-cancel-retry", metav1.GetOptions{})
	require.NoError(t, err)
	updatedDD := &velerov2alpha1.DataDownload{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(updated.Object, updatedDD))
	assert.True(t, updatedDD.Spec.Cancel)
}

func TestRestorePlugin_Cancel_NotFound(t *testing.T) {
	fakeDynamic := newDataDownloadDynamicClient(t)
	withFakeDynamicClient(t, fakeDynamic)

	plugin := &RestorePlugin{Log: newTestLogger()}
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRestoreName, Namespace: testVeleroNS},
		Spec:       velerov1.RestoreSpec{BackupName: testOrigBackupName},
	}

	err := plugin.Cancel("missing-op", restore)
	assert.NoError(t, err)
}

func TestRestorePlugin_Cancel_NilRestore(t *testing.T) {
	plugin := &RestorePlugin{Log: newTestLogger()}

	err := plugin.Cancel("some-op", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "restore object is nil")
}

func TestRestorePlugin_AreAdditionalItemsReady(t *testing.T) {
	plugin := &RestorePlugin{Log: newTestLogger()}

	ready, err := plugin.AreAdditionalItemsReady(nil, &velerov1.Restore{})

	require.NoError(t, err)
	assert.True(t, ready)
}

func TestGenerateOperationID_Restore(t *testing.T) {
	operationID := generateOperationID("restore-1", "namespace-1", "pvc-1")

	assert.NotEmpty(t, operationID)
	assert.Contains(t, operationID, "restore-1")
	assert.Contains(t, operationID, "namespace-1")
	assert.Contains(t, operationID, "pvc-1")

	// Deterministic by design (see deterministicSuffix doc comment): a retried
	// Execute() call for the same (restore, namespace, pvc) must converge on the
	// same operation ID so it can find the DataDownload it already created.
	operationID2 := generateOperationID("restore-1", "namespace-1", "pvc-1")
	assert.Equal(t, operationID, operationID2, "same inputs must produce the same operation ID")

	operationID3 := generateOperationID("restore-2", "namespace-1", "pvc-1")
	assert.NotEqual(t, operationID, operationID3, "different inputs must produce a different operation ID")
}

func TestGenerateDataDownloadName(t *testing.T) {
	name := generateDataDownloadName("restore-1", "ns-1", "pvc-1")

	assert.NotEmpty(t, name)
	assert.True(t, strings.HasPrefix(name, "dd-"), "name must start with the dd- prefix, got %q", name)
	assert.Contains(t, name, "restore-1")
	assert.Contains(t, name, "ns-1")
	assert.Contains(t, name, "pvc-1")
	assert.LessOrEqual(t, len(name), 253)

	// Same restore+PVC name but a different (target) namespace must produce a
	// different DataDownload name -- this is what prevents same-named PVCs
	// restored from different source namespaces within one restore from
	// colliding on both name and operationID (see generateDataDownloadName's
	// doc comment).
	otherNSName := generateDataDownloadName("restore-1", "ns-2", "pvc-1")
	assert.NotEqual(t, name, otherNSName, "different namespace must produce a different DataDownload name")

	long := func(n int, b byte) string {
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = b
		}
		return string(buf)
	}

	truncationCases := []struct {
		name        string
		restoreName string
		namespace   string
		pvcName     string
	}{
		{name: "all three long", restoreName: long(200, 'a'), namespace: long(200, 'c'), pvcName: long(200, 'b')},
		{name: "long restore, short others", restoreName: long(240, 'a'), namespace: "ns-1", pvcName: "pvc-1"},
		{name: "long namespace, short others", restoreName: "restore-1", namespace: long(240, 'c'), pvcName: "pvc-1"},
		{name: "long pvc, short others", restoreName: "restore-1", namespace: "ns-1", pvcName: long(240, 'b')},
		// Only pvcName is long here, so its truncation budget (maxBodyLen minus
		// the other two parts' lengths) is exact and hand-computable: 226
		// chars, landing the cut exactly on the "-" at index 225 and exercising
		// the strings.TrimRight hygiene path.
		{
			name:        "truncation boundary lands on separator",
			restoreName: "restore-1",
			namespace:   "ns-1",
			pvcName:     long(225, 'b') + "-" + long(50, 'b'),
		},
	}

	for _, tc := range truncationCases {
		t.Run(tc.name, func(t *testing.T) {
			longName := generateDataDownloadName(tc.restoreName, tc.namespace, tc.pvcName)
			assert.LessOrEqual(t, len(longName), 253, "generated name should not exceed 253 characters")
			assert.True(t, strings.HasPrefix(longName, "dd-"), "truncated name should keep the dd- prefix")
			for _, segment := range strings.Split(longName, "-") {
				assert.NotEmpty(t, segment, "truncation must not leave an empty name segment")
			}
			suffix := longName[strings.LastIndex(longName, "-")+1:]
			assert.Len(t, suffix, 8, "truncated name should keep the full 8-char hash suffix")
		})
	}
}
