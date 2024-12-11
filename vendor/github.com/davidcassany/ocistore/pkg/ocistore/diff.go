/*
Copyright © 2024 SUSE LLC

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

package ocistore

import (
	"context"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/diff/apply"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/plugins/diff/walking"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Define a local (grpcless) implementation of the diffservice
type diffService struct {
	walkDiff  diff.Comparer
	applyDiff diff.Applier
}

func (d *diffService) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (ocispec.Descriptor, error) {
	return d.walkDiff.Compare(ctx, lower, upper, opts...)
}

func (d *diffService) Apply(ctx context.Context, desc ocispec.Descriptor, mount []mount.Mount, opts ...diff.ApplyOpt) (ocispec.Descriptor, error) {
	return d.applyDiff.Apply(ctx, desc, mount, opts...)
}

func NewDiffService(store content.Store) client.DiffService {
	return &diffService{
		walkDiff:  walking.NewWalkingDiff(store),
		applyDiff: apply.NewFileSystemApplier(store),
	}
}
