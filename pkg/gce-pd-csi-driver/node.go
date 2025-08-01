/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gceGCEDriver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	"sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/common"
	"sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/deviceutils"
	metadataservice "sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/gce-cloud-provider/metadata"
	"sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/metrics"
	mountmanager "sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/mount-manager"
	"sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/resizefs"
)

type GCENodeServer struct {
	Driver                   *GCEDriver
	Mounter                  *mount.SafeFormatAndMount
	DeviceUtils              deviceutils.DeviceUtils
	VolumeStatter            mountmanager.Statter
	MetadataService          metadataservice.MetadataService
	EnableDataCache          bool
	DataCacheEnabledNodePool bool
	SysfsPath                string

	// A map storing all volumes with ongoing operations so that additional operations
	// for that same volume (as defined by VolumeID) return an Aborted error
	volumeLocks *common.VolumeLocks

	// enableDeviceInUseCheck, if true, will block NodeUnstageVolume request if the specified
	// device is still in use (or until --device-in-use-timeout is reached, if specified)
	enableDeviceInUseCheck bool
	// deviceInUseErrors keeps tracks of device names and a timestamp for when an error is
	// encounted for that device
	deviceInUseErrors *deviceErrMap

	// If set, this semaphore will be used to serialize formatAndMount. It will be raised
	// when the operation starts, and lowered either when finished, or when
	// formatAndMountTimeout has expired.
	//
	// This is used only on linux (where memory problems for concurrent fsck and mkfs have
	// been observed).
	formatAndMountSemaphore chan any
	formatAndMountTimeout   time.Duration

	// Embed UnimplementedNodeServer to ensure the driver returns Unimplemented for any
	// new RPC methods that might be introduced in future versions of the spec.
	csi.UnimplementedNodeServer

	metricsManager *metrics.MetricsManager
}

type NodeServerArgs struct {
	// EnableDeviceInUseCheck enables functionality which will block NodeUnstageVolume request
	// until the device is not in use
	EnableDeviceInUseCheck bool

	DeviceInUseTimeout time.Duration

	EnableDataCache bool

	DataCacheEnabledNodePool bool

	// SysfsPath defaults to "/sys", except if it's a unit test.
	SysfsPath string

	MetricsManager *metrics.MetricsManager
}

var _ csi.NodeServer = &GCENodeServer{}

// The constants are used to map from the machine type to the limit of
// persistent disks that can be attached to an instance. Please refer to gcloud
// doc https://cloud.google.com/compute/docs/disks/#pdnumberlimits
// These constants are all the documented attach limit minus one because the
// node boot disk is considered an attachable disk so effective attach limit is
// one less.
const (
	volumeLimitSmall int64 = 15
	volumeLimitBig   int64 = 127
	// doc https://cloud.google.com/compute/docs/memory-optimized-machines#x4_disks
	x4HyperdiskLimit int64 = 39
	// doc https://cloud.google.com/compute/docs/accelerator-optimized-machines#a4-disks
	a4HyperdiskLimit     int64 = 127
	defaultLinuxFsType         = "ext4"
	defaultWindowsFsType       = "ntfs"
	fsTypeExt3                 = "ext3"
	fsTypeBtrfs                = "btrfs"

	readAheadKBMountFlagRegexPattern = "^read_ahead_kb=(.+)$"
	btrfsReclaimDataRegexPattern     = "^btrfs-allocation-data-bg_reclaim_threshold=(\\d{1,2})$"     // 0-99 are valid, incl. 00
	btrfsReclaimMetadataRegexPattern = "^btrfs-allocation-metadata-bg_reclaim_threshold=(\\d{1,2})$" // ditto ^
)

var (
	readAheadKBMountFlagRegex = regexp.MustCompile(readAheadKBMountFlagRegexPattern)
	btrfsReclaimDataRegex     = regexp.MustCompile(btrfsReclaimDataRegexPattern)
	btrfsReclaimMetadataRegex = regexp.MustCompile(btrfsReclaimMetadataRegexPattern)
)

