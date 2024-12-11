/*
Copyright © 2022 - 2024 SUSE LLC

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
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/davidcassany/ocistore/pkg/ocistore"
	"github.com/rancher/elemental-toolkit/v2/pkg/constants"
	"github.com/rancher/elemental-toolkit/v2/pkg/elemental"
	"github.com/rancher/elemental-toolkit/v2/pkg/types"
	"github.com/rancher/elemental-toolkit/v2/pkg/utils"
)

const (
	errMsg         = "not implemented yet"
	containerdPath = ".containerd"
	// TODO rethink this naming
	activeLabel    = "elemental-toolkit.io/image.active"
	snapshotLabel  = "elemental-toolkit.io/image.elemental.id"
	scratchImgName = "local/elemental/fromscratch"
)

var _ types.Snapshotter = (*Containerd)(nil)

type Containerd struct {
	cfg            types.Config
	snapshotterCfg types.SnapshotterConfig
	containerdCfg  types.ContainerdConfig
	rootDir        string
	efiDir         string
	activeID       int
	currentID      int
	activeImgRef   string
	bootloader     types.Bootloader
	containerd     ocistore.OCIStore
	imgSrc         *types.ImageSource
}

func newContainerdSnapshotter(cfg types.Config, snapCfg types.SnapshotterConfig, bootloader types.Bootloader) (types.Snapshotter, error) {
	if snapCfg.Type != constants.ContainerdSnapshotterType {
		msg := "invalid snapshotter type ('%s'), must be of '%s' type"
		cfg.Logger.Errorf(msg, snapCfg.Type, constants.ContainerdSnapshotterType)
		return nil, fmt.Errorf(msg, snapCfg.Type, constants.ContainerdSnapshotterType)
	}
	var containerdCfg *types.ContainerdConfig
	var ok bool
	if snapCfg.Config == nil {
		containerdCfg = types.NewContainerdConfig()
	} else {
		containerdCfg, ok = snapCfg.Config.(*types.ContainerdConfig)
		if !ok {
			msg := "failed casting ContainerdConfig type"
			cfg.Logger.Errorf(msg)
			return nil, fmt.Errorf("%s", msg)
		}
	}
	return &Containerd{cfg: cfg, snapshotterCfg: snapCfg, containerdCfg: *containerdCfg, bootloader: bootloader}, nil
}

func (c *Containerd) InitSnapshotter(state *types.Partition, efiDir string) error {
	c.rootDir = state.MountPoint
	c.efiDir = efiDir
	c.containerd = ocistore.NewOCIStore(c.cfg.Logger, filepath.Join(state.MountPoint, containerdPath))
	err := c.containerd.Init(context.Background())
	if err != nil {
		c.cfg.Logger.Errorf("failed initating containerdOSStore: %v", err)
		return err
	}
	// Check we can read images and look for active and current if any
	_, err = c.getSnapshots()
	if err != nil {
		c.cfg.Logger.Errorf("could not find current active snapshot: %v", err)
		return err
	}
	return nil
}

func (c *Containerd) StartTransaction(src *types.ImageSource) (*types.Snapshot, error) {
	var img client.Image
	var key string
	err := utils.MkdirAll(c.cfg.Fs, constants.WorkingImgDir, constants.DirPerm)
	if err != nil {
		c.cfg.Logger.Errorf("failed creating working directory: %v", err)
		return nil, err
	}

	nextID, err := c.getNextSnapshotID()
	if err != nil {
		c.cfg.Logger.Errorf("transaction failed, cannot proceed without a new ID: %v", err)
		return nil, err
	}

	delImg := func() {
		dErr := c.containerd.Delete(img.Name())
		if dErr != nil {
			c.cfg.Logger.Warnf("could not cleanup image '%s': %v", img.Name(), err)
		}
	}

	umntSnap := func(uKey string) {
		uErr := c.containerd.Umount(constants.WorkingImgDir, uKey, -1)
		if uErr != nil {
			c.cfg.Logger.Warnf("cound not umount and/or delete active snapshot: %v", uErr)
		}
	}

	if src.IsImage() {
		img, err = c.containerd.Pull(src.Value())
		if err != nil {
			c.cfg.Logger.Errorf("transaction failed, pulling image '%s' error: %v", src.Value(), err)
			return nil, err
		}
	} else if src.IsFile() {
		// TODO support vfs
		img, err = c.containerd.SingleImportFile(src.Value())
		if err != nil {
			c.cfg.Logger.Errorf("transaction failed, importing image '%s' error: %v", src.Value(), err)
			return nil, err
		}
	}

	if src.IsImage() || src.IsFile() {
		err = c.containerd.Unpack(img)
		if err != nil {
			c.cfg.Logger.Errorf("transaction failed, could not unpack image '%s': %v", img.Name(), err)
			delImg()
			return nil, err
		}

		key, err = c.containerd.Mount(img, constants.WorkingImgDir, "", false)
		if err != nil {
			c.cfg.Logger.Errorf("transaction failed, could not mount image '%s': %v", img.Name(), err)
			delImg()
			return nil, err
		}
	}

	if src.IsDir() {
		key, err = c.containerd.MountFromScratch(constants.WorkingImgDir, "")
		if err != nil {
			c.cfg.Logger.Errorf("transaction failed, could not mount image '%s': %v", img.Name(), err)
			return nil, err
		}

		excludes := constants.GetDefaultSystemRootedExcludes(src.Value())
		err = utils.MirrorData(c.cfg.Logger, c.cfg.Runner, c.cfg.Fs, src.Value(), constants.WorkingImgDir, excludes...)
		if err != nil {
			c.cfg.Logger.Errorf("transaction failed, could not sync source '%s' to workdir: %v", src.Value(), err)
			umntSnap(key)
			return nil, err
		}

		err = utils.CreateDirStructure(c.cfg.Fs, constants.WorkingImgDir)
		if err != nil {
			c.cfg.Logger.Errorf("transaction failed, could not create directories in workdir: %v", err)
			umntSnap(key)
			return nil, err
		}
	}

	snapshot := &types.Snapshot{
		ID:         nextID,
		Path:       constants.WorkingImgDir,
		WorkDir:    constants.WorkingImgDir,
		MountPoint: constants.WorkingImgDir,
		Label:      key,
		InProgress: true,
	}

	c.imgSrc = src
	return snapshot, nil
}

func (c *Containerd) CloseTransactionOnError(snapshot *types.Snapshot) error {
	err := c.containerd.Umount(snapshot.MountPoint, snapshot.Label, -1)
	if err != nil {
		c.cfg.Logger.Errorf("could not umount snapshot: %v", err)
		return err
	}

	return c.cleanupImages()
}

func (c *Containerd) CloseTransaction(snapshot *types.Snapshot) (retErr error) {
	defer func() {
		if retErr != nil {
			_ = c.CloseTransactionOnError(snapshot)
		}
	}()

	err := c.containerd.Umount(snapshot.MountPoint, snapshot.Label, 0)
	if err != nil {
		c.cfg.Logger.Errorf("transaction failed, could not umount snapshot: %v", err)
		return err
	}

	ref, err := c.setNewImageReference(snapshot.Label)
	if err != nil {
		c.cfg.Logger.Errorf("transaction failed, could not define new image ref: %v", err)
		return err
	}

	// TODO does it actually makes any sense at all? Is diff keeping track of those changes? if it does, can it duplicate
	// the whole roottree in a new layer?
	err = elemental.ApplySELinuxLabels(c.cfg, snapshot.Path, map[string]string{})
	if err != nil {
		c.cfg.Logger.Errorf("failed relabelling snapshot path: %s", snapshot.Path)
		return err
	}

	opts := ocistore.ImgOpts{
		Ref: ref,
		Labels: map[string]string{
			activeLabel:   time.Now().UTC().Format(time.RFC3339),
			snapshotLabel: strconv.Itoa(snapshot.ID),
		},
	}
	_, err = c.containerd.Commit(snapshot.Label, ocistore.WithImgCommitOpts(opts))
	if err != nil {
		c.cfg.Logger.Errorf("transaction failed, could not commit containerd snapshot: %v", err)
		return err
	}

	err = c.removeActiveLabelFromImg()
	if err != nil {
		c.cfg.Logger.Warnf("could not delete active label from '%s' image", c.activeImgRef)
	}

	_ = c.cleanupImages()
	_ = c.setBootloader(snapshot.ID)
	_ = c.setDigestToSrc(ref)

	return nil
}

func (c *Containerd) DeleteSnapshot(id int) error {
	img, err := c.getImage(id)
	if err != nil {
		c.cfg.Logger.Errorf("failed to find Elemental snapshot %d", id)
		return err
	}
	if img == "" {
		c.cfg.Logger.Warn("image not found, nothing to delete")
		return nil
	}
	//TODO check id is not the current running image
	err = c.containerd.Delete(img)
	if err != nil {
		c.cfg.Logger.Errorf("failed deteling Elemental snapshot %d", id)
		return err
	}
	return nil
}

func (c *Containerd) GetSnapshots() ([]int, error) {
	ids := []int{}
	images, err := c.getSnapshots()
	if err != nil {
		return []int{}, err
	}
	for k := range images {
		ids = append(ids, k)
	}
	return ids, nil
}

func (c *Containerd) SnapshotToImageSource(snap *types.Snapshot) (*types.ImageSource, error) {
	// TODO consider better options for remote images
	if c.imgSrc == nil {
		return nil, errors.New("no source defined")
	}
	return c.imgSrc, nil
}

func (c *Containerd) getSnapshots() (map[int]string, error) {
	var pErrs []error

	images := map[int]string{}
	snapshotFilter := fmt.Sprintf("labels.\"%s\"", snapshotLabel)
	filters := []string{snapshotFilter}
	imgs, err := c.containerd.List(filters...)
	if err != nil {
		return nil, err
	}
	if len(imgs) == 0 {
		return images, nil
	}

	var latestActive time.Time
	var activeID int
	var activeImgRef string

	for _, img := range imgs {
		labels := img.Labels()
		if len(labels) == 0 {
			c.cfg.Logger.Warnf("'%s' is not an active image, ignoring it. No labels defined", img.Name())
			continue
		}
		snapValue := labels[snapshotLabel]
		id, err := strconv.Atoi(snapValue)
		if err != nil {
			c.cfg.Logger.Errorf("failed to parse snapshot label '%s' value to a valid ID: %v", snapValue, err)
			pErrs = append(pErrs, err)
			continue
		}
		images[id] = img.Name()

		// Parse active image
		tsStr := labels[activeLabel]
		if tsStr != "" {
			ts, err := time.Parse(time.RFC3339, tsStr)
			if err != nil {
				c.cfg.Logger.Warnf("'%s' is not properly labeled, ignoring it. Bad format on active label: %s", img.Name(), labels[activeLabel])
				continue
			}
			if latestActive.Before(ts) {
				activeID = id
				activeImgRef = img.Name()
				latestActive = ts
			}
		}
	}
	if len(pErrs) > 0 {
		return nil, errors.New("failed parsing some snapshot")
	}

	if activeID == 0 {
		c.cfg.Logger.Errorf("inconsistent state, found %d images, but none of them is set as active", len(imgs))
		return nil, errors.New("inconsistent state")
	}

	c.activeID = activeID
	c.activeImgRef = activeImgRef

	if elemental.IsActiveMode(c.cfg) {
		c.currentID = activeID
	} else if elemental.IsPassiveMode(c.cfg) {
		id, err := c.getCurrentImg()
		if err != nil {
			return nil, err
		}
		c.currentID = id
	}

	return images, nil
}

func (c *Containerd) getNextSnapshotID() (int, error) {
	ids, err := c.GetSnapshots()
	if err != nil {
		c.cfg.Logger.Errorf("failed to compute next snapshot ID: %v", err)
		return 0, err
	}
	if len(ids) == 0 {
		return 1, nil
	}
	return slices.Max(ids) + 1, nil
}

func (c *Containerd) getBaseImage(snapshotKey string) (client.Image, error) {
	info, err := c.containerd.GetSnapshot(snapshotKey)
	if err != nil {
		return nil, err
	}

	if imgRef, ok := info.Labels[ocistore.LabelSnapshotImgRef]; ok {
		img, err := c.containerd.Get(imgRef)
		if err != nil {
			return nil, err
		}
		return img, nil
	}

	return nil, nil
}

func (c *Containerd) setNewImageReference(snapshotKey string) (string, error) {
	img, err := c.getBaseImage(snapshotKey)
	if err != nil {
		return "", err
	}
	if img == nil {
		return fmt.Sprintf("%s:%s", scratchImgName, time.Now().Format("20060102150405.00")), nil
	}

	return fmt.Sprintf("%s-%s", img.Name(), time.Now().Format("20060102150405.00")), nil
}

func (c *Containerd) removeActiveLabelFromImg() error {
	if c.activeImgRef == "" {
		return nil
	}
	img, err := c.containerd.Get(c.activeImgRef)
	if err != nil {
		return err
	}
	image := img.Metadata()
	labels := image.Labels
	delete(labels, activeLabel)
	image.Labels = labels
	_, err = c.containerd.Update(image)
	return err
}

// setBootloader sets the bootloader variables to update new passives
func (c *Containerd) setBootloader(activeSnapshotID int) error {
	var passives, fallbacks []string

	c.cfg.Logger.Infof("Setting bootloader with current passive snapshots")
	ids, err := c.getPassiveSnapshots()
	if err != nil {
		c.cfg.Logger.Warnf("failed getting current passive snapshots: %v", err)
		return err
	}

	for i := len(ids) - 1; i >= 0; i-- {
		passives = append(passives, strconv.Itoa(ids[i]))
	}

	// We count first is active, then all passives and finally the recovery
	for i := 0; i <= len(ids)+1; i++ {
		fallbacks = append(fallbacks, strconv.Itoa(i))
	}
	snapsList := strings.Join(passives, " ")
	fallbackList := strings.Join(fallbacks, " ")
	envFile := filepath.Join(c.efiDir, constants.GrubOEMEnv)

	envs := map[string]string{
		constants.GrubFallback:         fallbackList,
		constants.GrubPassiveSnapshots: snapsList,
		constants.GrubActiveSnapshot:   strconv.Itoa(activeSnapshotID),
		"snapshotter":                  constants.ContainerdSnapshotterType,
	}

	err = c.bootloader.SetPersistentVariables(envFile, envs)
	if err != nil {
		c.cfg.Logger.Warnf("failed setting bootloader environment file %s: %v", envFile, err)
		return err
	}

	return err
}

func (c *Containerd) getPassiveSnapshots() ([]int, error) {
	ids, err := c.GetSnapshots()
	if err != nil {
		return nil, err
	}
	i := slices.Index(ids, c.activeID)
	if i < 0 {
		return ids, nil
	}
	ids = append(ids[:i], ids[i+1:]...)
	return ids, nil
}

func (c *Containerd) getImage(id int) (string, error) {
	imgs, err := c.getSnapshots()
	if err != nil {
		return "", err
	}

	return imgs[id], nil
}

func (c *Containerd) cleanupImages() error {
	images, err := c.getSnapshots()
	if err != nil {
		return err
	}
	snapsToDelete := len(images) - c.snapshotterCfg.MaxSnaps
	if snapsToDelete > 0 {
		var ids []int
		for k := range images {
			ids = append(ids, k)
		}
		slices.Sort(ids)
		for i := range snapsToDelete {
			if ids[i] == c.currentID {
				c.cfg.Logger.Warnf("current snapshot '%d' can't be cleaned up, stopping", ids[i])
				break
			}
			err = c.containerd.Delete(images[i])
			if err != nil {
				c.cfg.Logger.Errorf("failed deteling Elemental snapshot %d", i)
				return err
			}
		}
	}
	return nil
}

func (c *Containerd) getCurrentImg() (int, error) {
	cmdLineOut, err := c.cfg.Fs.ReadFile("/proc/cmdline")
	if err != nil {
		return 0, err
	}
	// Assumes there is an argument as 'img=<ID>' in kernel command line
	// This is coupled to grub setup and sysroot features
	for _, field := range strings.Fields(string(cmdLineOut)) {
		if strings.HasPrefix(field, "img=") {
			if _, a, ok := strings.Cut(field, "="); ok {
				id, err := strconv.Atoi(a)
				if err != nil {
					c.cfg.Logger.Warnf("failed to parse image id from '%s': %v", field, err)
					continue
				}
				return id, nil
			}
		}
	}
	return 0, errors.New("current image ID not found")
}

func (c *Containerd) setDigestToSrc(ref string) error {
	img, err := c.containerd.Get(ref)
	if err != nil {
		return err
	}
	c.imgSrc.SetDigest(img.Target().Digest.String())
	return nil
}
