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
FROM --platform=$BUILDPLATFORM quay.io/konveyor/builder:ubi9-latest AS builder
ARG BUILDPLATFORM
ARG TARGETOS
ARG TARGETARCH
ENV GOPATH=$APP_ROOT
ENV BIN kubevirt-datamover-plugin

WORKDIR $APP_ROOT/src/github.com/migtools/kubevirt-datamover-plugin

COPY go.mod go.sum $APP_ROOT/src/github.com/migtools/kubevirt-datamover-plugin/

RUN go mod download

COPY . $APP_ROOT/src/github.com/migtools/kubevirt-datamover-plugin

RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -installsuffix "static" -o _output/$BIN ./$BIN

FROM registry.access.redhat.com/ubi9-minimal
RUN mkdir /plugins
COPY --from=builder /opt/app-root/src/github.com/migtools/kubevirt-datamover-plugin/_output/$BIN /plugins/
USER 65534:65534
ENTRYPOINT ["/bin/bash", "-c", "cp /plugins/* /target/."]
