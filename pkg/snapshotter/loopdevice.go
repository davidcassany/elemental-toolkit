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
	"bufio"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

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
	// From now on we do not error out as the transaction is already done, cleanup steps are only logged
	_ = l.cfg.Fs.Remove(activeImgBak)
	_ = l.cleanOldSnaps()

	snap.InProgress = false
	return err
}

func (l *LoopDevice) DeleteSnapshot(id int) error {
	inUse, err := l.isSnapInUse(id)
	if err != nil {
		return err
	}

	if inUse {
		return fmt.Errorf("cannot delete a snapshot that is currently in use")
	}

	return l.cfg.Fs.RemoveAll(filepath.Join(l.rootDir, loopDeviceSnapsPath, strconv.Itoa(id)))
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

func (l *LoopDevice) cleanOldSnaps() error {
	ids, err := l.getSnaps()
	if err != nil {
		l.cfg.Logger.Warnf("could not get current snapshots")
		return err
	}

	sort.Ints(ids)
	for len(ids) > l.snapCfg.MaxSnaps {
		err = l.DeleteSnapshot(ids[0])
		if err != nil {
			l.cfg.Logger.Warnf("could not delete snapshot %d", ids[0])
			return err
		}
		ids = ids[1:]
	}
	return nil
}

func (l *LoopDevice) getNextSnapID() (int, error) {
	var id int

	ids, err := l.getSnaps()
	if err != nil {
		return -1, err
	}
	for _, i := range ids {
		if i > id {
			id = i
		}
	}
	return id + 1, nil
}

func (l *LoopDevice) isSnapInUse(id int) (bool, error) {
	backedFiles, err := l.cfg.Runner.Run("losetup", "-ln", "--output", "BACK-FILE")
	if err != nil {
		return false, err
	}

	scanner := bufio.NewScanner(strings.NewReader(strings.TrimSpace(string(backedFiles))))
	for scanner.Scan() {
		backedFile := scanner.Text()
		suffix := filepath.Join(loopDeviceSnapsPath, strconv.Itoa(id), loopDeviceImgName)
		if strings.HasSuffix(backedFile, suffix) {
			return true, nil
		}
	}
	return false, nil
}

func (l *LoopDevice) getCurrentSnap() (int, error) {

	ids, err := l.getSnaps()
	if err != nil {
		return -1, err
	}

	for _, id := range ids {
		if inUse, _ := l.isSnapInUse(id); inUse {
			return id, nil
		}
	}
	return 0, nil
}

func (l *LoopDevice) getSnaps() ([]int, error) {
	var ids []int

	snapsPath := filepath.Join(l.rootDir, loopDeviceSnapsPath)
	r := regexp.MustCompile(`\d`)
	if ok, _ := utils.Exists(l.cfg.Fs, snapsPath); ok {
		dirs, err := l.cfg.Fs.ReadDir(snapsPath)
		if err != nil {
			l.cfg.Logger.Errorf("failed reading %s contents", snapsPath)
			return ids, err
		}
		for _, d := range dirs {
			// Find snapshots based numeric directory names
			if !d.IsDir() || !r.MatchString(d.Name()) {
				continue
			}
			id, err := strconv.ParseInt(d.Name(), 10, 32)
			if err != nil {
				continue
			}
			ids = append(ids, int(id))
		}
		return ids, nil
	}
	l.cfg.Logger.Errorf("path %s does not exist", snapsPath)
	return ids, fmt.Errorf("cannot determine current snapshots, initate snapshotter first")
}
