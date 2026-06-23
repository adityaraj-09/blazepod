#!/usr/bin/env bash
# Injects the vm-agent binary and a systemd unit into the base rootfs ext4 image.
# Run on the Linux host after: make build
#
# Usage:
#   sudo bash deploy/inject-vm-agent.sh \
#     bin/vm-agent \
#     /var/lib/sandock/images/base-rootfs.ext4

set -euo pipefail

VM_AGENT_BIN="${1:?vm-agent binary path required}"
ROOTFS_IMAGE="${2:-/var/lib/sandock/images/base-rootfs.ext4}"
MOUNT="/mnt/sandock-rootfs"
# Overlay lower mount used by host-agent — must be unmounted before writing the image.
OVERLAY_LOWER="$(dirname "$ROOTFS_IMAGE")/base-rootfs-lower"

if [ ! -f "$VM_AGENT_BIN" ]; then
  echo "ERROR: vm-agent binary not found at $VM_AGENT_BIN (run: make build)"
  exit 1
fi
if [ ! -f "$ROOTFS_IMAGE" ]; then
  echo "ERROR: rootfs image not found at $ROOTFS_IMAGE"
  exit 1
fi

if mountpoint -q "$OVERLAY_LOWER" 2>/dev/null; then
  echo "==> Unmounting overlay lower mount at $OVERLAY_LOWER (same ext4 image, was read-only)"
  umount "$OVERLAY_LOWER"
fi

if mountpoint -q "$MOUNT" 2>/dev/null; then
  echo "==> Unmounting stale mount at $MOUNT"
  umount "$MOUNT"
fi

echo "==> Mounting $ROOTFS_IMAGE read-write at $MOUNT"
mkdir -p "$MOUNT"
mount -o loop,rw "$ROOTFS_IMAGE" "$MOUNT"

cleanup() {
  umount "$MOUNT" 2>/dev/null || true
  # Restore overlay lower mount for host-agent (read-only is fine for overlay).
  if [ -d "$OVERLAY_LOWER" ] && ! mountpoint -q "$OVERLAY_LOWER" 2>/dev/null; then
    mount -o ro,loop "$ROOTFS_IMAGE" "$OVERLAY_LOWER" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "==> Installing vm-agent binary"
mkdir -p "$MOUNT/usr/bin"
cp -f "$VM_AGENT_BIN" "$MOUNT/usr/bin/vm-agent"
chmod 755 "$MOUNT/usr/bin/vm-agent"

echo "==> Installing systemd unit"
mkdir -p "$MOUNT/etc/systemd/system"
cat > "$MOUNT/etc/systemd/system/sandock-vm-agent.service" <<'UNIT'
[Unit]
Description=Sandock in-VM exec agent (AF_VSOCK :8888)
DefaultDependencies=no
After=local-fs.target
Before=multi-user.target

[Service]
Type=simple
ExecStart=/usr/bin/vm-agent
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
UNIT

echo "==> Enabling sandock-vm-agent.service"
mkdir -p "$MOUNT/etc/systemd/system/multi-user.target.wants"
ln -sf /etc/systemd/system/sandock-vm-agent.service \
  "$MOUNT/etc/systemd/system/multi-user.target.wants/sandock-vm-agent.service"

echo "==> Syncing and unmounting"
sync
umount "$MOUNT"
trap - EXIT

if [ -d "$OVERLAY_LOWER" ]; then
  echo "==> Remounting overlay lower at $OVERLAY_LOWER"
  mount -o ro,loop "$ROOTFS_IMAGE" "$OVERLAY_LOWER"
fi

echo "==> Done. vm-agent installed at /usr/bin/vm-agent inside the rootfs."
