# Copyright 2026 Red Hat Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

BINS = $(wildcard kubevirt-datamover-*)

REPO ?= github.com/migtools/kubevirt-datamover-plugin

BUILD_IMAGE ?= golang:1.22

IMAGE ?= quay.io/konveyor/kubevirt-datamover-plugin

ARCH ?= amd64

OC_CLI ?= $(shell which oc)

CLUSTER_OS = $(shell $(OC_CLI) get node -o jsonpath='{.items[0].status.nodeInfo.operatingSystem}' 2> /dev/null)
CLUSTER_ARCH = $(shell $(OC_CLI) get node -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2> /dev/null)

# CONTAINER_TOOL defines the container tool to be used for building images.
# By default, this Makefile uses docker, as the target commands have been tested primarily with it.
# However, if docker is not available, the Makefile will attempt to use podman if it's installed.
# You may also set CONTAINER_TOOL directly as an environment variable to specify a different tool.
# If neither docker nor podman is found, or if the specified tool is unavailable, the Makefile will exit with an error.
CONTAINER_TOOL ?= $(shell \
  if command -v docker >/dev/null 2>&1; then echo docker; \
  elif command -v podman >/dev/null 2>&1; then echo podman; \
  else echo ""; \
  fi \
)
ifeq ($(shell command -v $(CONTAINER_TOOL) >/dev/null 2>&1 && echo found),)
  $(error The selected container tool '$(CONTAINER_TOOL)' is not available on this system. Please install it or choose a different tool.)
endif
$(info Using Container Tool: $(CONTAINER_TOOL))

all: $(addprefix build-, $(BINS))

build-%:
	$(MAKE) --no-print-directory BIN=$* build

build: _output/$(BIN)

_output/$(BIN): $(BIN)/*.go
	mkdir -p .go/src/$(REPO) .go/pkg .go/.cache .go/std/$(ARCH) _output
	cp -rp * .go/src/$(REPO)
	$(CONTAINER_TOOL) run \
				 --rm \
				 -v $$(pwd)/.go/pkg:/go/pkg:z \
				 -v $$(pwd)/.go/src:/go/src:z \
				 -v $$(pwd)/.go/std:/go/std:z \
				 -v $$(pwd)/.go/.cache:/go/.cache:z \
				 -v $$(pwd)/_output:/go/src/$(REPO)/_output:z \
				 -v $$(pwd)/.go/std/$(ARCH):/usr/local/go/pkg/linux_$(ARCH)_static:z \
				 -w /go/src/$(REPO) \
				 $(BUILD_IMAGE) \
				 go build -installsuffix "static" -o _output/$(BIN) ./$(BIN)

# Build locally without container
build-local:
	mkdir -p _output
	go build -o _output/kubevirt-datamover-plugin ./kubevirt-datamover-plugin

CONTAINER_BUILD_ARGS ?= --platform=linux/amd64
ifneq ($(CLUSTER_OS),)
	CONTAINER_BUILD_ARGS = --platform=$(CLUSTER_OS)/$(CLUSTER_ARCH)
endif

container:
	$(CONTAINER_TOOL) build -t $(IMAGE) . $(CONTAINER_BUILD_ARGS)

container-push:
	$(CONTAINER_TOOL) push $(IMAGE)

test:
	go test ./kubevirt-datamover-plugin/...

ci: all test

clean:
	rm -rf .go _output

.PHONY: all build build-local container container-push test ci clean
