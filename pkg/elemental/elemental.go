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

// Elemental is the struct meant to self-contain most utils and actions related to Elemental, like installing or applying selinux
type Elemental struct {
	config *v1.Config
}

func NewElemental(config *v1.Config) *Elemental {
	return &Elemental{
		config: config,
	}
}

// FormatPartition will format an already existing partition
func (e *Elemental) FormatPartition(part *v1.Partition, opts ...string) error {
	e.config.Logger.Infof("Formatting '%s' partition", part.Name)
	return partitioner.FormatDevice(e.config.Runner, part.Path, part.FS, part.FilesystemLabel, opts...)
}

// PartitionAndFormatDevice creates a new empty partition table on target disk
// and applies the configured disk layout by creating and formatting all
// required partitions
func (e *Elemental) PartitionAndFormatDevice(i *v1.InstallSpec) error {
	disk := partitioner.NewDisk(
		i.Target,
		partitioner.WithRunner(e.config.Runner),
		partitioner.WithFS(e.config.Fs),
		partitioner.WithLogger(e.config.Logger),
	)

	if !disk.Exists() {
		e.config.Logger.Errorf("Disk %s does not exist", i.Target)
		return fmt.Errorf("disk %s does not exist", i.Target)
	}

	e.config.Logger.Infof("Partitioning device...")
	out, err := disk.NewPartitionTable(i.PartTable)
	if err != nil {
		e.config.Logger.Errorf("Failed creating new partition table: %s", out)
		return err
	}

	parts := i.Partitions.PartitionsByInstallOrder(i.ExtraPartitions)
	return e.createPartitions(disk, parts)
}

func (e *Elemental) createAndFormatPartition(disk *partitioner.Disk, part *v1.Partition) error {
	e.config.Logger.Debugf("Adding partition %s", part.Name)
	num, err := disk.AddPartition(part.Size, part.FS, part.Name, part.Flags...)
	if err != nil {
		e.config.Logger.Errorf("Failed creating %s partition", part.Name)
		return err
	}
	partDev, err := disk.FindPartitionDevice(num)
	if err != nil {
		return err
	}
	if part.FS != "" {
		e.config.Logger.Debugf("Formatting partition with label %s", part.FilesystemLabel)
		err = partitioner.FormatDevice(e.config.Runner, partDev, part.FS, part.FilesystemLabel)
		if err != nil {
			e.config.Logger.Errorf("Failed formatting partition %s", part.Name)
			return err
		}
	} else {
		e.config.Logger.Debugf("Wipe file system on %s", part.Name)
		err = disk.WipeFsOnPartition(partDev)
		if err != nil {
			e.config.Logger.Errorf("Failed to wipe filesystem of partition %s", partDev)
			return err
		}
	}
	part.Path = partDev
	return nil
}

func (e *Elemental) createPartitions(disk *partitioner.Disk, parts v1.PartitionList) error {
	for _, part := range parts {
		err := e.createAndFormatPartition(disk, part)
		if err != nil {
			return err
		}
	}
	return nil
}

// MountPartitions mounts configured partitions. Partitions with an unset mountpoint are not mounted.
// Note umounts must be handled by caller logic.
func (e Elemental) MountPartitions(parts v1.PartitionList) error {
	e.config.Logger.Infof("Mounting disk partitions")
	var err error

	for _, part := range parts {
		if part.MountPoint != "" {
			err = MountPartition(e.config, part, "rw")
			if err != nil {
				_ = e.UnmountPartitions(parts)
				return err
			}
		}
	}

	return err
}

