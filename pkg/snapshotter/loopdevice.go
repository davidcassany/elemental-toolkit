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

package snapshotter

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/rancher/elemental-toolkit/pkg/constants"
	"github.com/rancher/elemental-toolkit/pkg/elemental"

	v1 "github.com/rancher/elemental-toolkit/pkg/types/v1"
	"github.com/rancher/elemental-toolkit/pkg/utils"
)

const (
	loopDeviceSnapsPath    = ".snapshots"
	loopDeviceImgName      = "snap.img"
	loopDeviceWorkDir      = "snap.workDir"
	loopDeviceLabelPattern = "EL_SNAP%d"
)

var _ v1.Snapshotter = (*LoopDevice)(nil)

type LoopDevice struct {
	cfg     *v1.Config
	snapCfg *v1.SnapshotterConfig
	rootDir string
}

func NewLoopDeviceSnapshotter(cfg *v1.Config, snapCfg *v1.SnapshotterConfig) *LoopDevice {
	return &LoopDevice{cfg: cfg, snapCfg: snapCfg}
}

func (l *LoopDevice) InitSnapshotter(rootDir string) error {
	l.cfg.Logger.Infof("Initiating a LoopDevice snapshotter at %s", rootDir)
	l.rootDir = rootDir
	return utils.MkdirAll(l.cfg.Fs, filepath.Join(rootDir, loopDeviceSnapsPath), constants.DirPerm)
}

func (l *LoopDevice) StartTransaction() (*v1.Snapshot, error) {
	var snap *v1.Snapshot

	l.cfg.Logger.Infof("Starting a snapshotter transaction")
	nextID, err := l.getNextSnapID()
	if err != nil {
		return nil, err
	}

	l.cfg.Logger.Debugf("Snapshot ID set to %d", nextID)

	snapPath := filepath.Join(l.rootDir, loopDeviceSnapsPath, strconv.FormatInt(int64(nextID), 10))
	err = utils.MkdirAll(l.cfg.Fs, snapPath, constants.DirPerm)
	if err != nil {
		_ = l.cfg.Fs.RemoveAll(snapPath)
		return nil, err
	}

	workDir := filepath.Join(snapPath, loopDeviceWorkDir)
	err = utils.MkdirAll(l.cfg.Fs, workDir, constants.DirPerm)
	if err != nil {
		_ = l.cfg.Fs.RemoveAll(snapPath)
		return nil, err
	}

	err = utils.MkdirAll(l.cfg.Fs, constants.WorkingImgDir, constants.DirPerm)
	if err != nil {
		_ = l.cfg.Fs.RemoveAll(snapPath)
		return nil, err
	}

	err = l.cfg.Mounter.Mount(workDir, constants.WorkingImgDir, "bind", []string{"bind"})
	if err != nil {
		_ = l.cfg.Fs.RemoveAll(snapPath)
		_ = l.cfg.Fs.RemoveAll(constants.WorkingImgDir)
		return nil, err
	}

	snap = &v1.Snapshot{
		ID:         nextID,
		Path:       filepath.Join(snapPath, loopDeviceImgName),
		WorkDir:    workDir,
		MountPoint: constants.WorkingImgDir,
		Label:      fmt.Sprintf(loopDeviceLabelPattern, nextID),
		InProgress: true,
	}

	l.cfg.Logger.Infof("Transaction for snapshot %d successfully started", nextID)
	return snap, nil
}

func (l *LoopDevice) CloseTransactionOnError(snap *v1.Snapshot) error {
	var err, rErr error

	if snap.InProgress {
		err = l.cfg.Mounter.Unmount(snap.MountPoint)
	}

	rErr = l.cfg.Fs.RemoveAll(filepath.Dir(snap.Path))
	if rErr != nil && err == nil {
		err = rErr
	}
	return err
}

func (l *LoopDevice) CloseTransaction(snap *v1.Snapshot) (err error) {
	if !snap.InProgress {
		l.cfg.Logger.Debugf("No transaction to close for snapshot %d workdir", snap.ID)
		return nil
	}
	defer func() {
		if err != nil {
			_ = l.cfg.Fs.RemoveAll(filepath.Dir(snap.Path))
		}
	}()

	l.cfg.Logger.Infof("Closing transaction for snapshot %d workdir", snap.ID)
	l.cfg.Logger.Debugf("Unmount %s", constants.WorkingImgDir)
	err = l.cfg.Mounter.Unmount(snap.MountPoint)
	if err != nil {
		l.cfg.Logger.Errorf("failed umounting snapshot %d workdir bind mount", snap.ID)
		return err
	}

	err = elemental.CreateImageFromTree(l.cfg, l.snapshotToImage(snap), snap.WorkDir, false)
	if err != nil {
		l.cfg.Logger.Errorf("failed creating image for snapshot %d: %v", snap.ID, err)
		return err
	}

	err = l.cfg.Fs.RemoveAll(snap.WorkDir)
	if err != nil {
		return err
	}

	activeImg := filepath.Join(l.rootDir, loopDeviceSnapsPath, constants.ActiveImgFile)
	activeImgBak := activeImg + ".bk"
	if exists, _ := utils.Exists(l.cfg.Fs, activeImg); exists {
		err = l.cfg.Fs.Rename(activeImg, activeImgBak)
		if err != nil {
			return err
		}
	}

	l.cfg.Fs.Symlink(fmt.Sprintf("%d/%s", snap.ID, loopDeviceImgName), filepath.Join(l.rootDir, loopDeviceSnapsPath, constants.ActiveImgFile))
	if err != nil {
		l.cfg.Logger.Errorf("failed default snapshot image for snapshot %d: %v", snap.ID, err)
		_ = l.cfg.Fs.Rename(activeImgBak, activeImg)
		return err
	}
	_ = l.cfg.Fs.Remove(activeImgBak)

	snap.InProgress = false
	return err
}

func (l *LoopDevice) snapshotToImage(snap *v1.Snapshot) *v1.Image {
	return &v1.Image{
		File:       snap.Path,
		Label:      snap.Label,
		Size:       l.snapCfg.Size,
		FS:         l.snapCfg.FS,
		MountPoint: snap.MountPoint,
	}
}

func (l *LoopDevice) getNextSnapID() (int, error) {
	var snapID, max int64

	snapsPath := filepath.Join(l.rootDir, loopDeviceSnapsPath)
	r := regexp.MustCompile(`\d`)
	if ok, _ := utils.Exists(l.cfg.Fs, snapsPath); ok {
		dirs, err := l.cfg.Fs.ReadDir(snapsPath)
		if err != nil {
			return -1, err
		}
		for _, d := range dirs {
			// Find next snapshot ID based on directory names
			if !d.IsDir() || !r.MatchString(d.Name()) {
				continue
			}
			snapID, err = strconv.ParseInt(d.Name(), 10, 32)
			if err != nil {
				continue
			}
			if snapID > max {
				max = snapID
			}
		}
		return int(max + 1), nil
	}
	return -1, fmt.Errorf("cannot determine next snapshot ID, initate snapshotter first")
}
