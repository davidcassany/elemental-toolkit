/*
Copyright © 2022 - 2023 SUSE LLC

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

package elemental

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	cnst "github.com/rancher/elemental-toolkit/pkg/constants"
	"github.com/rancher/elemental-toolkit/pkg/partitioner"
	v1 "github.com/rancher/elemental-toolkit/pkg/types/v1"
	"github.com/rancher/elemental-toolkit/pkg/utils"
)

// elemental package inclues a collection of helper methods to run elemental specific
// operations such as creating, formatting and mounting images or partitions and similar
// high level operations

// FormatPartition will format an already existing partition
func FormatPartition(c v1.Config, part *v1.Partition, opts ...string) error {
	c.Logger.Infof("Formatting '%s' partition", part.Name)
	return partitioner.FormatDevice(c.Runner, part.Path, part.FS, part.FilesystemLabel, opts...)
}

// PartitionAndFormatDevice creates a new empty partition table on target disk
// and applies the configured disk layout by creating and formatting all
// required partitions
func PartitionAndFormatDevice(c v1.Config, i *v1.InstallSpec) error {
	disk := partitioner.NewDisk(
		i.Target,
		partitioner.WithRunner(c.Runner),
		partitioner.WithFS(c.Fs),
		partitioner.WithLogger(c.Logger),
	)

	if !disk.Exists() {
		c.Logger.Errorf("Disk %s does not exist", i.Target)
		return fmt.Errorf("disk %s does not exist", i.Target)
	}

	c.Logger.Infof("Partitioning device...")
	out, err := disk.NewPartitionTable(i.PartTable)
	if err != nil {
		c.Logger.Errorf("Failed creating new partition table: %s", out)
		return err
	}

	parts := i.Partitions.PartitionsByInstallOrder(i.ExtraPartitions)
	return createPartitions(c, disk, parts)
}

func createAndFormatPartition(c v1.Config, disk *partitioner.Disk, part *v1.Partition) error {
	c.Logger.Debugf("Adding partition %s", part.Name)
	num, err := disk.AddPartition(part.Size, part.FS, part.Name, part.Flags...)
	if err != nil {
		c.Logger.Errorf("Failed creating %s partition", part.Name)
		return err
	}
	partDev, err := disk.FindPartitionDevice(num)
	if err != nil {
		return err
	}
	if part.FS != "" {
		c.Logger.Debugf("Formatting partition with label %s", part.FilesystemLabel)
		err = partitioner.FormatDevice(c.Runner, partDev, part.FS, part.FilesystemLabel)
		if err != nil {
			c.Logger.Errorf("Failed formatting partition %s", part.Name)
			return err
		}
	} else {
		c.Logger.Debugf("Wipe file system on %s", part.Name)
		err = disk.WipeFsOnPartition(partDev)
		if err != nil {
			c.Logger.Errorf("Failed to wipe filesystem of partition %s", partDev)
			return err
		}
	}
	part.Path = partDev
	return nil
}

func createPartitions(c v1.Config, disk *partitioner.Disk, parts v1.PartitionList) error {
	for _, part := range parts {
		err := createAndFormatPartition(c, disk, part)
		if err != nil {
			return err
		}
	}
	return nil
}

// MountPartitions mounts configured partitions. Partitions with an unset mountpoint are not mounted.
// Note umounts must be handled by caller logic.
func MountPartitions(c v1.Config, parts v1.PartitionList) error {
	c.Logger.Infof("Mounting disk partitions")
	var err error

	for _, part := range parts {
		if part.MountPoint != "" {
			err = MountPartition(c, part, "rw")
			if err != nil {
				_ = UnmountPartitions(c, parts)
				return err
			}
		}
	}

	return err
}

// UnmountPartitions unmounts configured partitiosn. Partitions with an unset mountpoint are not unmounted.
func UnmountPartitions(c v1.Config, parts v1.PartitionList) error {
	c.Logger.Infof("Unmounting disk partitions")
	var err error
	errMsg := ""
	failure := false

	// If there is an early error we still try to unmount other partitions
	for _, part := range parts {
		if part.MountPoint != "" {
			err = UnmountPartition(c, part)
			if err != nil {
				errMsg += fmt.Sprintf("Failed to unmount %s\n", part.MountPoint)
				failure = true
			}
		}
	}
	if failure {
		return errors.New(errMsg)
	}
	return nil
}

// MountRWPartition mounts, or remounts if needed, a partition with RW permissions
func MountRWPartition(c v1.Config, part *v1.Partition) (umount func() error, err error) {
	if mnt, _ := utils.IsMounted(c, part); mnt {
		err = MountPartition(c, part, "remount", "rw")
		if err != nil {
			c.Logger.Errorf("failed mounting %s partition: %v", part.Name, err)
			return nil, err
		}
		umount = func() error { return MountPartition(c, part, "remount", "ro") }
	} else {
		err = MountPartition(c, part, "rw")
		if err != nil {
			c.Logger.Error("failed mounting %s partition: %v", part.Name, err)
			return nil, err
		}
		umount = func() error { return UnmountPartition(c, part) }
	}
	return umount, nil
}

// MountPartition mounts a partition with the given mount options
func MountPartition(c v1.Config, part *v1.Partition, opts ...string) error {
	c.Logger.Debugf("Mounting partition %s", part.FilesystemLabel)
	err := utils.MkdirAll(c.Fs, part.MountPoint, cnst.DirPerm)
	if err != nil {
		return err
	}
	if part.Path == "" {
		// Lets error out only after 10 attempts to find the device
		device, err := utils.GetDeviceByLabel(c.Runner, part.FilesystemLabel, 10)
		if err != nil {
			c.Logger.Errorf("Could not find a device with label %s", part.FilesystemLabel)
			return err
		}
		part.Path = device
	}
	err = c.Mounter.Mount(part.Path, part.MountPoint, "auto", opts)
	if err != nil {
		c.Logger.Errorf("Failed mounting device %s with label %s", part.Path, part.FilesystemLabel)
		return err
	}
	return nil
}

// UnmountPartition unmounts the given partition or does nothing if not mounted
func UnmountPartition(c v1.Config, part *v1.Partition) error {
	if mnt, _ := utils.IsMounted(c, part); !mnt {
		c.Logger.Debugf("Not unmounting partition, %s doesn't look like mountpoint", part.MountPoint)
		return nil
	}
	c.Logger.Debugf("Unmounting partition %s", part.FilesystemLabel)
	return c.Mounter.Unmount(part.MountPoint)
}

// MountFileSystemImage mounts an image with the given mount options
func MountFileSystemImage(c v1.Config, img *v1.Image, opts ...string) error {
	c.Logger.Debugf("Mounting image %s to %s", img.File, img.MountPoint)
	out, err := c.Runner.Run("losetup", "--show", "-f", img.File)
	if err != nil {
		c.Logger.Errorf("failed setting a loop device for %s", img.File)
		return err
	}
	loop := strings.TrimSpace(string(out))
	c.Logger.Debugf("Loopdevice for %s set to %s", img.File, loop)
	err = c.Mounter.Mount(loop, img.MountPoint, "auto", opts)
	if err != nil {
		c.Logger.Errorf("failed to mount %s", loop)
		c.Runner.RunNoError("losetup", "-d", loop)
		return err
	}
	img.LoopDevice = loop
	return nil
}

// UmountFileSystemImage unmounts the given image or does nothing if not mounted
func UmountFileSystemImage(c v1.Config, img *v1.Image) error {
	if notMnt, _ := c.Mounter.IsLikelyNotMountPoint(img.MountPoint); notMnt {
		c.Logger.Debugf("Not unmounting image, %s doesn't look like mountpoint", img.MountPoint)
		return nil
	}

	loop := img.LoopDevice
	img.LoopDevice = ""

	c.Logger.Debugf("Unmounting image from %s", img.MountPoint)
	err := c.Mounter.Unmount(img.MountPoint)
	if err != nil {
		c.Runner.RunNoError("losetup", "-d", loop)
		return err
	}
	_, err = c.Runner.Run("losetup", "-d", loop)
	return err
}

// CreateFileSystemImage creates the image file for the given image, rootDir is used to compute the image
// size and preload boolean is to create the FS with preloaded rootDir (only for ext2-4)
func CreateFileSystemImage(c v1.Config, img *v1.Image, rootDir string, preload bool) error {
	c.Logger.Infof("Creating image %s", img.File)
	if img.Size == 0 && rootDir != "" {
		size, err := utils.DirSizeMB(c.Fs, rootDir)
		if err != nil {
			return err
		}
		img.Size = size + cnst.ImgOverhead
		c.Logger.Debugf("Image size %dM", img.Size)
	}
	err := utils.MkdirAll(c.Fs, filepath.Dir(img.File), cnst.DirPerm)
	if err != nil {
		c.Logger.Errorf("failed creatin target file directory path: %v", err)
		return err
	}

	err = utils.CreateRAWFile(c.Fs, img.File, img.Size)
	if err != nil {
		c.Logger.Errorf("failed creating raw file %s", img.File)
		return err
	}

	extraOpts := []string{}
	r := regexp.MustCompile("ext[2-4]")
	match := r.MatchString(img.FS)
	if preload && match {
		extraOpts = []string{"-d", rootDir}
	}
	if preload && !match {
		c.Logger.Errorf("Preloaded filesystem images are only supported for ext2-4 filesystems")
		return fmt.Errorf("unexpected filesystem: %s", img.FS)
	}
	mkfs := partitioner.NewMkfsCall(img.File, img.FS, img.Label, c.Runner, extraOpts...)
	_, err = mkfs.Apply()
	if err != nil {
		c.Logger.Errorf("failed formatting file %s with %s", img.File, img.FS)
		_ = c.Fs.RemoveAll(img.File)
		return err
	}
	return nil
}

// CreateSimpleFileSystemImage creates the image file for the given image
func CreateSimpleFileSystemImage(c v1.Config, img *v1.Image) error {
	return CreateFileSystemImage(c, img, "", false)
}

// CreatePreLoadedFileSystemImage creates the image file for the given image including the contents of the rootDir.
// If rootDir is empty it simply creates an empty filesystem image
func CreatePreLoadedFileSystemImage(c v1.Config, img *v1.Image, rootDir string) error {
	preload, _ := utils.Exists(c.Fs, rootDir)
	return CreateFileSystemImage(c, img, rootDir, preload)
}

// DeployImgTree will deploy the given image into the given root tree. Returns source metadata in info,
// a tree cleaner function and error. The given root will be a bind mount of a temporary directory into the same
// filesystem of img.File, this is helpful to make the deployment easily accessible in after-* hooks.
func DeployImgTree(c v1.Config, img *v1.Image, root string) (info interface{}, cleaner func() error, err error) {
	// We prepare the rootTree next to the target image file, in the same base path
	c.Logger.Infof("Preparing root tree for image: %s", img.File)
	tmp := strings.TrimSuffix(img.File, filepath.Ext(img.File))
	tmp += ".imgTree"
	err = utils.MkdirAll(c.Fs, tmp, cnst.DirPerm)
	if err != nil {
		return nil, nil, err
	}

	err = utils.MkdirAll(c.Fs, root, cnst.DirPerm)
	if err != nil {
		_ = c.Fs.RemoveAll(tmp)
		return nil, nil, err
	}
	err = c.Mounter.Mount(tmp, root, "bind", []string{"bind"})
	if err != nil {
		_ = c.Fs.RemoveAll(tmp)
		_ = c.Fs.RemoveAll(root)
		return nil, nil, err
	}

	cleaner = func() error {
		_ = c.Mounter.Unmount(root)
		err := c.Fs.RemoveAll(root)
		if err != nil {
			return err
		}
		return c.Fs.RemoveAll(tmp)
	}

	info, err = DumpSource(c, root, img.Source)
	if err != nil {
		_ = cleaner()
		return nil, nil, err
	}

	return info, cleaner, err
}

// CreateImgFromTree creates the given image from with the contents of the tree for the given root.
// NoMount flag allows formatting an image including its contents (experimental and ext* specific)
/*func CreateImgFromTree(c v1.Config, root string, img *v1.Image, noMount bool, cleaner func() error) (err error) {
	if cleaner != nil {
		defer func() {
			cErr := cleaner()
			if cErr != nil && err == nil {
				err = cErr
			}
		}()
	}

	err = CreateImageFromTree(c, img, root, noMount)

	return err
}*/

