package vm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestDeletePlugin_AppliesTo(t *testing.T) {
	plugin := NewDeletePlugin(newTestLogger())

	selector, err := plugin.AppliesTo()

	require.NoError(t, err)
	assert.Equal(t, velero.ResourceSelector{
		IncludedResources: []string{"virtualmachines.kubevirt.io"},
	}, selector)
}

func TestDeletePlugin_Name(t *testing.T) {
	plugin := NewDeletePlugin(newTestLogger())

	name := plugin.Name()

	assert.Equal(t, "kubevirt-vm-delete-plugin", name)
}

func TestDeletePlugin_Execute(t *testing.T) {
	plugin := NewDeletePlugin(newTestLogger())

	item := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name":      "test-vm",
				"namespace": "test-namespace",
			},
		},
	}

	input := &velero.DeleteItemActionExecuteInput{
		Item: item,
	}

	err := plugin.Execute(input)

	require.NoError(t, err)
}
