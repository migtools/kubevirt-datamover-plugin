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

package main

import (
	veleroplugin "github.com/vmware-tanzu/velero/pkg/plugin/framework"
)

func main() {
	veleroplugin.NewServer().
		// Register your backup and restore plugins here
		// Example:
		// RegisterBackupItemAction("kubevirt.io/01-vm-backup-plugin", newVMBackupPlugin).
		// RegisterRestoreItemAction("kubevirt.io/01-vm-restore-plugin", newVMRestorePlugin).
		Serve()
}

// Add plugin constructor functions here as you implement them
// Example:
//
// func newVMBackupPlugin(logger logrus.FieldLogger) (interface{}, error) {
//     return &vm.BackupPlugin{Log: logger}, nil
// }
//
// func newVMRestorePlugin(logger logrus.FieldLogger) (interface{}, error) {
//     return &vm.RestorePlugin{Log: logger}, nil
// }