func getDefaultFsType() string {
	if runtime.GOOS == "windows" {
		return defaultWindowsFsType
	} else {
		return defaultLinuxFsType
	}
}

func (ns *GCENodeServer) isVolumePathMounted(path string) bool {
	notMnt, err := ns.Mounter.Interface.IsLikelyNotMountPoint(path)
	klog.V(4).Infof("Checking volume path %s is mounted %t: error %v", path, !notMnt, err)
	if err == nil && !notMnt {
		// TODO(#95): check if mount is compatible. Return OK if it is, or appropriate error.
		/*
			1) Target Path MUST be the vol referenced by vol ID
			2) TODO(#253): Check volume capability matches for ALREADY_EXISTS
			3) Readonly MUST match
		*/
		return true
	}
	return false
}

func (ns *GCENodeServer) WithSerializedFormatAndMount(timeout time.Duration, maxConcurrent int) *GCENodeServer {
	if maxConcurrent > 0 {
		ns.formatAndMountSemaphore = make(chan any, maxConcurrent)
		ns.formatAndMountTimeout = timeout
	}
	return ns
}

func (ns *GCENodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	// Validate Arguments
	targetPath := req.GetTargetPath()
	stagingTargetPath := req.GetStagingTargetPath()
	readOnly := req.GetReadonly()
	volumeID := req.GetVolumeId()
	volumeCapability := req.GetVolumeCapability()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Volume ID must be provided")
	}
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Staging Target Path must be provided")
	}
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Target Path must be provided")
	}
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Volume Capability must be provided")
	}

	if acquired := ns.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, common.VolumeOperationAlreadyExistsFmt, volumeID)
	}
	defer ns.volumeLocks.Release(volumeID)

	if err := validateVolumeCapability(volumeCapability); err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("VolumeCapability is invalid: %v", err.Error()))
	}

	if ns.isVolumePathMounted(targetPath) {
		klog.V(4).Infof("NodePublishVolume succeeded on volume %v to %s, mount already exists.", volumeID, targetPath)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// Perform a bind mount to the full path to allow duplicate mounts of the same PD.
	fstype := ""
	sourcePath := ""
	options := []string{"bind"}
	if readOnly {
		options = append(options, "ro")
	}
	var err error

	if mnt := volumeCapability.GetMount(); mnt != nil {
		if mnt.FsType != "" {
			fstype = mnt.FsType
		} else {
			// Default fstype is ext4
			fstype = "ext4"
		}

		klog.V(4).Infof("NodePublishVolume with filesystem %s", fstype)
		options = append(options, collectMountOptions(fstype, mnt.MountFlags)...)

		sourcePath = stagingTargetPath
		if err := preparePublishPath(targetPath, ns.Mounter); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("mkdir failed on disk %s (%v)", targetPath, err.Error()))
		}

	} else if blk := volumeCapability.GetBlock(); blk != nil {
		klog.V(4).Infof("NodePublishVolume with block volume mode")

		partition := ""
		if part, ok := req.GetVolumeContext()[common.VolumeAttributePartition]; ok {
			partition = part
		}

		sourcePath, err = getDevicePath(ns, volumeID, partition)
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Error when getting device path: %v", err.Error()))
		}

		// Expose block volume as file at target path
		err = makeFile(targetPath)
		if err != nil {
			if removeErr := os.Remove(targetPath); removeErr != nil {
				return nil, status.Error(codes.Internal, fmt.Sprintf("Error removing block file at target path %v: %v, mounti error: %v", targetPath, removeErr, err.Error()))
			}
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to create block file at target path %v: %v", targetPath, err.Error()))
		}
	} else {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("NodePublishVolume volume capability must specify either mount or block mode"))
	}

	err = ns.Mounter.Interface.Mount(sourcePath, targetPath, fstype, options)
	if err != nil {
		klog.Errorf("Mount of disk %s failed: %v", targetPath, err.Error())
		notMnt, mntErr := ns.Mounter.Interface.IsLikelyNotMountPoint(targetPath)
		if mntErr != nil {
			klog.Errorf("IsLikelyNotMountPoint check failed: %v", mntErr.Error())
			return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume failed to check whether target path is a mount point: %v", err.Error()))
		}
		if !notMnt {
			// TODO: check the logic here again. If mntErr == nil & notMnt == false, it means volume is actually mounted.
			// Why need to unmount?
			klog.Warningf("Although volume mount failed, but IsLikelyNotMountPoint returns volume %s is mounted already at %s", volumeID, targetPath)
			if mntErr = ns.Mounter.Interface.Unmount(targetPath); mntErr != nil {
				klog.Errorf("Failed to unmount: %v", mntErr.Error())
				return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume failed to unmount target path: %v", err.Error()))
			}
			notMnt, mntErr := ns.Mounter.Interface.IsLikelyNotMountPoint(targetPath)
			if mntErr != nil {
				klog.Errorf("IsLikelyNotMountPoint check failed: %v", mntErr.Error())
				return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume failed to check whether target path is a mount point: %v", err.Error()))
			}
			if !notMnt {
				// This is very odd, we don't expect it.  We'll try again next sync loop.
				klog.Errorf("%s is still mounted, despite call to unmount().  Will try again next sync loop.", targetPath)
				return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume something is wrong with mounting: %v", err.Error()))
			}
		}
		if err := os.Remove(targetPath); err != nil {
			klog.Errorf("failed to remove targetPath %s: %v", targetPath, err.Error())
		}
		return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume mount of disk failed: %v", err.Error()))
	}

	klog.V(4).Infof("NodePublishVolume succeeded on volume %v to %s", volumeID, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

func makeFile(path string) error {
	// Create file
	newFile, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0750)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", path, err)
	}
	if err := newFile.Close(); err != nil {
		return fmt.Errorf("failed to close file %s: %w", path, err)
	}
	return nil
}

