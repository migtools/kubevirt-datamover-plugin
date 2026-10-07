# AGENTS.md — AI Agent Instructions for migtools/kubevirt-datamover-plugin

## Project Overview
KubeVirt Datamover Plugin is an OADP Velero plugin for KubeVirt incremental backup and restore. It provides Velero `BackupItemAction` and `RestoreItemAction` implementations that enable incremental qcow2-based VM backups using KubeVirt/libvirt Changed Block Tracking (CBT) instead of CSI snapshots.

- **Primary Language**: Go
- **Module**: `github.com/migtools/kubevirt-datamover-plugin`
- **Default Branch**: `oadp-dev`

## Build Instructions
```bash
# Build all plugin binaries
make all

# Build a specific plugin binary
make build

# Build locally (without container)
make build-local

# Build container image
make container

# Build and push container
make container-push
```

## Test Instructions
```bash
# Run all tests
make test

# Run CI checks (build + test + lint)
make ci

# Run specific tests
go test ./kubevirt-datamover-plugin/... -run TestName

# Vet code
make vet
# or directly:
go vet ./...

# Format code
make fmt
```

## Linting
```bash
# Run golangci-lint
make lint
# or:
make golangci-lint
```

## Code Conventions
- Plugin implementations in `kubevirt-datamover-plugin/` directory
- Follow Velero plugin interface patterns (BackupItemAction, RestoreItemAction)
- KubeVirt and libvirt API types for VM management
- CBT (Changed Block Tracking) for incremental backups
- Standard Go error handling

## Project Structure
```
kubevirt-datamover-plugin/  - Plugin implementation
  backup_item_action.go     - Velero BackupItemAction for KubeVirt VMs
  restore_item_action.go    - Velero RestoreItemAction for KubeVirt VMs
  main.go                   - Plugin registration and entry point
Dockerfile                  - Container image definition
```

## CI/CD
- CI driven by Makefile targets
- Reproduce CI locally:
  ```bash
  make ci  # Runs build, test, and lint
  ```

## Common Tasks

### Modifying backup behavior
1. Edit `kubevirt-datamover-plugin/backup_item_action.go`
2. Update the `Execute()` method
3. Run `make test`

### Modifying restore behavior
1. Edit `kubevirt-datamover-plugin/restore_item_action.go`
2. Update the `Execute()` method
3. Run `make test`

### Adding support for new VM resource types
1. Update `AppliesTo()` in the relevant action
2. Add handling logic in `Execute()`
3. Add tests
4. Run `make ci`
