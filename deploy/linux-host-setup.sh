#!/usr/bin/env bash
# Layer: deploy — Linux host setup script.
# Prepares a bare Linux machine for running sandock host-agent with Firecracker.
# Run as root on Ubuntu 22.04+ or any distro with Linux 5.10+ and KVM support.
#
# What this script does:
#   1. Verifies KVM is available (/dev/kvm).
#   2. Installs required system packages.
#   3. Downloads the Firecracker + jailer binaries.
#   4. Creates directory structure under /var/lib/sandock/.
#   5. Sets up cgroup v2 (unified hierarchy).
#   6. Prints next steps (download kernel image and base rootfs).

set -euo pipefail

FIRECRACKER_VERSION="v1.8.0"
SANDOCK_DIR="/var/lib/sandock"
BIN_DIR="/usr/local/bin"

echo "==> Checking KVM availability..."
if [ ! -c /dev/kvm ]; then
  echo "ERROR: /dev/kvm not found. Enable hardware virtualization in your BIOS/hypervisor."
  exit 1
fi
echo "    KVM found: $(ls -la /dev/kvm)"

echo "==> Installing system dependencies..."
apt-get update -qq
apt-get install -y --no-install-recommends \
  iproute2 \
  iptables \
  util-linux \
  ca-certificates \
  curl \
  jq

echo "==> Downloading Firecracker ${FIRECRACKER_VERSION}..."
ARCH=$(uname -m)
FC_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-${ARCH}.tgz"
curl -fsSL "${FC_URL}" | tar -xz -C /tmp
mv "/tmp/release-${FIRECRACKER_VERSION}-${ARCH}/firecracker-${FIRECRACKER_VERSION}-${ARCH}" "${BIN_DIR}/firecracker"
mv "/tmp/release-${FIRECRACKER_VERSION}-${ARCH}/jailer-${FIRECRACKER_VERSION}-${ARCH}" "${BIN_DIR}/jailer"
chmod +x "${BIN_DIR}/firecracker" "${BIN_DIR}/jailer"
echo "    Firecracker: $(firecracker --version)"

echo "==> Loading host vsock kernel modules (required for exec over AF_VSOCK)..."
modprobe vsock 2>/dev/null || true
modprobe vmw_vsock_virtio_transport 2>/dev/null || true
modprobe vhost_vsock 2>/dev/null || true
if [ ! -e /dev/vhost-vsock ]; then
  echo "WARNING: /dev/vhost-vsock not found. Guest exec over vsock will not work."
  echo "         Try: apt-get install -y linux-modules-extra-\$(uname -r) && modprobe vhost_vsock"
else
  echo "    vsock ready: /dev/vhost-vsock"
fi

echo "==> Creating directory structure..."
mkdir -p \
  "${SANDOCK_DIR}/images" \
  "${SANDOCK_DIR}/sandboxes" \
  "${SANDOCK_DIR}/snapshots" \
  /etc/sandock

echo "==> Configuring cgroup v2 (unified hierarchy)..."
# Verify cgroup v2 is mounted at /sys/fs/cgroup.
if mountpoint -q /sys/fs/cgroup; then
  CGROUP_TYPE=$(stat -fc %T /sys/fs/cgroup)
  if [ "${CGROUP_TYPE}" != "cgroup2fs" ]; then
    echo "WARNING: cgroup v2 not active. Add 'systemd.unified_cgroup_hierarchy=1' to kernel cmdline and reboot."
  else
    echo "    cgroup v2 active."
  fi
fi
# Create the sandboxes sub-cgroup.
mkdir -p /sys/fs/cgroup/sandboxes
echo "+cpu +memory" > /sys/fs/cgroup/sandboxes/cgroup.subtree_control 2>/dev/null || true

echo ""
echo "==> Setup complete! Next steps:"
echo ""
echo "  1. Download a guest kernel image (vmlinux) compatible with Firecracker:"
echo "     https://github.com/firecracker-microvm/firecracker/blob/main/docs/rootfs-and-kernel-setup.md"
echo "     Place it at: ${SANDOCK_DIR}/images/vmlinux-5.10.217"
echo ""
echo "  2. Build or download a base rootfs ext4 image:"
echo "     Place it at: ${SANDOCK_DIR}/images/base-rootfs.ext4"
echo "     The rootfs should include: /bin/sh, the vm-agent binary at /usr/bin/vm-agent,"
echo "     and any language runtimes (python3, node, etc.) your sandboxes need."
echo ""
echo "  3. Copy the example config and customise:"
echo "     cp deploy/config.example.yaml /etc/sandock/config.yaml"
echo ""
echo "  4. Build and run the services:"
echo "     make build"
echo "     # host-agent needs root for loop mounts, cgroups, and Firecracker:"
echo "     sudo ./bin/host-agent --config /etc/sandock/config.yaml &"
echo "     ./bin/api --config /etc/sandock/config.yaml"
