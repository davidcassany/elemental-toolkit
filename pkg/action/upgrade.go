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

package action

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/rancher/elemental-toolkit/pkg/bootloader"
	"github.com/rancher/elemental-toolkit/pkg/constants"
	"github.com/rancher/elemental-toolkit/pkg/elemental"
	elementalError "github.com/rancher/elemental-toolkit/pkg/error"
	"github.com/rancher/elemental-toolkit/pkg/snapshotter"
	v1 "github.com/rancher/elemental-toolkit/pkg/types/v1"
	"github.com/rancher/elemental-toolkit/pkg/utils"
)

// UpgradeAction represents the struct that will run the upgrade from start to finish
type UpgradeAction struct {
	config      *v1.RunConfig
	spec        *v1.UpgradeSpec
	bootloader  v1.Bootloader
	snapshotter v1.Snapshotter
}

type UpgradeActionOption func(r *UpgradeAction) error

func WithUpgradeBootloader(bootloader v1.Bootloader) func(u *UpgradeAction) error {
	return func(u *UpgradeAction) error {
		u.bootloader = bootloader
		return nil
	}
}

func NewUpgradeAction(config *v1.RunConfig, spec *v1.UpgradeSpec, opts ...UpgradeActionOption) *UpgradeAction {
	u := &UpgradeAction{config: config, spec: spec}

	for _, o := range opts {
		err := o(u)
		if err != nil {
			config.Logger.Errorf("error applying config option: %s", err.Error())
			return nil
		}
	}

	if u.bootloader == nil {
		u.bootloader = bootloader.NewGrub(&config.Config)
	}

	if u.snapshotter == nil {
		u.snapshotter = snapshotter.NewLoopDeviceSnapshotter(&config.Config, spec.SnapshotterCfg, u.bootloader)
	}

	return u
}

func (u UpgradeAction) Info(s string, args ...interface{}) {
	u.config.Logger.Infof(s, args...)
}

func (u UpgradeAction) Debug(s string, args ...interface{}) {
	u.config.Logger.Debugf(s, args...)
}

func (u UpgradeAction) Error(s string, args ...interface{}) {
	u.config.Logger.Errorf(s, args...)
}

func (u UpgradeAction) upgradeHook(hook string) error {
	u.Info("Applying '%s' hook", hook)
	return Hook(&u.config.Config, hook, u.config.Strict, u.config.CloudInitPaths...)
}

func (u UpgradeAction) upgradeChrootHook(hook string, root string) error {
	u.Info("Applying '%s' hook", hook)
	mountPoints := map[string]string{}

	oemDevice := u.spec.Partitions.OEM
	if oemDevice != nil && oemDevice.MountPoint != "" {
		mountPoints[oemDevice.MountPoint] = constants.OEMPath
	}

	persistentDevice := u.spec.Partitions.Persistent
	if persistentDevice != nil && persistentDevice.MountPoint != "" {
		mountPoints[persistentDevice.MountPoint] = constants.UsrLocalPath
	}

	return ChrootHook(&u.config.Config, hook, u.config.Strict, root, mountPoints, u.config.CloudInitPaths...)
}

func (u *UpgradeAction) upgradeInstallStateYaml(meta interface{}, img v1.Image) error {
	if u.spec.Partitions.Recovery == nil || u.spec.Partitions.State == nil {
		return fmt.Errorf("undefined state or recovery partition")
	}

	if u.spec.State == nil {
		u.spec.State = &v1.InstallState{
			Partitions: map[string]*v1.PartitionState{},
		}
	}

	u.spec.State.Date = time.Now().Format(time.RFC3339)
	imgState := &v1.ImageState{
		Source:         img.Source,
		SourceMetadata: meta,
		Label:          img.Label,
		FS:             img.FS,
	}
	if u.spec.RecoveryUpgrade {
		recoveryPart := u.spec.State.Partitions[constants.RecoveryPartName]
		if recoveryPart == nil {
			recoveryPart = &v1.PartitionState{
				Images:  map[string]*v1.ImageState{},
				FSLabel: u.spec.Partitions.Recovery.FilesystemLabel,
			}
			u.spec.State.Partitions[constants.RecoveryPartName] = recoveryPart
		}
		recoveryPart.Images[constants.RecoveryImgName] = imgState
	} else {
		statePart := u.spec.State.Partitions[constants.StatePartName]
		if statePart == nil {
			statePart = &v1.PartitionState{
				Images:  map[string]*v1.ImageState{},
				FSLabel: u.spec.Partitions.State.FilesystemLabel,
			}
			u.spec.State.Partitions[constants.StatePartName] = statePart
		}
		statePart.Images[constants.ActiveImgName] = imgState
	}

	return u.config.WriteInstallState(
		u.spec.State,
		filepath.Join(u.spec.Partitions.State.MountPoint, constants.InstallStateFile),
		filepath.Join(u.spec.Partitions.Recovery.MountPoint, constants.InstallStateFile),
	)
}

