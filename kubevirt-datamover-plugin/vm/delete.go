package vm

import (
	"context"
	"fmt"
	"slices"

	"github.com/bombsimon/logrusr/v4"
	//"github.com/go-logr/logr"
	"github.com/migtools/kubevirt-datamover-controller/pkg/uploader"
	"github.com/migtools/kubevirt-datamover-plugin/kubevirt-datamover-plugin/clients"
	"github.com/sirupsen/logrus"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kvcore "kubevirt.io/api/core/v1"
)

type DeletePlugin struct {
	Log                logrus.FieldLogger
	ObjectStoreFactory func(c *uploader.UploaderConfig) (velero.ObjectStore, error)
}

func NewDeletePlugin(log logrus.FieldLogger, factory func(c *uploader.UploaderConfig) (velero.ObjectStore, error)) *DeletePlugin {
	return &DeletePlugin{Log: log, ObjectStoreFactory: factory}
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
	p.Log.Info("[vm-delete] Executing delete action for VirtualMachine")

	vm := &kvcore.VirtualMachine{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(input.Item.UnstructuredContent(), vm); err != nil {
		return fmt.Errorf("unable to convert unstructured item to VirtualMachine: %w", err)
	}
	p.Log.Infof("[vm-delete] delete action for %s/%s", vm.Namespace, vm.Name)

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

	objectStore, cfg, err := uploader.InitObjectStoreFromBSL(
		context.Background(),
		crClient,
		input.Backup.Namespace,
		bsl,
		p.ObjectStoreFactory,
	)
	if err != nil {
		return fmt.Errorf("failed to get object store from BSL: %w", err)
	}

	// read per-backup-per-vm manifest
	vmBackupManifest, found, err := uploader.GetVMBackupManifest(objectStore, vm.Namespace, vm.Name, input.Backup.Name, cfg.Bucket, logrusr.New(p.Log))
	if err != nil {
		return fmt.Errorf("failed to get VM backup manifest BSL: %w", err)
	}
	if !found {
		return nil
	}
	// get checkpoint list
	checkpointNames := vmBackupManifest.CheckpointChain
	// read per-vm manifest
	vmIndex, found, err := uploader.GetVMIndex(objectStore, vm.Namespace, vm.Name, cfg.Bucket, logrusr.New(p.Log))
	if err != nil {
		return fmt.Errorf("failed to get VM index from BSL: %w", err)
	}
	// iterate over checkpoints
	if found {
		n := 0
		for _, checkpoint := range vmIndex.Checkpoints {
			//   for each referenced checkpoint, remove this backup from referencedBy
			if slices.Contains(checkpointNames, checkpoint.ID) {
				slices.DeleteFunc(checkpoint.ReferencedBy, func(e string) bool {
					return e == input.Backup.Name
				})
			}
			//   if referencedBy empty
			if len(checkpoint.ReferencedBy) == 0 {
				//      remove qcow2 files and vmb/vmbt and checkpoint dir
				for _, qcowFile := range checkpoint.Files {
					if err := uploader.DeleteQCOW(objectStore, vm.Namespace, vm.Name, checkpoint.ID, qcowFile.Filename, cfg.Bucket); err != nil {
						return fmt.Errorf("failed to delete qcow file from BSL: %w", err)
					}
				}
			} else {
				//      keep checkpoint entry
				vmIndex.Checkpoints[n] = checkpoint
				n++
			}
		}
		vmIndex.Checkpoints = vmIndex.Checkpoints[:n]
		// write per-vm manifest (delete if no checkpoints left)
		if len(vmIndex.Checkpoints) > 0 {
			err = uploader.PutVMIndex(objectStore, vm.Namespace, vm.Name, cfg.Bucket, vmIndex)
			if err != nil {
				return fmt.Errorf("failed to write VM index to BSL: %w", err)
			}
		} else {
			err = uploader.DeleteVMIndex(objectStore, vm.Namespace, vm.Name, cfg.Bucket)
			if err != nil {
				return fmt.Errorf("failed to write VM index to BSL: %w", err)
			}
		}
	}
	// read per-backup manifest
	backupManifest, found, err := uploader.GetBackupManifest(objectStore, input.Backup.Name, cfg.Bucket, logrusr.New(p.Log))
	if err != nil {
		return fmt.Errorf("failed to get backup manifest from BSL: %w", err)
	}
	if found {
		// remove vm
		slices.DeleteFunc(backupManifest.VMs, func(e uploader.VMBackupReference) bool {
			return e.Name == vm.Name
		})
		// write per-backup manifest (delete if no VMs left)
		if len(backupManifest.VMs) == 0 {
			err = uploader.DeleteBackupManifest(objectStore, input.Backup.Name, cfg.Bucket)
			if err != nil {
				return fmt.Errorf("failed to delete backup manifest from BSL: %w", err)
			}
		} else {
			err = uploader.PutBackupManifest(objectStore, input.Backup.Name, cfg.Bucket, backupManifest)
			if err != nil {
				return fmt.Errorf("failed to update backup manifest in BSL: %w", err)
			}
		}
	}
	// delete per-backup-per-vm manifest
	err = uploader.DeleteVMBackupManifest(objectStore, vm.Namespace, vm.Name, input.Backup.Name, cfg.Bucket)
	if err != nil {
		return fmt.Errorf("failed to delete VM backup manifest from BSL: %w", err)
	}

	return nil
}
