FROM alpine:3.12

COPY ./namespace-provisioner /app/namespace-provisioner
COPY ./role.yaml             /app/config/role.yaml

ENTRYPOINT ["/app/namespace-provisioner"]