func (u *UpgradeAction) Run() (err error) {
	var activeSnap *v1.Snapshot
	var upgradeImg v1.Image
	var finalImageFile string
	var treeCleaner func() error
	var upgradeMeta interface{}

	cleanup := utils.NewCleanStack()
	defer func() {
		err = cleanup.Cleanup(err)
	}()

	upgradeImg = u.spec.Recovery
	finalImageFile = filepath.Join(u.spec.Partitions.Recovery.MountPoint, constants.RecoveryImgFile)

	err = u.mountPartitions(cleanup)
	if err != nil {
		u.Error("failed mounting partitions: %v", err)
		return err
	}

	if !u.spec.RecoveryUpgrade {
		err = u.snapshotter.InitSnapshotter(u.spec.Partitions.State.MountPoint)
		if err != nil {
			// TODO add init snaphotter error
			return err
		}
	}

	// before upgrade hook happens once partitions are RW mounted, just before image OS is deployed
	err = u.upgradeHook(constants.BeforeUpgradeHook)
	if err != nil {
		u.Error("Error while running hook before-upgrade: %s", err)
		return elementalError.NewFromError(err, elementalError.HookBeforeUpgrade)
	}

	if !u.spec.RecoveryUpgrade {
		activeSnap, err = u.snapshotter.StartTransaction()
		if err != nil {
			u.Error("failed starting snapshot transaction: %v", err)
			return err
		}
		cleanup.PushErrorOnly(func() error { return u.snapshotter.CloseTransactionOnError(activeSnap) })

		// Deploy active image
		upgradeMeta, err = elemental.DumpSource(&u.config.Config, activeSnap.WorkDir, u.spec.Active)
		if err != nil {
			u.Error("failed extracting %s image: %v", u.spec.Active.String(), err)
			return elementalError.NewFromError(err, elementalError.DeployImgTree)
		}
	} else {
		// Deploy recovery image
		u.Info("deploying image %s to %s", upgradeImg.Source.Value(), upgradeImg.File)
		upgradeMeta, treeCleaner, err = elemental.DeployImgTree(&u.config.Config, &upgradeImg, constants.WorkingImgDir)
		if err != nil {
			u.Error("Failed deploying image to file '%s': %s", upgradeImg.File, err)
			return elementalError.NewFromError(err, elementalError.DeployImgTree)
		}
		cleanup.Push(func() error { return treeCleaner() })
	}

	err = u.refineDeployment()
	if err != nil {
		u.Error("failed refining dumped OS tree: %v", err)
		return err
	}

	if !u.spec.RecoveryUpgrade {
		err = u.snapshotter.CloseTransaction(activeSnap)
		if err != nil {
			u.Error("failed closing snapshot transaction: %v", err)
			return err
		}
	} else {
		err = elemental.CreateImgFromTree(&u.config.Config, constants.WorkingImgDir, &upgradeImg, false, treeCleaner)
		if err != nil {
			u.Error("failed creating transition image")
			return elementalError.NewFromError(err, elementalError.CreateImgFromTree)
		}

		// TODO change mv call to fs.Rename
		u.Info("Moving %s to %s", upgradeImg.File, finalImageFile)
		_, err = u.config.Runner.Run("mv", "-f", upgradeImg.File, finalImageFile)
		if err != nil {
			u.Error("Failed to move %s to %s: %s", upgradeImg.File, finalImageFile, err)
			return elementalError.NewFromError(err, elementalError.MoveFile)
		}
		u.Info("Finished moving %s to %s", upgradeImg.File, finalImageFile)
	}

	err = u.upgradeHook(constants.PostUpgradeHook)
	if err != nil {
		u.Error("Error running hook post-upgrade: %s", err)
		return elementalError.NewFromError(err, elementalError.HookPostUpgrade)
	}

	if !u.spec.RecoveryUpgrade {
		// Update state.yaml file on recovery and state partitions
		err = u.upgradeInstallStateYaml(upgradeMeta, v1.Image{
			File:   activeSnap.Path,
			Label:  activeSnap.Label,
			Size:   u.spec.SnapshotterCfg.Size,
			FS:     u.spec.SnapshotterCfg.FS,
			Source: u.spec.Active,
		})
		if err != nil {
			u.Error("failed upgrading installation metadata")
			return err
		}
	} else {
		// Update state.yaml file on recovery and state partitions
		err = u.upgradeInstallStateYaml(upgradeMeta, upgradeImg)
		if err != nil {
			u.Error("failed upgrading installation metadata")
			return err
		}
	}

	u.Info("Upgrade completed")

	// Do not reboot/poweroff on cleanup errors
	err = cleanup.Cleanup(err)
	if err != nil {
		return elementalError.NewFromError(err, elementalError.Cleanup)
	}

	return PowerAction(u.config)
}