// CopyFileImg copies the files target as the source of this image. It also applies the img label over the copied image.
func CopyFileImg(c v1.Config, img *v1.Image) error {
	if !img.Source.IsFile() {
		return fmt.Errorf("Copying a file image requires an image source of file type")
	}

	err := utils.MkdirAll(c.Fs, filepath.Dir(img.File), cnst.DirPerm)
	if err != nil {
		return err
	}

	c.Logger.Infof("Copying image %s to %s", img.Source.Value(), img.File)
	err = utils.CopyFile(c.Fs, img.Source.Value(), img.File)
	if err != nil {
		return err
	}

	if img.FS != cnst.SquashFs && img.Label != "" {
		c.Logger.Infof("Setting label: %s ", img.Label)
		_, err = c.Runner.Run("tune2fs", "-L", img.Label, img.File)
	}
	return err
}

// DeployImage will deploy the given image into the target. This method
// creates the filesystem image file and fills it with the correspondant data
func DeployImage(c v1.Config, img *v1.Image) (interface{}, error) {
	c.Logger.Infof("Deploying image: %s", img.File)
	info, cleaner, err := DeployImgTree(c, img, cnst.WorkingImgDir)
	if err != nil {
		return nil, err
	}

	err = CreateImageFromTree(c, img, cnst.WorkingImgDir, false)
	if err != nil {
		return nil, err
	}
	err = cleaner()
	if err != nil {
		c.Logger.Errorf("failed cleaning deployed image tree: %v", err)
		return nil, err
	}
	return info, nil
}

