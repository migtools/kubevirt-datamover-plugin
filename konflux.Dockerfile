FROM brew.registry.redhat.io/rh-osbs/openshift-golang-builder:rhel_9_golang_1.25 AS builder

COPY . /workspace
WORKDIR /workspace/
ENV GOEXPERIMENT strictfipsruntime
RUN CGO_ENABLED=1 GOOS=linux go build -v -mod=mod -tags strictfipsruntime \
    -o /workspace/bin/kubevirt-datamover-plugin ./kubevirt-datamover-plugin

FROM registry.redhat.io/ubi9/ubi:latest
RUN dnf -y install openssl && dnf -y reinstall tzdata && dnf clean all
RUN mkdir /plugins
COPY --from=builder /workspace/bin/kubevirt-datamover-plugin /plugins/
COPY LICENSE /licenses/
USER 65534:65534
ENTRYPOINT ["/bin/bash", "-c", "cp /plugins/* /target/."]

LABEL description="kubevirt-datamover-plugin"
LABEL io.k8s.description="kubevirt-datamover-plugin"
LABEL io.k8s.display-name="kubevirt-datamover-plugin"
LABEL io.openshift.tags="migration"
LABEL summary="kubevirt-datamover-plugin"