func (u *UpgradeAction) refineDeployment() error {
	// Relabel SELinux
	err := applySelinuxLabels(&u.config.Config, u.spec.Partitions)
	if err != nil {
		u.Error("failed setting SELiux labels")
		return elementalError.NewFromError(err, elementalError.SelinuxRelabel)
	}

	err = u.upgradeChrootHook(constants.AfterUpgradeChrootHook, constants.WorkingImgDir)
	if err != nil {
		u.Error("Error running hook after-upgrade-chroot: %s", err)
		return elementalError.NewFromError(err, elementalError.HookAfterUpgradeChroot)
	}
	err = u.upgradeHook(constants.AfterUpgradeHook)
	if err != nil {
		u.Error("Error running hook after-upgrade: %s", err)
		return elementalError.NewFromError(err, elementalError.HookAfterUpgrade)
	}

	grubVars := u.spec.GetGrubLabels()
	err = u.bootloader.SetPersistentVariables(
		filepath.Join(u.spec.Partitions.State.MountPoint, constants.GrubOEMEnv),
		grubVars,
	)
	if err != nil {
		u.Error("Error setting GRUB labels: %s", err)
		return elementalError.NewFromError(err, elementalError.SetGrubVariables)
	}

	u.Info("rebranding")
	err = u.bootloader.SetDefaultEntry(u.spec.Partitions.State.MountPoint, constants.WorkingImgDir, u.spec.GrubDefEntry)
	if err != nil {
		u.Error("failed setting default entry")
		return elementalError.NewFromError(err, elementalError.SetDefaultGrubEntry)
	}

	return nil
}

func (u *UpgradeAction) mountPartitions(cleanup *utils.CleanStack) error {
	umount, err := elemental.MountRWPartition(&u.config.Config, u.spec.Partitions.State)
	if err != nil {
		return elementalError.NewFromError(err, elementalError.MountStatePartition)
	}
	cleanup.Push(umount)
	umount, err = elemental.MountRWPartition(&u.config.Config, u.spec.Partitions.Recovery)
	if err != nil {
		return elementalError.NewFromError(err, elementalError.MountRecoveryPartition)
	}
	cleanup.Push(umount)

	// Recovery does not mount persistent, so try to mount it. Ignore errors, as it's not mandatory.
	persistentPart := u.spec.Partitions.Persistent
	if persistentPart != nil {
		// Create the dir otherwise the check for mounted dir fails
		_ = utils.MkdirAll(u.config.Fs, persistentPart.MountPoint, constants.DirPerm)
		if mnt, err := utils.IsMounted(&u.config.Config, persistentPart); !mnt && err == nil {
			u.Debug("mounting persistent partition")
			umount, err = elemental.MountRWPartition(&u.config.Config, persistentPart)
			if err != nil {
				u.config.Logger.Warn("could not mount persistent partition: %s", err.Error())
			} else {
				cleanup.Push(umount)
			}
		}
	}
	return nil
}

// remove attempts to remove the given path. Does nothing if it doesn't exist
func (u *UpgradeAction) remove(path string) error {
	if exists, _ := utils.Exists(u.config.Fs, path); exists {
		u.Debug("[Cleanup] Removing %s", path)
		return u.config.Fs.RemoveAll(path)
	}
	return nil
}