// DumpSource sets the image data according to the image source type
func DumpSource(c v1.Config, target string, imgSrc *v1.ImageSource) (info interface{}, err error) { // nolint:gocyclo
	c.Logger.Infof("Copying %s source...", imgSrc.Value())

	err = utils.MkdirAll(c.Fs, target, cnst.DirPerm)
	if err != nil {
		c.Logger.Errorf("failed to create target directory %s", target)
		return nil, err
	}

	if imgSrc.IsImage() {
		if c.Cosign {
			c.Logger.Infof("Running cosing verification for %s", imgSrc.Value())
			out, err := utils.CosignVerify(
				c.Fs, c.Runner, imgSrc.Value(),
				c.CosignPubKey, v1.IsDebugLevel(c.Logger),
			)
			if err != nil {
				c.Logger.Errorf("Cosign verification failed: %s", out)
				return nil, err
			}
		}

		err = c.ImageExtractor.ExtractImage(imgSrc.Value(), target, c.Platform.String(), c.LocalImage)
		if err != nil {
			return nil, err
		}
	} else if imgSrc.IsDir() {
		excludes := []string{"/mnt", "/proc", "/sys", "/dev", "/tmp", "/host", "/run"}
		err = utils.SyncData(c.Logger, c.Runner, c.Fs, imgSrc.Value(), target, excludes...)
		if err != nil {
			return nil, err
		}
	} else if imgSrc.IsFile() {
		err = utils.MkdirAll(c.Fs, cnst.ImgSrcDir, cnst.DirPerm)
		if err != nil {
			return nil, err
		}
		img := &v1.Image{File: imgSrc.Value(), MountPoint: cnst.ImgSrcDir}
		err = MountFileSystemImage(c, img, "auto", "ro")
		if err != nil {
			return nil, err
		}
		defer UmountFileSystemImage(c, img) // nolint:errcheck
		excludes := []string{"/mnt", "/proc", "/sys", "/dev", "/tmp", "/host", "/run"}
		err = utils.SyncData(c.Logger, c.Runner, c.Fs, cnst.ImgSrcDir, target, excludes...)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("unknown image source type")
	}
	err = utils.CreateDirStructure(c.Fs, target)
	if err != nil {
		return nil, err
	}
	c.Logger.Infof("Finished copying %s into %s", imgSrc.Value(), target)
	return info, nil
}

