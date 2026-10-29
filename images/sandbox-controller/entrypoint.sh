#!/usr/bin/env bash
# Entrypoint of the sandbox-controller runtime container: starts containerd, waits for its socket,
# runs the controller with the given arguments and exits as soon as either of them exits, with the
# status of the one that ended first, so that Docker's restart policy applies. SIGTERM and SIGINT
# are passed on to both.
set -euo pipefail

readonly SOCKET=/run/containerd/containerd.sock
readonly CONFIG=/etc/containerd/config.toml
readonly SOCKET_TIMEOUT=30

log() { printf 'entrypoint: %s\n' "$*" >&2; }

# containerd's state and the sandbox network state (namespaces, address leases) must not outlive
# a start of this container, or a restart would meet leftovers of dead sandboxes.
for dir in /run/containerd /run/pdd-sandbox; do
  mkdir -p "${dir}"
  if ! mountpoint -q "${dir}" && ! mount -t tmpfs -o mode=0755 tmpfs "${dir}"; then
    log "cannot mount a tmpfs on ${dir}; run the container with --privileged"
    exit 1
  fi
done

# cgroup v2: runc and runsc create the cgroups of the sandboxes below this container's cgroup, which
# only works for controllers enabled in its subtree_control, and that is refused while processes
# live in the cgroup itself. Move them into a child first.
if [[ -f /sys/fs/cgroup/cgroup.controllers ]]; then
  mkdir -p /sys/fs/cgroup/init
  xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init/cgroup.procs || true
  sed -e 's/ / +/g' -e 's/^/+/' < /sys/fs/cgroup/cgroup.controllers > /sys/fs/cgroup/cgroup.subtree_control || {
    log "cannot enable cgroup controllers; run the container with --privileged --cgroupns=private"
    exit 1
  }
fi

containerd_pid=
controller_pid=
terminate() {
  kill -TERM "${controller_pid}" "${containerd_pid}" 2> /dev/null || true
}
trap terminate TERM INT

containerd --config "${CONFIG}" &
containerd_pid=$!

for ((i = 0; i < SOCKET_TIMEOUT * 10; i++)); do
  [[ -S ${SOCKET} ]] && break
  if ! kill -0 "${containerd_pid}" 2> /dev/null; then
    wait "${containerd_pid}" || status=$?
    log "containerd exited with status ${status:-0} before it created ${SOCKET}"
    exit "${status:-1}"
  fi
  sleep 0.1
done
if [[ ! -S ${SOCKET} ]]; then
  log "containerd did not create ${SOCKET} within ${SOCKET_TIMEOUT}s"
  kill -TERM "${containerd_pid}" 2> /dev/null || true
  exit 1
fi

sandbox-controller "$@" &
controller_pid=$!

# A signal interrupts wait before any process has ended; the trap has passed it on, so the loop
# goes on until the first one ends.
ended=
while [[ -z ${ended:-} ]]; do
  status=0
  wait -n -p ended "${controller_pid}" "${containerd_pid}" || status=$?
done
terminate
wait || true
exit "${status}"
