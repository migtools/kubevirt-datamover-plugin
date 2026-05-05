package vm

import (
	"context"
	"fmt"

	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/clients"
	"github.com/sirupsen/logrus"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	"github.com/vmware-tanzu/velero/pkg/uploader"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kvcore "kubevirt.io/api/core/v1"
)

type DeletePlugin struct {
	Log logrus.FieldLogger
}

func NewDeletePlugin(log logrus.FieldLogger) *DeletePlugin {
	return &DeletePlugin{Log: log}
}

func (p *DeletePlugin) Name() string {
	return "kubevirt-vm-delete-plugin"
}

func (p *DeletePlugin) AppliesTo() (velero.ResourceSelector, error) {
	return velero.ResourceSelector{
		IncludedResources: []string{
			"virtualmachines.kubevirt.io",
		},
	}, nil
}

func (p *DeletePlugin) Execute(input *velero.DeleteItemActionExecuteInput) error {
	p.Log.Infof("[vm-delete] Executing delete action for %s/%s", input.Item.GetNamespace(), input.Item.GetName())

	vm := &kvcore.VirtualMachine{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(input.Item.UnstructuredContent(), vm); err != nil {
		return fmt.Errorf("unable to convert unstructured item to VirtualMachine: %w", err)
	}

	crClient, err := clients.CRClient()
	if err != nil {
		return fmt.Errorf("failed to get client: %w", err)
	}

	bsl := &velerov1.BackupStorageLocation{}
	if err := crClient.Get(context.Background(), types.NamespacedName{
		Namespace: input.Backup.Namespace,
		Name:      input.Backup.Spec.StorageLocation,
	}, bsl); err != nil {
		return fmt.Errorf("failed to get BSL: %w", err)
	}

	objectStore, _, err := uploader.InitObjectStoreFromBSL(
		context.Background(),
		crClient,
		input.Backup.Namespace,
		bsl,
		nil,
	)
	if err != nil {
		return fmt.Errorf("failed to get object store from BSL: %w", err)
	}

	_ = objectStore // Available for use

	return nil
}
