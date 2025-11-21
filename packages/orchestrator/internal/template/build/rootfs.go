package build

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/dustin/go-humanize"
	containerregistry "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/ext4"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/oci"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/writer"
	artifactsregistry "github.com/e2b-dev/infra/packages/shared/pkg/artifacts-registry"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

const (
	ToMBShift = 20
	// Max size of the rootfs file in MB.
	maxRootfsSize = 15000 << ToMBShift

	rootfsBuildFileName = "rootfs.ext4.build"
	rootfsProvisionLink = "rootfs.ext4.build.provision"

	// provisionScriptFileName is a path where the provision script stores it's exit code.
	provisionScriptResultPath = "/provision.result"
	logExternalPrefix         = "[external] "

	busyBoxBinaryPath = "/bin/busybox"
	busyBoxInitPath   = "usr/bin/init"
	systemdInitPath   = "/sbin/init"
)

type Rootfs struct {
	template         *TemplateConfig
	artifactRegistry artifactsregistry.ArtifactsRegistry
}

type MultiWriter struct {
	writers []io.Writer
}

func (mw *MultiWriter) Write(p []byte) (int, error) {
	for _, writer := range mw.writers {
		_, err := writer.Write(p)
		if err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

func NewRootfs(artifactRegistry artifactsregistry.ArtifactsRegistry, template *TemplateConfig) *Rootfs {
	return &Rootfs{
		template:         template,
		artifactRegistry: artifactRegistry,
	}
}

func (r *Rootfs) createExt4Filesystem(ctx context.Context, tracer trace.Tracer, postProcessor *writer.PostProcessor, rootfsPath string) (c containerregistry.Config, e error) {
	childCtx, childSpan := tracer.Start(ctx, "create-ext4-file")
	defer childSpan.End()

	defer func() {
		if e != nil {
			telemetry.ReportCriticalError(childCtx, "failed to create ext4 filesystem", e)
		}
	}()

	postProcessor.WriteMsg("Requesting Docker Image")

	img, err := oci.GetImage(childCtx, tracer, r.artifactRegistry, r.template.TemplateId, r.template.BuildId)
	if err != nil {
		return containerregistry.Config{}, fmt.Errorf("error requesting docker image: %w", err)
	}

	imageSize, err := oci.GetImageSize(img)
	if err != nil {
		return containerregistry.Config{}, fmt.Errorf("error getting image size: %w", err)
	}
	postProcessor.WriteMsg(fmt.Sprintf("Docker image size: %s", humanize.Bytes(uint64(imageSize))))

	postProcessor.WriteMsg("Setting up system files")
	layers, err := additionalOCILayers(childCtx, r.template)
	if err != nil {
		return containerregistry.Config{}, fmt.Errorf("error populating filesystem: %w", err)
	}
	img, err = mutate.AppendLayers(img, layers...)
	if err != nil {
		return containerregistry.Config{}, fmt.Errorf("error appending layers: %w", err)
	}
	telemetry.ReportEvent(childCtx, "set up filesystem")

	postProcessor.WriteMsg("Creating file system and pulling Docker image")
	ext4Size, err := oci.ToExt4(ctx, tracer, postProcessor, img, rootfsPath, maxRootfsSize, r.template.RootfsBlockSize())
	if err != nil {
		return containerregistry.Config{}, fmt.Errorf("error creating ext4 filesystem: %w", err)
	}
	r.template.rootfsSize = ext4Size
	telemetry.ReportEvent(childCtx, "created rootfs ext4 file")

	postProcessor.WriteMsg("Filesystem cleanup")
	// Make rootfs writable, be default it's readonly
	err = ext4.MakeWritable(ctx, tracer, rootfsPath)
	if err != nil {
		return containerregistry.Config{}, fmt.Errorf("error making rootfs file writable: %w", err)
	}

	// Resize rootfs
	rootfsFreeSpace, err := ext4.GetFreeSpace(ctx, tracer, rootfsPath, r.template.RootfsBlockSize())
	if err != nil {
		return containerregistry.Config{}, fmt.Errorf("error getting free space: %w", err)
	}
	// We need to remove the remaining free space from the ext4 file size
	// This is a residual space that could not be shrunk when creating the filesystem,
	// but is still available for use
	diskAdd := r.template.DiskSizeMB<<ToMBShift - rootfsFreeSpace
	zap.L().Debug("adding disk size diff to rootfs",
		zap.Int64("size_current", ext4Size),
		zap.Int64("size_add", diskAdd),
		zap.Int64("size_free", rootfsFreeSpace),
	)
	if diskAdd > 0 {
		rootfsFinalSize, err := ext4.Enlarge(ctx, tracer, rootfsPath, diskAdd)
		if err != nil {
			return containerregistry.Config{}, fmt.Errorf("error enlarging rootfs: %w", err)
		}
		r.template.rootfsSize = rootfsFinalSize
	}

	// Check the rootfs filesystem corruption
	ext4Check, err := ext4.CheckIntegrity(rootfsPath, true)
	zap.L().Debug("filesystem ext4 integrity",
		zap.String("result", ext4Check),
		zap.Error(err),
	)
	if err != nil {
		return containerregistry.Config{}, fmt.Errorf("error checking ext4 filesystem integrity: %w", err)
	}

	config, err := img.ConfigFile()
	if err != nil {
		return containerregistry.Config{}, fmt.Errorf("error getting image config file: %w", err)
	}

	return config.Config, nil
}

func additionalOCILayers(
	ctx context.Context,
	config *TemplateConfig,
) ([]containerregistry.Layer, error) {
	var scriptDef bytes.Buffer
	err := ProvisionScriptTemplate.Execute(&scriptDef, struct {
		ResultPath string
	}{
		ResultPath: provisionScriptResultPath,
	})
	if err != nil {
		return nil, fmt.Errorf("error executing provision script: %w", err)
	}
	telemetry.ReportEvent(ctx, "executed provision script env")

	memoryLimit := int(math.Min(float64(config.MemoryMB)/2, 512))
	envdService := fmt.Sprintf(`[Unit]
Description=Env Daemon Service
After=multi-user.target

[Service]
Type=simple
Restart=always
User=root
Group=root
Environment=GOTRACEBACK=all
LimitCORE=infinity
ExecStart=/bin/bash -l -c "/usr/bin/envd"
OOMPolicy=continue
OOMScoreAdjust=-1000
Environment="GOMEMLIMIT=%dMiB"

[Install]
WantedBy=multi-user.target
`, memoryLimit)

	autologinService := `[Service]
ExecStart=
ExecStart=-/sbin/agetty --noissue --autologin root %I 115200,38400,9600 vt102
`

	hostname := "e2b.local"

	hosts := fmt.Sprintf(`127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::	ip6-localnet
ff00::	ip6-mcastprefix
ff02::1	ip6-allnodes
ff02::2	ip6-allrouters
127.0.1.1	%s
`, hostname)

	e2bFile := fmt.Sprintf(`ENV_ID=%s
BUILD_ID=%s
`, config.TemplateId, config.BuildId)

	envdFileData, err := os.ReadFile(storage.HostEnvdPath)
	if err != nil {
		return nil, fmt.Errorf("error reading envd file: %w", err)
	}

	busyBox, err := os.ReadFile(busyBoxBinaryPath)
	if err != nil {
		return nil, fmt.Errorf("error reading busybox binary: %w", err)
	}

	filesLayer, err := LayerFile(
		map[string]layerFile{
			// Setup system
			"etc/hostname":    {[]byte(hostname), 0o644},
			"etc/hosts":       {[]byte(hosts), 0o644},
			"etc/resolv.conf": {[]byte("nameserver 169.254.169.253\nnameserver 8.8.8.8\noptions timeout:2 attempts:2 ndots:2\n"), 0o644},

			".e2b":                            {[]byte(e2bFile), 0o644},
			storage.GuestEnvdPath:             {envdFileData, 0o777},
			"etc/systemd/system/envd.service": {[]byte(envdService), 0o644},
			"etc/systemd/system/serial-getty@ttyS0.service.d/autologin.conf": {[]byte(autologinService), 0o644},

			// Provision script
			"usr/local/bin/provision.sh": {scriptDef.Bytes(), 0o777},
			// Setup init system
			"usr/bin/busybox": {busyBox, 0o755},
			// Set to bin/init so it's not in conflict with systemd
			// Any rewrite of the init file when booted from it will corrupt the filesystem
			busyBoxInitPath: {busyBox, 0o755},
			"etc/init.d/rcS": {[]byte(`#!/usr/bin/busybox ash
echo "Mounting essential filesystems"
# Ensure necessary mount points exist
mkdir -p /proc /sys /dev /tmp /run

# Mount essential filesystems
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mount -t tmpfs tmpfs /tmp
mount -t tmpfs tmpfs /run

echo "System Init"`), 0o777},
			"etc/inittab": {[]byte(fmt.Sprintf(`# Run system init
::sysinit:/etc/init.d/rcS

# Run the provision script, prefix the output with a log prefix
::wait:/bin/sh -c '/usr/local/bin/provision.sh 2>&1 | sed "s/^/%s/"'

# Reboot the system after the script
# Running the poweroff or halt commands inside a Linux guest will bring it down but Firecracker process remains unaware of the guest shutdown so it lives on.
# Running the reboot command in a Linux guest will gracefully bring down the guest system and also bring a graceful end to the Firecracker process.
::once:/usr/bin/busybox reboot

# Clean shutdown of filesystems and swap
::shutdown:/usr/bin/busybox swapoff -a
::shutdown:/usr/bin/busybox umount -a -r -v
`, logExternalPrefix)), 0o777},

			// EFS/NFS mount helper: read MMDS and mount /home/user
			"usr/local/bin/efs-mount.sh": {[]byte(`#!/bin/bash
set -euo pipefail

log() { echo "[efs-mount] $1"; }
warn() { echo "[efs-mount][warn] $1"; }
err() { echo "[efs-mount][error] $1"; }

# MMDS address
MMDS_URL="http://169.254.169.254/"

USER_ID=""
EFS_HOST=""
EFS_ROOT=""

if command -v curl >/dev/null 2>&1; then
  # MMDS v2 requires a token
  TOKEN=$(curl -fsS -m 2 -X PUT -H "X-metadata-token-ttl-seconds: 30" "$MMDS_URL/latest/api/token" 2>/dev/null || true)
  if [ -z "$TOKEN" ]; then
    warn "MMDS token not available; skipping"
    exit 0
  fi
  if METADATA_JSON=$(curl -fsS -m 2 -H "X-metadata-token: $TOKEN" -H "Accept: application/json" "$MMDS_URL" 2>/dev/null); then
    # Persist raw metadata for debugging/verification
    mkdir -p "/home/user"
    printf "%s\n" "$METADATA_JSON" > "/home/user/metadata.json" || true

    if command -v jq >/dev/null 2>&1; then
      USER_ID=$(echo "$METADATA_JSON" | jq -r '.userID // empty')
      EFS_HOST=$(echo "$METADATA_JSON" | jq -r '.efsHost // empty')
      EFS_ROOT=$(echo "$METADATA_JSON" | jq -r '.efsRoot // ""')
    else
      # crude parsing fallback
      USER_ID=$(echo "$METADATA_JSON" | sed -n 's/.*"userID"\s*:\s*"\([^"]*\)".*/\1/p')
      EFS_HOST=$(echo "$METADATA_JSON" | sed -n 's/.*"efsHost"\s*:\s*"\([^"]*\)".*/\1/p')
      EFS_ROOT=$(echo "$METADATA_JSON" | sed -n 's/.*"efsRoot"\s*:\s*"\([^"]*\)".*/\1/p')
    fi
  else
    warn "MMDS not reachable; skipping EFS mount"
    exit 0
  fi
else
  warn "curl not found; skipping EFS mount"
  exit 0
fi

if [ -z "$USER_ID" ]; then
  warn "userID missing in MMDS; skipping"
  exit 0
fi

if [ -z "$EFS_HOST" ]; then
  warn "efsHost missing; skipping"
  exit 0
fi

TARGET="/home/user"
SRC_PATH="$EFS_HOST:${EFS_ROOT}/${USER_ID}"

mkdir -p "$TARGET"

# Prefer NFSv4 mount which works for EFS and standard NFS
MOUNT_OPTS="nfsvers=4.1,noresvport"

# Ensure remote subdirectory exists by mounting EFS root temporarily
ROOT="${EFS_ROOT:-/}"
TMP="/mnt/efs"
SRC_ROOT="$EFS_HOST:${ROOT}"
mkdir -p "$TMP"
if ! mountpoint -q "$TMP"; then
  if ! mount -t nfs4 -o "$MOUNT_OPTS" "$SRC_ROOT" "$TMP"; then
    warn "failed to mount EFS root via nfs4; trying nfs"
    mount -t nfs -o "$MOUNT_OPTS" "$SRC_ROOT" "$TMP" || true
  fi
fi
if mountpoint -q "$TMP"; then
  mkdir -p "$TMP/${USER_ID}" || true
  umount "$TMP" || true
fi

if mountpoint -q "$TARGET"; then
  log "target already mounted"
  exit 0
fi

if mount -t nfs4 -o "$MOUNT_OPTS" "$SRC_PATH" "$TARGET"; then
  log "mounted $SRC_PATH to $TARGET"
else
  warn "failed NFS4 mount, trying legacy nfs"
  if mount -t nfs -o "$MOUNT_OPTS" "$SRC_PATH" "$TARGET"; then
    log "mounted (nfs) $SRC_PATH to $TARGET"
  else
    err "mount failed for $SRC_PATH"
    exit 0
  fi
fi

# Best-effort ownership to 'user'
if id -u user >/dev/null 2>&1; then
  chown -R user:user "$TARGET" || true
fi
`), 0o755},

			"etc/systemd/system/efs-mount.service": {[]byte(`[Unit]
Description=EFS/NFS mount for /home/user
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/efs-mount.sh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`), 0o644},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error creating layer from files: %w", err)
	}

	symlinkLayer, err := LayerSymlink(
		map[string]string{
			// Enable envd service autostart
			"etc/systemd/system/multi-user.target.wants/envd.service": "etc/systemd/system/envd.service",
			// Enable chrony service autostart
			"etc/systemd/system/multi-user.target.wants/chrony.service": "etc/systemd/system/chrony.service",
			// Enable EFS/NFS mount service at boot
			"etc/systemd/system/multi-user.target.wants/efs-mount.service": "etc/systemd/system/efs-mount.service",
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error creating layer from symlinks: %w", err)
	}

	return []containerregistry.Layer{
		filesLayer,
		symlinkLayer,
	}, nil
}
