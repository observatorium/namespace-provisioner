FROM --platform=$BUILDPLATFORM golang:1.21-alpine3.19 AS build

COPY . /src
WORKDIR /src
RUN go mod vendor
ARG TARGETOS TARGETARCH
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build .

FROM scratch as runner
COPY --from=build /src/namespace-provisioner /usr/bin/namespace-provisioner

LABEL vendor="Observatorium" \
    name="observatorium/namespace-provisioner" \
    description="Create temporary namespaces in a Kubernetes cluster" \
    io.k8s.display-name="observatorium/namespace-provisioner" \
    io.k8s.description="Create temporary namespaces in a Kubernetes cluster" \
    maintainer="Observatorium <team-monitoring@redhat.com>" \
    org.label-schema.description="Create temporary namespaces in a Kubernetes cluster" \
    org.label-schema.name="observatorium/namespace-provisioner" \
    org.label-schema.schema-version="1.0" \
    org.label-schema.vendor="Observatorium"

ENTRYPOINT ["/usr/bin/namespace-provisioner"]
