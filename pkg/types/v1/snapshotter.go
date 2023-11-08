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

package v1

type Snapshotter interface {
	InitSnapshotter(rootDir string) error
	StartTransaction() (*Snapshot, error)
	CloseTransaction(snap *Snapshot) error
	CloseTransactionOnError(snap *Snapshot) error
}

type SnapshotterConfig struct {
	Type     string
	MaxSnaps int `yaml:"max-snaps,omitempty" mapstructure:"max-snaps"`
	//Label    string `yaml:"label,omitempty" mapstructure:"label"`
	Size uint   `yaml:"size,omitempty" mapstructure:"size"`
	FS   string `yaml:"fs,omitempty" mapstructure:"fs"`
}

type Snapshot struct {
	ID         int
	MountPoint string
	Path       string
	WorkDir    string
	Label      string
	InProgress bool
}