// UnmountPartitions unmounts configured partitiosn. Partitions with an unset mountpoint are not unmounted.
func (e Elemental) UnmountPartitions(parts v1.PartitionList) error {
	e.config.Logger.Infof("Unmounting disk partitions")
	var err error
	errMsg := ""
	failure := false

	// If there is an early error we still try to unmount other partitions
	for _, part := range parts {
		if part.MountPoint != "" {
			err = UnmountPartition(e.config, part)
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
func MountRWPartition(cfg *v1.Config, part *v1.Partition) (umount func() error, err error) {
	if mnt, _ := utils.IsMounted(cfg, part); mnt {
		err = MountPartition(cfg, part, "remount", "rw")
		if err != nil {
			cfg.Logger.Errorf("failed mounting %s partition: %v", part.Name, err)
			return nil, err
		}
		umount = func() error { return MountPartition(cfg, part, "remount", "ro") }
	} else {
		err = MountPartition(cfg, part, "rw")
		if err != nil {
			cfg.Logger.Error("failed mounting %s partition: %v", part.Name, err)
			return nil, err
		}
		umount = func() error { return UnmountPartition(cfg, part) }
	}
	return umount, nil
}

// MountPartition mounts a partition with the given mount options
func MountPartition(cfg *v1.Config, part *v1.Partition, opts ...string) error {
	cfg.Logger.Debugf("Mounting partition %s", part.FilesystemLabel)
	err := utils.MkdirAll(cfg.Fs, part.MountPoint, cnst.DirPerm)
	if err != nil {
		return err
	}
	if part.Path == "" {
		// Lets error out only after 10 attempts to find the device
		device, err := utils.GetDeviceByLabel(cfg.Runner, part.FilesystemLabel, 10)
		if err != nil {
			cfg.Logger.Errorf("Could not find a device with label %s", part.FilesystemLabel)
			return err
		}
		part.Path = device
	}
	err = cfg.Mounter.Mount(part.Path, part.MountPoint, "auto", opts)
	if err != nil {
		cfg.Logger.Errorf("Failed mounting device %s with label %s", part.Path, part.FilesystemLabel)
		return err
	}
	return nil
}

// UnmountPartition unmounts the given partition or does nothing if not mounted
func UnmountPartition(cfg *v1.Config, part *v1.Partition) error {
	if mnt, _ := utils.IsMounted(cfg, part); !mnt {
		cfg.Logger.Debugf("Not unmounting partition, %s doesn't look like mountpoint", part.MountPoint)
		return nil
	}
	cfg.Logger.Debugf("Unmounting partition %s", part.FilesystemLabel)
	return cfg.Mounter.Unmount(part.MountPoint)
}

// TODO delete method
// MountImage mounts an image with the given mount options
func (e Elemental) MountImage(img *v1.Image, opts ...string) error {
	return MountFileSystemImage(e.config, img, opts...)
}

// TODO delete method
// UnmountImage unmounts the given image or does nothing if not mounted
func (e Elemental) UnmountImage(img *v1.Image) error {
	return UmountFileSystemImage(e.config, img)
}

// TODO delete method
// CreateFileSystemImage creates the image file for the given image
func (e Elemental) CreateFileSystemImage(img *v1.Image) error {
	return CreateFileSystemImage(e.config, img, "", false)
}

// TODO delete method
// CreatePreLoadedFileSystemImage creates the image file for the given image including the contents of the rootDir.
// If rootDir is empty it simply creates an empty filesystem image
func (e Elemental) CreatePreLoadedFileSystemImage(img *v1.Image, rootDir string) error {
	preload, _ := utils.Exists(e.config.Fs, rootDir)
	return CreateFileSystemImage(e.config, img, rootDir, preload)
}

// DeployImgTree will deploy the given image into the given root tree. Returns source metadata in info,
// a tree cleaner function and error. The given root will be a bind mount of a temporary directory into the same
// filesystem of img.File, this is helpful to make the deployment easily accessible in after-* hooks.
func (e *Elemental) DeployImgTree(img *v1.Image, root string) (info interface{}, cleaner func() error, err error) {
	// We prepare the rootTree next to the target image file, in the same base path
	e.config.Logger.Infof("Preparing root tree for image: %s", img.File)
	tmp := strings.TrimSuffix(img.File, filepath.Ext(img.File))
	tmp += ".imgTree"
	err = utils.MkdirAll(e.config.Fs, tmp, cnst.DirPerm)
	if err != nil {
		return nil, nil, err
	}

	err = utils.MkdirAll(e.config.Fs, root, cnst.DirPerm)
	if err != nil {
		_ = e.config.Fs.RemoveAll(tmp)
		return nil, nil, err
	}
	err = e.config.Mounter.Mount(tmp, root, "bind", []string{"bind"})
	if err != nil {
		_ = e.config.Fs.RemoveAll(tmp)
		_ = e.config.Fs.RemoveAll(root)
		return nil, nil, err
	}

	cleaner = func() error {
		_ = e.config.Mounter.Unmount(root)
		err := e.config.Fs.RemoveAll(root)
		if err != nil {
			return err
		}
		return e.config.Fs.RemoveAll(tmp)
	}

	info, err = e.DumpSource(root, img.Source)
	if err != nil {
		_ = cleaner()
		return nil, nil, err
	}
	err = utils.CreateDirStructure(e.config.Fs, root)
	if err != nil {
		_ = cleaner()
		return nil, nil, err
	}

	return info, cleaner, err
}

// CreateImgFromTree creates the given image from with the contents of the tree for the given root.
// NoMount flag allows formatting an image including its contents (experimental and ext* specific)
func (e *Elemental) CreateImgFromTree(root string, img *v1.Image, noMount bool, cleaner func() error) (err error) {
	if cleaner != nil {
		defer func() {
			cErr := cleaner()
			if cErr != nil && err == nil {
				err = cErr
			}
		}()
	}

	err = CreateImageFromTree(e.config, img, root, noMount)

	return err
}

// CopyFileImg copies the files target as the source of this image. It also applies the img label over the copied image.
func (e *Elemental) CopyFileImg(img *v1.Image) error {
	if !img.Source.IsFile() {
		return fmt.Errorf("Copying a file image requires an image source of file type")
	}

	err := utils.MkdirAll(e.config.Fs, filepath.Dir(img.File), cnst.DirPerm)
	if err != nil {
		return err
	}

	e.config.Logger.Infof("Copying image %s to %s", img.Source.Value(), img.File)
	err = utils.CopyFile(e.config.Fs, img.Source.Value(), img.File)
	if err != nil {
		return err
	}

	if img.FS != cnst.SquashFs && img.Label != "" {
		e.config.Logger.Infof("Setting label: %s ", img.Label)
		_, err = e.config.Runner.Run("tune2fs", "-L", img.Label, img.File)
	}
	return err
}

// DeployImage will deploy the given image into the target. This method
// creates the filesystem image file and fills it with the correspondant data
func (e *Elemental) DeployImage(img *v1.Image) (interface{}, error) {
	e.config.Logger.Infof("Deploying image: %s", img.File)
	info, cleaner, err := e.DeployImgTree(img, cnst.WorkingImgDir)
	if err != nil {
		return nil, err
	}

	err = e.CreateImgFromTree(cnst.WorkingImgDir, img, false, cleaner)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// DumpSource sets the image data according to the image source type
func (e *Elemental) DumpSource(target string, imgSrc *v1.ImageSource) (info interface{}, err error) { // nolint:gocyclo
	e.config.Logger.Infof("Copying %s source...", imgSrc.Value())

	err = utils.MkdirAll(e.config.Fs, target, cnst.DirPerm)
	if err != nil {
		e.config.Logger.Errorf("failed to create target directory %s", target)
		return nil, err
	}

	if imgSrc.IsImage() {
		if e.config.Cosign {
			e.config.Logger.Infof("Running cosing verification for %s", imgSrc.Value())
			out, err := utils.CosignVerify(
				e.config.Fs, e.config.Runner, imgSrc.Value(),
				e.config.CosignPubKey, v1.IsDebugLevel(e.config.Logger),
			)
			if err != nil {
				e.config.Logger.Errorf("Cosign verification failed: %s", out)
				return nil, err
			}
		}

		err = e.config.ImageExtractor.ExtractImage(imgSrc.Value(), target, e.config.Platform.String(), e.config.LocalImage)
		if err != nil {
			return nil, err
		}
	} else if imgSrc.IsDir() {
		excludes := []string{"/mnt", "/proc", "/sys", "/dev", "/tmp", "/host", "/run"}
		err = utils.SyncData(e.config.Logger, e.config.Runner, e.config.Fs, imgSrc.Value(), target, excludes...)
		if err != nil {
			return nil, err
		}
	} else if imgSrc.IsFile() {
		err = utils.MkdirAll(e.config.Fs, cnst.ImgSrcDir, cnst.DirPerm)
		if err != nil {
			return nil, err
		}
		img := &v1.Image{File: imgSrc.Value(), MountPoint: cnst.ImgSrcDir}
		err = e.MountImage(img, "auto", "ro")
		if err != nil {
			return nil, err
		}
		defer e.UnmountImage(img) // nolint:errcheck
		excludes := []string{"/mnt", "/proc", "/sys", "/dev", "/tmp", "/host", "/run"}
		err = utils.SyncData(e.config.Logger, e.config.Runner, e.config.Fs, cnst.ImgSrcDir, target, excludes...)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("unknown image source type")
	}
	e.config.Logger.Infof("Finished copying %s into %s", imgSrc.Value(), target)
	return info, nil
}

// CopyCloudConfig will check if there is a cloud init in the config and store it on the target
func CopyCloudConfig(cfg *v1.Config, path string, cloudInit []string) (err error) {
	if path == "" {
		cfg.Logger.Warnf("empty path. Will not copy cloud config files.")
		return nil
	}
	for i, ci := range cloudInit {
		customConfig := filepath.Join(path, fmt.Sprintf("9%d_custom.yaml", i))
		err = utils.GetSource(cfg, ci, customConfig)
		if err != nil {
			return err
		}
		if err = cfg.Fs.Chmod(customConfig, cnst.FilePerm); err != nil {
			return err
		}
		cfg.Logger.Infof("Finished copying cloud config file %s to %s", cloudInit, customConfig)
	}
	return nil
}

// SelinuxRelabel will relabel the system if it finds the binary and the context
func SelinuxRelabel(cfg *v1.Config, rootDir string, raiseError bool) error {
	policyFile, err := utils.FindFile(cfg.Fs, rootDir, filepath.Join(cnst.SELinuxTargetedPolicyPath, "policy.*"))
	contextFile := filepath.Join(rootDir, cnst.SELinuxTargetedContextFile)
	contextExists, _ := utils.Exists(cfg.Fs, contextFile)

	if err == nil && contextExists && cfg.Runner.CommandExists("setfiles") {
		var out []byte
		var err error
		if rootDir == "/" || rootDir == "" {
			out, err = cfg.Runner.Run("setfiles", "-c", policyFile, "-e", "/dev", "-e", "/proc", "-e", "/sys", "-F", contextFile, "/")
		} else {
			out, err = cfg.Runner.Run("setfiles", "-c", policyFile, "-F", "-r", rootDir, contextFile, rootDir)
		}
		cfg.Logger.Debugf("SELinux setfiles output: %s", string(out))
		if err != nil && raiseError {
			return err
		}
	} else {
		cfg.Logger.Debugf("No files relabelling as SELinux utilities are not found")
	}

	return nil
}

// CheckActiveDeployment returns true if at least one of the mode sentinel files is found
func (e *Elemental) CheckActiveDeployment() bool {
	e.config.Logger.Infof("Checking for active deployment")

	sentinels := []string{cnst.ActiveMode, cnst.PassiveMode, cnst.RecoveryMode}
	for _, s := range sentinels {
		if ok, _ := utils.Exists(e.config.Fs, s); ok {
			return true
		}
	}

	return false
}

// SourceISO downloads an ISO in a temporary folder, mounts it and returns the image source to be used
// Returns a source and cleaner method to unmount and remove the temporary folder afterwards.
func SourceFormISO(cfg *v1.Config, iso string) (*v1.ImageSource, func() error, error) {
	nilErr := func() error { return nil }

	tmpDir, err := utils.TempDir(cfg.Fs, "", "elemental")
	if err != nil {
		return nil, nilErr, err
	}

	cleanTmpDir := func() error { return cfg.Fs.RemoveAll(tmpDir) }

	tmpFile := filepath.Join(tmpDir, "elemental.iso")
	err = utils.GetSource(cfg, iso, tmpFile)
	if err != nil {
		return nil, cleanTmpDir, err
	}

	isoMnt := filepath.Join(tmpDir, "iso")
	err = utils.MkdirAll(cfg.Fs, isoMnt, cnst.DirPerm)
	if err != nil {
		return nil, cleanTmpDir, err
	}

	cfg.Logger.Infof("Mounting iso %s into %s", tmpFile, isoMnt)
	err = cfg.Mounter.Mount(tmpFile, isoMnt, "auto", []string{"loop"})
	if err != nil {
		return nil, cleanTmpDir, err
	}

	cleanAll := func() error {
		cErr := cfg.Mounter.Unmount(isoMnt)
		if cErr != nil {
			return cErr
		}
		return cleanTmpDir()
	}

	squashfsImg := filepath.Join(isoMnt, cnst.ISORootFile)
	ok, _ := utils.Exists(cfg.Fs, squashfsImg)
	if !ok {
		return nil, cleanAll, fmt.Errorf("squashfs image not found in ISO: %s", squashfsImg)
	}

	return v1.NewFileSrc(squashfsImg), cleanAll, nil
}

// DeactivateDevice deactivates unmounted the block devices present within the system.
// Useful to deactivate LVM volumes, if any, related to the target device.
func (e Elemental) DeactivateDevices() error {
	out, err := e.config.Runner.Run(
		"blkdeactivate", "--lvmoptions", "retry,wholevg",
		"--dmoptions", "force,retry", "--errors",
	)
	e.config.Logger.Debugf("blkdeactivate command output: %s", string(out))
	return err
}

// NEW METHODS

func CreateImageFromTree(cfg *v1.Config, img *v1.Image, rootDir string, preload bool) (err error) {
	if img.FS == cnst.SquashFs {
		cfg.Logger.Infof("Creating squashfs image for file %s", img.File)

		squashOptions := append(cnst.GetDefaultSquashfsOptions(), cfg.SquashFsCompressionConfig...)
		err = utils.CreateSquashFS(cfg.Runner, cfg.Logger, rootDir, img.File, squashOptions)
		if err != nil {
			return err
		}
	} else {
		err = CreateFileSystemImage(cfg, img, rootDir, preload)
		if err != nil {
			cfg.Logger.Errorf("failed creating filesystem image: %v", err)
			return err
		}
		if !preload {
			err = MountFileSystemImage(cfg, img, "rw")
			if err != nil {
				cfg.Logger.Errorf("failed mounting filesystem image: %v", err)
				return err
			}
			defer func() {
				mErr := UmountFileSystemImage(cfg, img)
				if err == nil && mErr != nil {
					err = mErr
				}
			}()

			cfg.Logger.Infof("Sync %s to %s", rootDir, img.MountPoint)
			err = utils.SyncData(cfg.Logger, cfg.Runner, cfg.Fs, rootDir, img.MountPoint)
			if err != nil {
				return err
			}
		}
	}
	return err
}

func CreateFileSystemImage(cfg *v1.Config, img *v1.Image, rootDir string, preload bool) error {
	cfg.Logger.Infof("Creating image %s", img.File)
	if img.Size == 0 && rootDir != "" {
		size, err := utils.DirSizeMB(cfg.Fs, rootDir)
		if err != nil {
			return err
		}
		img.Size = size + cnst.ImgOverhead
		cfg.Logger.Debugf("Image size %dM", img.Size)
	}
	err := utils.CreateRAWFile(cfg.Fs, img.File, img.Size)
	if err != nil {
		cfg.Logger.Errorf("failed creating raw file %s", img.File)
		return err
	}

	extraOpts := []string{}
	r := regexp.MustCompile("ext[2-4]")
	match := r.MatchString(img.FS)
	if preload && match {
		extraOpts = []string{"-d", rootDir}
	}
	if preload && !match {
		cfg.Logger.Errorf("Preloaded filesystem images are only supported for ext2-4 filesystems")
		return fmt.Errorf("unexpected filesystem: %s", img.FS)
	}
	mkfs := partitioner.NewMkfsCall(img.File, img.FS, img.Label, cfg.Runner, extraOpts...)
	_, err = mkfs.Apply()
	if err != nil {
		cfg.Logger.Errorf("failed formatting file %s with %s", img.File, img.FS)
		_ = cfg.Fs.RemoveAll(img.File)
		return err
	}
	return nil
}

func MountFileSystemImage(cfg *v1.Config, img *v1.Image, opts ...string) error {
	cfg.Logger.Debugf("Mounting image %s to %s", img.File, img.MountPoint)
	out, err := cfg.Runner.Run("losetup", "--show", "-f", img.File)
	if err != nil {
		cfg.Logger.Errorf("failed setting a loop device for %s", img.File)
		return err
	}
	loop := strings.TrimSpace(string(out))
	cfg.Logger.Debugf("Loopdevice for %s set to %s", img.File, loop)
	err = cfg.Mounter.Mount(loop, img.MountPoint, "auto", opts)
	if err != nil {
		cfg.Logger.Errorf("failed to mount %s", loop)
		cfg.Runner.RunNoError("losetup", "-d", loop)
		return err
	}
	img.LoopDevice = loop
	return nil
}

func UmountFileSystemImage(cfg *v1.Config, img *v1.Image) error {
	if notMnt, _ := cfg.Mounter.IsLikelyNotMountPoint(img.MountPoint); notMnt {
		cfg.Logger.Debugf("Not unmounting image, %s doesn't look like mountpoint", img.MountPoint)
		return nil
	}

	loop := img.LoopDevice
	img.LoopDevice = ""

	cfg.Logger.Debugf("Unmounting image from %s", img.MountPoint)
	err := cfg.Mounter.Unmount(img.MountPoint)
	if err != nil {
		cfg.Runner.RunNoError("losetup", "-d", loop)
		return err
	}
	_, err = cfg.Runner.Run("losetup", "-d", loop)
	return err
}