func (ns *GCENodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	// Validate Arguments
	targetPath := req.GetTargetPath()
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume Volume ID must be provided")
	}
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume Target Path must be provided")
	}

	if acquired := ns.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, common.VolumeOperationAlreadyExistsFmt, volumeID)
	}
	defer ns.volumeLocks.Release(volumeID)

	if err := cleanupPublishPath(targetPath, ns.Mounter); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Unmount failed: %v\nUnmounting arguments: %s\n", err.Error(), targetPath))
	}
	klog.V(4).Infof("NodeUnpublishVolume succeeded on %v from %s", volumeID, targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *GCENodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	// Validate Arguments
	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()
	volumeCapability := req.GetVolumeCapability()
	nodeId := ns.MetadataService.GetName()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Volume ID must be provided")
	}
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Staging Target Path must be provided")
	}
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Volume Capability must be provided")
	}

	if acquired := ns.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, common.VolumeOperationAlreadyExistsFmt, volumeID)
	}
	defer ns.volumeLocks.Release(volumeID)

	if err := validateVolumeCapability(volumeCapability); err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("VolumeCapability is invalid: %v", err.Error()))
	}

	// TODO(#253): Check volume capability matches for ALREADY_EXISTS

	_, volumeKey, err := common.VolumeIDToKey(volumeID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("NodeStageVolume Volume ID is invalid: %v", err.Error()))
	}

	// Part 1: Get device path of attached device
	partition := ""

	if part, ok := req.GetVolumeContext()[common.VolumeAttributePartition]; ok {
		partition = part
	}
	devicePath, err := getDevicePath(ns, volumeID, partition)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Error when getting device path: %v", err.Error()))
	}

	klog.Infof("Successfully found attached GCE PD %q at device path %s.", volumeKey.Name, devicePath)

	if ns.EnableDataCache && (req.GetPublishContext()[common.ContextDataCacheSize] != "" || req.GetPublishContext()[common.ContextDataCacheMode] != "") {
		if len(nodeId) == 0 {
			return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Node ID must be provided")
		}
		devFsPath, err := filepath.EvalSymlinks(devicePath)
		if err != nil {
			klog.Errorf("filepath.EvalSymlinks(%q) failed when trying to create volume group: %v", devicePath, err)
		}
		configError := ValidateDataCacheConfig(req.GetPublishContext()[common.ContextDataCacheMode], req.GetPublishContext()[common.ContextDataCacheSize], ctx)
		if configError != nil {
			if ns.DataCacheEnabledNodePool {
				return nil, status.Error(codes.DataLoss, fmt.Sprintf("Error validate configuration for Data Cache: %v", configError.Error()))
			}
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("The Data Cache PVC is scheduled on an incompatible node pool. Please select a node pool with data cache configured: %v", configError.Error()))
		}
		devicePath, err = setupCaching(devFsPath, req, nodeId)
		if err != nil {
			errStatus, _ := status.FromError(err)
			if errStatus.Code() == codes.InvalidArgument {
				return nil, err
			}
			return nil, status.Error(codes.DataLoss, fmt.Sprintf("Error setting up cache: %v", err.Error()))
		}
	}

	// Part 2: Check if mount already exists at stagingTargetPath
	if ns.isVolumePathMounted(stagingTargetPath) {
		klog.V(4).Infof("NodeStageVolume succeeded on volume %v to %s, mount already exists.", volumeID, stagingTargetPath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	if err := prepareStagePath(stagingTargetPath, ns.Mounter); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("mkdir failed on disk %s (%v)", stagingTargetPath, err.Error()))
	}

	// Part 3: Mount device to stagingTargetPath
	fstype := getDefaultFsType()

	var btrfsReclaimData, btrfsReclaimMetadata string
	shouldUpdateReadAhead := false
	var readAheadKB int64
	options := []string{}
	if mnt := volumeCapability.GetMount(); mnt != nil {
		if mnt.FsType != "" {
			fstype = mnt.FsType
		}
		options = collectMountOptions(fstype, mnt.MountFlags)

		readAheadKB, shouldUpdateReadAhead, err = extractReadAheadKBMountFlag(mnt.MountFlags)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failure parsing mount flags: %v", err.Error())
		}

		if mnt.FsType == fsTypeBtrfs {
			btrfsReclaimData, btrfsReclaimMetadata = extractBtrfsReclaimFlags(mnt.MountFlags)
		}
	} else if blk := volumeCapability.GetBlock(); blk != nil {
		// Noop for Block NodeStageVolume
		klog.V(4).Infof("NodeStageVolume succeeded on %v to %s, capability is block so this is a no-op", volumeID, stagingTargetPath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	readonly, _ := getReadOnlyFromCapability(volumeCapability)
	if readonly {
		options = append(options, "ro")
		klog.V(4).Infof("CSI volume is read-only, mounting with extra option ro")
	}

	err = ns.formatAndMount(devicePath, stagingTargetPath, fstype, options, ns.Mounter)
	if err != nil {
		// If a volume is created from a content source like snapshot or cloning, the filesystem might get marked
		// as "dirty" even if it is otherwise consistent and ext3/4 will try to restore to a consistent state by replaying
		// the journal which is not possible in read-only mode. So we'll try again with noload option to skip it. This may
		// allow mounting of an actually inconsistent filesystem, but because the mount is read-only no further damage should
		// be caused.
		if readonly && (fstype == defaultLinuxFsType || fstype == fsTypeExt3) {
			klog.V(4).Infof("Failed to mount CSI volume read-only, retry mounting with extra option noload")

			options = append(options, "noload")
			err = ns.formatAndMount(devicePath, stagingTargetPath, fstype, options, ns.Mounter)
			if err == nil {
				klog.V(4).Infof("NodeStageVolume succeeded with \"noload\" option on %v to %s", volumeID, stagingTargetPath)
				return &csi.NodeStageVolumeResponse{}, nil
			}
		}

		return nil, status.Error(codes.Internal,
			fmt.Sprintf("Failed to format and mount device from (%q) to (%q) with fstype (%q) and options (%q): %v",
				devicePath, stagingTargetPath, fstype, options, err.Error()))
	}

	// Part 4: Resize filesystem.
	// https://github.com/kubernetes/kubernetes/issues/94929
	if !readonly {
		resizer := resizefs.NewResizeFs(ns.Mounter)
		_, err = ns.DeviceUtils.Resize(resizer, devicePath, stagingTargetPath)
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("error when resizing volume %s from device '%s' at path '%s': %v", volumeID, devicePath, stagingTargetPath, err.Error()))
		}
	}

	// Part 5: Update read_ahead
	if shouldUpdateReadAhead {
		if err := ns.updateReadAhead(devicePath, readAheadKB); err != nil {
			return nil, status.Errorf(codes.Internal, "failure updating readahead for %s to %dKB: %v", devicePath, readAheadKB, err.Error())
		}
	}

	// Part 6: if configured, write sysfs values
	if !readonly {
		sysfs := map[string]string{}
		if btrfsReclaimData != "" {
			sysfs["allocation/data/bg_reclaim_threshold"] = btrfsReclaimData
		}
		if btrfsReclaimMetadata != "" {
			sysfs["allocation/metadata/bg_reclaim_threshold"] = btrfsReclaimMetadata
		}

		if len(sysfs) > 0 {
			args := []string{"--match-tag", "UUID", "--output", "value", stagingTargetPath}
			cmd := ns.Mounter.Exec.Command("blkid", args...)
			var stderr bytes.Buffer
			cmd.SetStderr(&stderr)
			klog.V(4).Infof(
				"running %q for volume %s",
				strings.Join(append([]string{"blkid"}, args...), " "),
				volumeID,
			)
			uuid, err := cmd.Output()
			if err != nil {
				klog.Errorf("blkid failed for %s. stderr:\n%s", volumeID, stderr.String())
				return nil, status.Errorf(codes.Internal, "blkid failed: %v", err)
			}
			uuid = bytes.TrimRight(uuid, "\n")

			for key, value := range sysfs {
				path := fmt.Sprintf("%s/fs/btrfs/%s/%s", ns.SysfsPath, uuid, key)
				if err := writeSysfs(path, value); err != nil {
					return nil, status.Error(codes.Internal, err.Error())
				}
				klog.V(4).Infof("NodeStageVolume set %s %s=%s", volumeID, key, value)
			}
		}
	}

	klog.V(4).Infof("NodeStageVolume succeeded on %v to %s", volumeID, stagingTargetPath)
	return &csi.NodeStageVolumeResponse{}, nil
}

