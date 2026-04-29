package vm

import (
	"github.com/sirupsen/logrus"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
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
	return nil
}