// CopyCloudConfig will check if there is a cloud init in the config and store it on the target
func CopyCloudConfig(c v1.Config, path string, cloudInit []string) (err error) {
	if path == "" {
		c.Logger.Warnf("empty path. Will not copy cloud config files.")
		return nil
	}
	for i, ci := range cloudInit {
		customConfig := filepath.Join(path, fmt.Sprintf("9%d_custom.yaml", i))
		err = utils.GetSource(c, ci, customConfig)
		if err != nil {
			return err
		}
		if err = c.Fs.Chmod(customConfig, cnst.FilePerm); err != nil {
			return err
		}
		c.Logger.Infof("Finished copying cloud config file %s to %s", cloudInit, customConfig)
	}
	return nil
}

// SelinuxRelabel will relabel the system if it finds the binary and the context
func SelinuxRelabel(c v1.Config, rootDir string, raiseError bool) error {
	policyFile, err := utils.FindFile(c.Fs, rootDir, filepath.Join(cnst.SELinuxTargetedPolicyPath, "policy.*"))
	contextFile := filepath.Join(rootDir, cnst.SELinuxTargetedContextFile)
	contextExists, _ := utils.Exists(c.Fs, contextFile)

	if err == nil && contextExists && c.Runner.CommandExists("setfiles") {
		var out []byte
		var err error
		if rootDir == "/" || rootDir == "" {
			out, err = c.Runner.Run("setfiles", "-c", policyFile, "-e", "/dev", "-e", "/proc", "-e", "/sys", "-F", contextFile, "/")
		} else {
			out, err = c.Runner.Run("setfiles", "-c", policyFile, "-F", "-r", rootDir, contextFile, rootDir)
		}
		c.Logger.Debugf("SELinux setfiles output: %s", string(out))
		if err != nil && raiseError {
			return err
		}
	} else {
		c.Logger.Debugf("No files relabelling as SELinux utilities are not found")
	}

	return nil
}

