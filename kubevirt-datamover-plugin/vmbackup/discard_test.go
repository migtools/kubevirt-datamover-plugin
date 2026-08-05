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

package vmbackup

import (
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

func newTestLogger() logrus.FieldLogger {
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	return logrus.NewEntry(logger)
}

func newTestItem(kind, namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "backup.kubevirt.io/v1alpha1",
		"kind":       kind,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
	}}
}

func TestDiscardPlugin_AppliesTo(t *testing.T) {
	plugin := NewDiscardPlugin(newTestLogger())

	selector, err := plugin.AppliesTo()

	require.NoError(t, err)
	assert.Equal(t, velero.ResourceSelector{
		IncludedResources: []string{
			"virtualmachinebackups.backup.kubevirt.io",
			"virtualmachinebackuptrackers.backup.kubevirt.io",
		},
	}, selector)
}

func TestDiscardPlugin_Execute_VirtualMachineBackup(t *testing.T) {
	plugin := NewDiscardPlugin(newTestLogger())

	item := newTestItem("VirtualMachineBackup", "test-ns", "vmb-1")
	itemFromBackup := newTestItem("VirtualMachineBackup", "test-ns", "vmb-1-from-backup")

	output, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           item,
		ItemFromBackup: itemFromBackup,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.True(t, output.SkipRestore)
	assert.Equal(t, item, output.UpdatedItem)
}

func TestDiscardPlugin_Execute_VirtualMachineBackupTracker(t *testing.T) {
	plugin := NewDiscardPlugin(newTestLogger())

	item := newTestItem("VirtualMachineBackupTracker", "test-ns", "vmbt-1")
	itemFromBackup := newTestItem("VirtualMachineBackupTracker", "test-ns", "vmbt-1-from-backup")

	output, err := plugin.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           item,
		ItemFromBackup: itemFromBackup,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.True(t, output.SkipRestore)
	assert.Equal(t, item, output.UpdatedItem)
}
