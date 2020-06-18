FROM alpine:3.12

COPY ./namespace-provisioner /usr/bin/namespace-provisioner

ENTRYPOINT ["/usr/bin/namespace-provisioner"]
