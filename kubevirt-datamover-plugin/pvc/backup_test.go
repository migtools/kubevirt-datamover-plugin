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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
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
	plugin := NewBackupPlugin(newTestLogger())

	selector, err := plugin.AppliesTo()

	require.NoError(t, err)
	assert.Equal(t, velero.ResourceSelector{
		IncludedResources: []string{"persistentvolumeclaims"},
	}, selector)
}

func TestBackupPlugin_Name(t *testing.T) {
	plugin := NewBackupPlugin(newTestLogger())

	name := plugin.Name()

	assert.Equal(t, "kubevirt-pvc-backup-plugin", name)
}

func TestBackupPlugin_Execute_NilBackup(t *testing.T) {
	plugin := NewBackupPlugin(newTestLogger())

	pvc := createTestPVC(testNamespace, testPVCName)
	item := pvcToUnstructured(t, pvc)

	_, _, _, _, err := plugin.Execute(item, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup object is nil")
}

// Helper functions

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
		Spec: corev1.PersistentVolumeClaimSpec{},
	}
}

func pvcToUnstructured(t *testing.T, pvc *corev1.PersistentVolumeClaim) runtime.Unstructured {
	pvcMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pvc)
	require.NoError(t, err)
	return &unstructured.Unstructured{Object: pvcMap}
}