// CheckActiveDeployment returns true if at least one of the mode sentinel files is found
func CheckActiveDeployment(c v1.Config) bool {
	c.Logger.Infof("Checking for active deployment")

	tests := []func(v1.Config) bool{IsActiveMode, IsPassiveMode, IsRecoveryMode}
	for _, t := range tests {
		if t(c) {
			return true
		}
	}

	return false
}

// IsActiveMode checks if the active mode sentinel file exists
func IsActiveMode(c v1.Config) bool {
	if ok, _ := utils.Exists(c.Fs, cnst.ActiveMode); ok {
		return true
	}
	return false
}

// IsPassiveMode checks if the passive mode sentinel file exists
func IsPassiveMode(c v1.Config) bool {
	if ok, _ := utils.Exists(c.Fs, cnst.PassiveMode); ok {
		return true
	}
	return false
}

// IsRecoveryMode checks if the recovery mode sentinel file exists
func IsRecoveryMode(c v1.Config) bool {
	if ok, _ := utils.Exists(c.Fs, cnst.RecoveryMode); ok {
		return true
	}
	return false
}

// SourceISO downloads an ISO in a temporary folder, mounts it and returns the image source to be used
// Returns a source and cleaner method to unmount and remove the temporary folder afterwards.
func SourceFormISO(c v1.Config, iso string) (*v1.ImageSource, func() error, error) {
	nilErr := func() error { return nil }

	tmpDir, err := utils.TempDir(c.Fs, "", "elemental")
	if err != nil {
		return nil, nilErr, err
	}

	cleanTmpDir := func() error { return c.Fs.RemoveAll(tmpDir) }

	tmpFile := filepath.Join(tmpDir, "elemental.iso")
	err = utils.GetSource(c, iso, tmpFile)
	if err != nil {
		return nil, cleanTmpDir, err
	}

	isoMnt := filepath.Join(tmpDir, "iso")
	err = utils.MkdirAll(c.Fs, isoMnt, cnst.DirPerm)
	if err != nil {
		return nil, cleanTmpDir, err
	}

	c.Logger.Infof("Mounting iso %s into %s", tmpFile, isoMnt)
	err = c.Mounter.Mount(tmpFile, isoMnt, "auto", []string{"loop"})
	if err != nil {
		return nil, cleanTmpDir, err
	}

	cleanAll := func() error {
		cErr := c.Mounter.Unmount(isoMnt)
		if cErr != nil {
			return cErr
		}
		return cleanTmpDir()
	}

	squashfsImg := filepath.Join(isoMnt, cnst.ISORootFile)
	ok, _ := utils.Exists(c.Fs, squashfsImg)
	if !ok {
		return nil, cleanAll, fmt.Errorf("squashfs image not found in ISO: %s", squashfsImg)
	}

	return v1.NewFileSrc(squashfsImg), cleanAll, nil
}

// DeactivateDevice deactivates unmounted the block devices present within the system.
// Useful to deactivate LVM volumes, if any, related to the target device.
func DeactivateDevices(c v1.Config) {
	// This is a best effort call, it does not error out
	c.Runner.RunNoError(
		"blkdeactivate", "--lvmoptions", "retry,wholevg",
		"--dmoptions", "force,retry", "--errors",
	)
}

func CreateImageFromTree(c v1.Config, img *v1.Image, rootDir string, preload bool) (err error) {
	if img.FS == cnst.SquashFs {
		c.Logger.Infof("Creating squashfs image for file %s", img.File)

		squashOptions := append(cnst.GetDefaultSquashfsOptions(), c.SquashFsCompressionConfig...)
		err = utils.CreateSquashFS(c.Runner, c.Logger, rootDir, img.File, squashOptions)
		if err != nil {
			return err
		}
	} else {
		err = CreateFileSystemImage(c, img, rootDir, preload)
		if err != nil {
			c.Logger.Errorf("failed creating filesystem image: %v", err)
			return err
		}
		if !preload {
			err = MountFileSystemImage(c, img, "rw")
			if err != nil {
				c.Logger.Errorf("failed mounting filesystem image: %v", err)
				return err
			}
			defer func() {
				mErr := UmountFileSystemImage(c, img)
				if err == nil && mErr != nil {
					err = mErr
				}
			}()

			c.Logger.Infof("Sync %s to %s", rootDir, img.MountPoint)
			err = utils.SyncData(c.Logger, c.Runner, c.Fs, rootDir, img.MountPoint)
			if err != nil {
				return err
			}
		}
	}
	return err
}