func writeSysfs(path, value string) (_err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	defer func() {
		_err = errors.Join(_err, f.Close())
	}()

	if _, err := f.Write([]byte(value)); err != nil {
		return err
	}

	return nil
}

func (ns *GCENodeServer) updateReadAhead(devicePath string, readAheadKB int64) error {
	isBlock, err := ns.VolumeStatter.IsBlockDevice(devicePath)
	if err != nil {
		return fmt.Errorf("failed to determine whether %s is a block device: %v", devicePath, err)
	}
	if !isBlock {
		return nil
	}

	if err := setReadAheadKB(devicePath, readAheadKB, ns.Mounter); err != nil {
		return fmt.Errorf("failed to set readahead: %v", err)
	}

	return nil
}

func extractBtrfsReclaimFlags(mountFlags []string) (string, string) {
	var reclaimData, reclaimMetadata string
	for _, mountFlag := range mountFlags {
		if got := btrfsReclaimDataRegex.FindStringSubmatch(mountFlag); len(got) == 2 {
			reclaimData = got[1]
		} else if got := btrfsReclaimMetadataRegex.FindStringSubmatch(mountFlag); len(got) == 2 {
			reclaimMetadata = got[1]
		}
	}
	return reclaimData, reclaimMetadata
}

func extractReadAheadKBMountFlag(mountFlags []string) (int64, bool, error) {
	for _, mountFlag := range mountFlags {
		if readAheadKB := readAheadKBMountFlagRegex.FindStringSubmatch(mountFlag); len(readAheadKB) == 2 {
			// There is only one matching pattern in readAheadKBMountFlagRegex
			// If found, it will be at index 1
			readAheadKBInt, err := strconv.ParseInt(readAheadKB[1], 10, 0)
			if err != nil {
				return -1, false, fmt.Errorf("invalid read_ahead_kb mount flag %q: %v", mountFlag, err)
			}
			if readAheadKBInt < 0 {
				// Negative values can result in unintuitive values when setting read ahead
				// (due to blockdev intepreting negative integers as large positive integers).
				return -1, false, fmt.Errorf("invalid negative value for read_ahead_kb mount flag: %q", mountFlag)
			}
			return readAheadKBInt, true, nil
		}
	}
	return -1, false, nil
}

