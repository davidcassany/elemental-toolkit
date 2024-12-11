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

package action

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/davidcassany/ocistore/pkg/ocistore"
	"github.com/rancher/elemental-toolkit/v2/pkg/constants"
	"github.com/rancher/elemental-toolkit/v2/pkg/types"
)

func RunSysrootMount(cfg *types.RunConfig, snapshot string) error {
	var filter string

	st := ocistore.NewOCIStore(cfg.Logger, constants.RunningStateDir)
	if err := st.Init(context.Background()); err != nil {
		return err
	}
	if snapshot == constants.ActiveImgName {
		filter = fmt.Sprintf("labels.%s", "elemental-toolkit.io/image.active")
	} else {
		filter = fmt.Sprintf("labels.%s==%s", "elemental-toolkit.io/image.elemental.id", snapshot)
	}
	imgs, err := st.List(filter)
	if err != nil {
		return err
	}
	if len(imgs) == 0 {
		return fmt.Errorf("image '%s' not found", snapshot)
	}
	image := imgs[0]
	// Mount found image at /sysroot
	_, err = st.Mount(image, "/sysroot", "", true, ocistore.WithMountSnapshotOpts(snapshots.WithLabels(map[string]string{
		"elemental-toolkit.io/snapshot.active": time.Now().UTC().Format(time.RFC3339),
	})))

	return err
}
