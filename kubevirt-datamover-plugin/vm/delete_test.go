package vm

import (
	"testing"

	"github.com/migtools/kubevirt-datamover-controller/pkg/uploader"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/clients"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/builder"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	//"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDeletePlugin_AppliesTo(t *testing.T) {
	plugin := NewDeletePlugin(newTestLogger(), nil)

	selector, err := plugin.AppliesTo()

	require.NoError(t, err)
	assert.Equal(t, velero.ResourceSelector{
		IncludedResources: []string{"virtualmachines.kubevirt.io"},
	}, selector)
}

func TestDeletePlugin_Name(t *testing.T) {
	plugin := NewDeletePlugin(newTestLogger(), nil)

	name := plugin.Name()

	assert.Equal(t, "kubevirt-vm-delete-plugin", name)
}

func TestDeletePlugin_Execute(t *testing.T) {
	//testEnv := &envtest.Environment{}
	//cfg, err := testEnv.Start()
	//require.NoError(t, err)
	//defer testEnv.Stop()
	// initialize clients using envtest config
	//require.NoError(t, err)
	fakeClientBuilder := fake.NewClientBuilder()
	scheme := runtime.NewScheme()
        corev1.AddToScheme(scheme)
        velerov1.AddToScheme(scheme)
	mockStore := uploader.NewMockObjectStore("test-bucket", "velero-kubevirt-datamover")

	//vmIndex := uploader.VMIndex{
	//	VMName:    vmName,
	//	Namespace: vmNamespace,
	//	Checkpoints: []uploader.CheckpointEntry{
	//		{
	//			ID:     existingCheckpointID,
	//			Type:   "full",
	//			Parent: "",
	//			Files: []uploader.CheckpointFile{
	//				{
	//					Filename:   "vmb-prev-disk1.qcow2",
	//					ObjectPath: "checkpoints/" + vmNamespace + "/" + vmName + "/" + existingCheckpointID + "/vmb-prev-disk1.qcow2",
	//				},
	//			},
	//		},
	//	},
	//}
	//indexData, _ := json.Marshal(vmIndex)
	//indexPath := "checkpoints/" + vmNamespace + "/" + vmName + "/index.json"
	//_ = mockStore.PutObject("test-bucket", indexPath, bytes.NewReader(indexData))

	// Also store the qcow2 file so chain validation succeeds
	//qcow2Path := "checkpoints/" + vmNamespace + "/" + vmName + "/" + existingCheckpointID + "/vmb-prev-disk1.qcow2"
	//_ = mockStore.PutObject("test-bucket", qcow2Path, bytes.NewReader([]byte("fake-qcow2-data")))
	testObjectStoreFactory := func(_ *uploader.UploaderConfig) (velero.ObjectStore, error) {
		return mockStore, nil
	}
	fakeClientBuilder = fakeClientBuilder.WithScheme(scheme)
	item := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name":      "test-vm",
				"namespace": "test-namespace",
			},
			"apiVersion": "kubevirt.io/v1",
			"kind": "VirtualMachine",
		},
	}
	bsl := builder.ForBackupStorageLocation("velero", "bsl-1").Provider("aws").Bucket("bucket").Prefix("/prefix/").Credential(
		builder.ForSecretKeySelector("bsl-secret", "cloud").Result(),
	).Result()
	bsl.Spec.Config = map[string]string{"region": "myregion"}
	s3SecretBytes := []byte(`[default]
aws_access_key_id=foo
aws_secret_access_key=bar
`)
	credSecret := builder.ForSecret("velero", "bsl-secret").Data(map[string][]byte{
		"cloud": s3SecretBytes,
	}).Result()

	var objs []runtime.Object
	objs = append(objs, item)
	objs = append(objs, bsl)
	objs = append(objs, credSecret)

	fakeClient := fakeClientBuilder.WithRuntimeObjects(objs...).Build()
	clients.SetCRClient(fakeClient)

	plugin := NewDeletePlugin(newTestLogger(), testObjectStoreFactory)
	backup := builder.ForBackup("velero", "backup-1").StorageLocation("bsl-1").Result()

	input := &velero.DeleteItemActionExecuteInput{
		Item: item,
		Backup: backup,
	}

	err := plugin.Execute(input)

	require.NoError(t, err)
}