func (ns *GCENodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	// Validate arguments
	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnstageVolume Volume ID must be provided")
	}
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnstageVolume Staging Target Path must be provided")
	}

	if acquired := ns.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, common.VolumeOperationAlreadyExistsFmt, volumeID)
	}
	defer ns.volumeLocks.Release(volumeID)

	if err := cleanupStagePath(stagingTargetPath, ns.Mounter); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("NodeUnstageVolume failed: %v\nUnmounting arguments: %s\n", err.Error(), stagingTargetPath))
	}

	if ns.enableDeviceInUseCheck {
		if err := ns.confirmDeviceUnused(volumeID); err != nil {
			var ignoreableErr *ignoreableError
			if errors.As(err, &ignoreableErr) {
				klog.Warningf("Unable to check if device for %s is unused. Device has been unmounted successfully. Ignoring and continuing with unstaging. (%v)", volumeID, err)
			} else if ns.deviceInUseErrors.deviceErrorExpired(volumeID) {
				klog.Warningf("Device %s could not be released after timeout of %f seconds. NodeUnstageVolume will return success.", volumeID, ns.deviceInUseErrors.timeout.Seconds())
			} else {
				ns.deviceInUseErrors.markDeviceError(volumeID)
				return nil, status.Errorf(codes.Internal, "NodeUnstageVolume for volume %s failed: %v", volumeID, err)
			}
		}
		ns.deviceInUseErrors.deleteDevice(volumeID)
	}

	// The NodeUnstageVolume does not have any volume or publish context, we need to get the info from LVM locally
	// Check if cache group cache-{volumeID} exist in LVM
	if ns.EnableDataCache && ns.DataCacheEnabledNodePool {
		nodeId := ns.MetadataService.GetName()
		err := cleanupCache(volumeID, nodeId)
		if err != nil {
			return nil, status.Errorf(codes.DataLoss, "Failed to cleanup cache for volume %s: %v", volumeID, err)
		}
	}
	klog.V(4).Infof("NodeUnstageVolume succeeded on %v from %s", volumeID, stagingTargetPath)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// A private error wrapper used to pass control flow decisions up to the caller
