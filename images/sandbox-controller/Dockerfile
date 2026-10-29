# Build context: services/   (docker build -f images/sandbox-controller/Dockerfile -t pdd/sandbox-controller services)
# The image is the runtime container of ADR 0004: the controller, containerd with the runc v2 shim,
# runsc and the CNI plugins. Run it privileged with a private cgroup namespace, see
# cmd/sandbox-controller/README.md.
FROM golang:1.24 AS build
ENV CGO_ENABLED=0 GOOS=linux
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# containerd and the CNI plugins are built at the versions go.mod pins for the client library, so
# that the daemon and the client cannot drift apart.
RUN go build -trimpath -ldflags="-s -w" -o /out/sandbox-controller ./cmd/sandbox-controller \
 && containerd="$(go list -m -f '{{.Version}}' github.com/containerd/containerd/v2)" \
 && plugins="$(go list -m -f '{{.Version}}' github.com/containernetworking/plugins)" \
 && cd /tmp \
 && GOBIN=/out/bin go install -trimpath -ldflags="-s -w" \
      "github.com/containerd/containerd/v2/cmd/containerd@${containerd}" \
      "github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2@${containerd}" \
 && GOBIN=/out/cni go install -trimpath -ldflags="-s -w" \
      "github.com/containernetworking/plugins/plugins/main/ptp@${plugins}" \
      "github.com/containernetworking/plugins/plugins/main/loopback@${plugins}" \
      "github.com/containernetworking/plugins/plugins/ipam/host-local@${plugins}" \
 && mkdir -p /out/data

FROM ubuntu:24.04
# runsc comes from the official gVisor apt repository.
# The package puts the binary at /usr/bin/runsc and the helpers it needs next to it in /usr/bin/gvisor-bin.
# iptables (the nft backend of Ubuntu) fences the sandboxes in, see runtime/containerd/firewall.go.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg iptables \
 && curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" > /etc/apt/sources.list.d/gvisor.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends runsc \
 && apt-get purge -y --auto-remove curl gnupg \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/data /data
COPY --from=build /out/bin/ /usr/local/bin/
COPY --from=build /out/sandbox-controller /usr/local/bin/sandbox-controller
COPY --from=build /out/cni/ /opt/cni/bin/
COPY images/sandbox-controller/containerd.toml /etc/containerd/config.toml
COPY --chmod=0755 images/sandbox-controller/entrypoint.sh /usr/local/bin/entrypoint.sh
# /data: database. /var/lib/containerd: images and snapshots; it must be a volume because the
# overlay snapshotter cannot run on the container's own overlay file system. /var/lib/pdd-sandbox:
# workspaces, dependency caches and logs.
VOLUME ["/data", "/var/lib/containerd", "/var/lib/pdd-sandbox"]
EXPOSE 8080 3128
# Runs as root: containerd, the shim and runsc need to create namespaces, mount and write cgroups,
# and the controller writes workspaces that the sandbox user (uid 1000) must be able to use. The
# sandboxes it starts are unprivileged and run under runsc.
# The deployment mounts the merged configuration here (see cmd/sandbox-controller/README.md).
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["--config", "/etc/pdd/sandbox-controller/values.yaml"]