type ignoreableError struct{ error }

func (ns *GCENodeServer) confirmDeviceUnused(volumeID string) error {
	devicePath, err := getDevicePath(ns, volumeID, "" /* partition, which is unused */)
	if err != nil {
		return &ignoreableError{fmt.Errorf("failed to find device path for volume %s: %v", volumeID, err.Error())}
	}

	devFsPath, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return &ignoreableError{fmt.Errorf("filepath.EvalSymlinks(%q) failed: %v", devicePath, err)}
	}

	if inUse, err := ns.DeviceUtils.IsDeviceFilesystemInUse(ns.Mounter, devicePath, devFsPath); err != nil {
		return &ignoreableError{fmt.Errorf("failed to check if device %s (aka %s) is in use: %v", devicePath, devFsPath, err)}
	} else if inUse {
		return fmt.Errorf("device %s (aka %s) is still in use", devicePath, devFsPath)
	}

	return nil
}

func (ns *GCENodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: ns.Driver.nscap,
	}, nil
}

func (ns *GCENodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	top := &csi.Topology{
		Segments: map[string]string{common.TopologyKeyZone: ns.MetadataService.GetZone()},
	}

	nodeID := common.CreateNodeID(ns.MetadataService.GetProject(), ns.MetadataService.GetZone(), ns.MetadataService.GetName())

	volumeLimits, err := ns.GetVolumeLimits(ctx)
	if err != nil {
		klog.Errorf("GetVolumeLimits failed: %v. The error is ignored so that the driver can register", err.Error())
		// No error should be returned from NodeGetInfo, otherwise the driver will not register
		err = nil
	}

	resp := &csi.NodeGetInfoResponse{
		NodeId:             nodeID,
		MaxVolumesPerNode:  volumeLimits,
		AccessibleTopology: top,
	}
	return resp, err
}

func (ns *GCENodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if len(req.VolumeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats volume ID was empty")
	}
	if len(req.VolumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats volume path was empty")
	}

	_, err := os.Lstat(req.VolumePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "path %s does not exist", req.VolumePath)
		}
		return nil, status.Errorf(codes.Internal, "unknown error when stat on %s: %v", req.VolumePath, err.Error())
	}

	isBlock, err := ns.VolumeStatter.IsBlockDevice(req.VolumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to determine whether %s is block device: %v", req.VolumePath, err.Error())
	}
	if isBlock {
		bcap, err := getBlockSizeBytes(req.VolumePath, ns.Mounter)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get block capacity on path %s: %v", req.VolumePath, err.Error())
		}
		return &csi.NodeGetVolumeStatsResponse{
			Usage: []*csi.VolumeUsage{
				{
					Unit:  csi.VolumeUsage_BYTES,
					Total: bcap,
				},
			},
		}, nil
	}
	available, capacity, used, inodesFree, inodes, inodesUsed, err := ns.VolumeStatter.StatFS(req.VolumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get fs info on path %s: %v", req.VolumePath, err.Error())
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Available: available,
				Total:     capacity,
				Used:      used,
			},
			{
				Unit:      csi.VolumeUsage_INODES,
				Available: inodesFree,
				Total:     inodes,
				Used:      inodesUsed,
			},
		},
	}, nil
}

func (ns *GCENodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID must be provided")
	}
	capacityRange := req.GetCapacityRange()
	reqBytes, err := getRequestCapacity(capacityRange)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("capacity range is invalid: %v", err.Error()))
	}

	volumePath := req.GetVolumePath()
	if len(volumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume path must be provided")
	}

	_, volKey, err := common.VolumeIDToKey(volumeID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("volume ID is invalid: %v", err.Error()))
	}

	devicePath, err := getDevicePath(ns, volumeID, "")
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("error when getting device path for %s: %v", volumeID, err.Error()))
	}

	volumeCapability := req.GetVolumeCapability()
	if volumeCapability != nil {
		// VolumeCapability is optional, if specified, validate it
		if err := validateVolumeCapability(volumeCapability); err != nil {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("VolumeCapability is invalid: %v", err.Error()))
		}

		if blk := volumeCapability.GetBlock(); blk != nil {
			// Noop for Block NodeExpandVolume
			klog.V(4).Infof("NodeExpandVolume succeeded on %v to %s, capability is block so this is a no-op", volumeID, volumePath)
			return &csi.NodeExpandVolumeResponse{}, nil
		}

		readonly, err := getReadOnlyFromCapability(volumeCapability)
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("failed to check if capability for volume %s is readonly: %v", volumeID, err))
		}
		if readonly {
			klog.V(4).Infof("NodeExpandVolume succeeded on %v to %s, capability access is readonly so this is a no-op", volumeID, volumePath)
			return &csi.NodeExpandVolumeResponse{}, nil
		}
	}

	// TODO(#328): Use requested size in resize if provided
	resizer := resizefs.NewResizeFs(ns.Mounter)
	_, err = resizer.Resize(devicePath, volumePath)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("error when resizing volume %s from device '%s' at path '%s': %v", volKey.String(), devicePath, volumePath, err.Error()))

	}

	diskSizeBytes, err := getBlockSizeBytes(devicePath, ns.Mounter)
	if diskSizeBytes < reqBytes {
		// It's possible that the somewhere the volume size was rounded up, getting more size than requested is a success :)
		return nil, status.Errorf(codes.Internal, "resize requested for %v but after resize volume was size %v", reqBytes, diskSizeBytes)
	}

	// TODO(dyzz) Some sort of formatted volume could also check the fs size.
	// Issue is that we don't know how to account for filesystem overhead, it
	// could be proportional to fs size and different for xfs, ext4 and we don't
	// know the proportions

	/*
		format, err := ns.Mounter.GetDiskFormat(devicePath)
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("ControllerExpandVolume error checking format for device %s: %v", devicePath, err.Error()))
		}
		gotSizeBytes, err = ns.getFSSizeBytes(devicePath)

		if err != nil {
			return nil, status.Errorf(codes.Internal, "ControllerExpandVolume resize could not get fs size of %s: %v", volumePath, err.Error())
		}
		if gotSizeBytes != reqBytes {
			return nil, status.Errorf(codes.Internal, "ControllerExpandVolume resize requested for size %v but after resize volume was size %v", reqBytes, gotSizeBytes)
		}
	*/

	// Respond
	klog.V(4).Infof("NodeExpandVolume succeeded on volume %v to size %v", volKey, reqBytes)
	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: reqBytes,
	}, nil
}

func (ns *GCENodeServer) GetVolumeLimits(ctx context.Context) (int64, error) {
	// Machine-type format: n1-type-CPUS or custom-CPUS-RAM or f1/g1-type
	machineType := ns.MetadataService.GetMachineType()

	smallMachineTypes := []string{"f1-micro", "g1-small", "e2-micro", "e2-small", "e2-medium"}
	for _, st := range smallMachineTypes {
		if machineType == st {
			return volumeLimitSmall, nil
		}
	}

	// Get attach limit override from label
	attachLimitOverride, err := GetAttachLimitsOverrideFromNodeLabel(ctx, ns.MetadataService.GetName())
	if err == nil && attachLimitOverride > 0 && attachLimitOverride < 128 {
		return attachLimitOverride, nil
	} else {
		// If there is an error or the range is not valid, still proceed to get defaults for the machine type
		if err != nil {
			klog.Warningf("using default value due to err getting node-restriction.kubernetes.io/gke-volume-attach-limit-override: %v", err)
		}
		if attachLimitOverride != 0 {
			klog.Warningf("using default value due to invalid node-restriction.kubernetes.io/gke-volume-attach-limit-override: %d", attachLimitOverride)
		}
	}

	// Process gen4 machine attach limits
	gen4MachineTypesPrefix := []string{"c4a-", "c4-", "n4-", "c4d-"}
	for _, gen4Prefix := range gen4MachineTypesPrefix {
		if strings.HasPrefix(machineType, gen4Prefix) {
			machineTypeSlice := strings.Split(machineType, "-")
			if len(machineTypeSlice) > 2 {
				cpuString := machineTypeSlice[2]
				cpus, err := strconv.ParseInt(cpuString, 10, 64)
				if err != nil {
					return volumeLimitBig, fmt.Errorf("invalid cpuString %s for machine type: %v", cpuString, machineType)
				}
				// Extract the machine type prefix (e.g., "c4", "c4a", "n4")
				prefix := strings.TrimSuffix(gen4Prefix, "-")
				return common.GetHyperdiskAttachLimit(prefix, cpus), nil
			} else {
				return volumeLimitBig, fmt.Errorf("unconventional machine type: %v", machineType)
			}
		}
	}
	if strings.HasPrefix(machineType, "x4-") {
		return x4HyperdiskLimit, nil
	}
	if strings.HasPrefix(machineType, "a4-") {
		return a4HyperdiskLimit, nil
	}

	return volumeLimitBig, nil
}

func GetAttachLimitsOverrideFromNodeLabel(ctx context.Context, nodeName string) (int64, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return 0, err
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return 0, err
	}
	node, err := getNodeWithRetry(ctx, kubeClient, nodeName)
	if err != nil {
		return 0, err
	}
	if val, found := node.GetLabels()[fmt.Sprintf(common.NodeRestrictionLabelPrefix, common.AttachLimitOverrideLabel)]; found {
		attachLimitOverrideForNode, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("error getting attach limit override from node label: %v", err)
		}
		klog.V(4).Infof("attach limit override for the node: %v", attachLimitOverrideForNode)
		return attachLimitOverrideForNode, nil
	}
	return 0, nil
}
