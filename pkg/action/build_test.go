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

package action_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"
	"github.com/twpayne/go-vfs"
	"github.com/twpayne/go-vfs/vfst"

	"github.com/rancher/elemental-toolkit/pkg/action"
	"github.com/rancher/elemental-toolkit/pkg/config"
	"github.com/rancher/elemental-toolkit/pkg/constants"
	v1mock "github.com/rancher/elemental-toolkit/pkg/mocks"
	v1 "github.com/rancher/elemental-toolkit/pkg/types/v1"
	"github.com/rancher/elemental-toolkit/pkg/utils"
)

var _ = Describe("Build Actions", func() {
	var cfg *v1.BuildConfig
	var runner *v1mock.FakeRunner
	var fs vfs.FS
	var logger v1.Logger
	var mounter *v1mock.ErrorMounter
	var syscall *v1mock.FakeSyscall
	var client *v1mock.FakeHTTPClient
	var cloudInit *v1mock.FakeCloudInitRunner
	var extractor *v1mock.FakeImageExtractor
	var cleanup func()
	var memLog *bytes.Buffer
	BeforeEach(func() {
		runner = v1mock.NewFakeRunner()
		syscall = &v1mock.FakeSyscall{}
		mounter = v1mock.NewErrorMounter()
		client = &v1mock.FakeHTTPClient{}
		memLog = &bytes.Buffer{}
		logger = v1.NewBufferLogger(memLog)
		logger.SetLevel(logrus.DebugLevel)
		extractor = v1mock.NewFakeImageExtractor(logger)
		cloudInit = &v1mock.FakeCloudInitRunner{}
		fs, cleanup, _ = vfst.NewTestFS(map[string]interface{}{})
		cfg = config.NewBuildConfig(
			config.WithFs(fs),
			config.WithRunner(runner),
			config.WithLogger(logger),
			config.WithMounter(mounter),
			config.WithSyscall(syscall),
			config.WithClient(client),
			config.WithCloudInitRunner(cloudInit),
			config.WithImageExtractor(extractor),
			config.WithPlatform("linux/amd64"),
		)

	})
	AfterEach(func() {
		cleanup()
	})
	Describe("Build ISO", Label("iso"), func() {
		var iso *v1.LiveISO
		BeforeEach(func() {
			iso = config.NewISO()

			tmpDir, err := utils.TempDir(fs, "", "test")
			Expect(err).ShouldNot(HaveOccurred())

			cfg.Date = false
			cfg.OutDir = tmpDir

			runner.SideEffect = func(cmd string, args ...string) ([]byte, error) {
				switch cmd {
				case "xorriso":
					err := fs.WriteFile(filepath.Join(tmpDir, "elemental.iso"), []byte("profound thoughts"), constants.FilePerm)
					return []byte{}, err
				default:
					return []byte{}, nil
				}
			}
		})
		It("Successfully builds an ISO from an OCI image", func() {
			rootSrc, _ := v1.NewSrcFromURI("oci:elementalos:latest")
			iso.RootFS = []*v1.ImageSource{rootSrc}

			extractor.SideEffect = func(_, destination, platform string, _ bool) error {
				bootDir := filepath.Join(destination, "boot")
				logger.Debugf("Creating %s", bootDir)
				err := utils.MkdirAll(fs, bootDir, constants.DirPerm)
				if err != nil {
					return err
				}
				_, err = fs.Create(filepath.Join(bootDir, "vmlinuz"))
				if err != nil {
					return err
				}

				_, err = fs.Create(filepath.Join(bootDir, "initrd"))
				return err
			}

			buildISO := action.NewBuildISOAction(cfg, iso)
			err := buildISO.ISORun()

			Expect(err).ShouldNot(HaveOccurred())
		})
		It("Successfully builds an ISO using self contained binaries and including overlayed files", func() {
			rootSrc, _ := v1.NewSrcFromURI("dir:/overlay/dir")
			iso.RootFS = []*v1.ImageSource{rootSrc}

			liveBoot := &v1mock.LiveBootLoaderMock{}
			buildISO := action.NewBuildISOAction(cfg, iso, action.WithLiveBoot(liveBoot))
			err := buildISO.ISORun()

			Expect(err).Should(HaveOccurred())
		})
		It("Fails on prepare EFI", func() {
			iso.BootloaderInRootFs = true

			rootSrc, _ := v1.NewSrcFromURI("oci:elementalos:latest")
			iso.RootFS = append(iso.RootFS, rootSrc)

			liveBoot := &v1mock.LiveBootLoaderMock{ErrorEFI: true}
			buildISO := action.NewBuildISOAction(cfg, iso, action.WithLiveBoot(liveBoot))
			err := buildISO.ISORun()

			Expect(err).Should(HaveOccurred())
		})
		It("Fails on prepare ISO", func() {
			iso.BootloaderInRootFs = true

			rootSrc, _ := v1.NewSrcFromURI("channel:system/elemental")
			iso.RootFS = append(iso.RootFS, rootSrc)

			liveBoot := &v1mock.LiveBootLoaderMock{ErrorISO: true}
			buildISO := action.NewBuildISOAction(cfg, iso, action.WithLiveBoot(liveBoot))
			err := buildISO.ISORun()

			Expect(err).Should(HaveOccurred())
		})
		It("Fails if kernel or initrd is not found in rootfs", func() {
			rootSrc, _ := v1.NewSrcFromURI("dir:/local/dir")
			iso.RootFS = []*v1.ImageSource{rootSrc}

			err := utils.MkdirAll(fs, "/local/dir/boot", constants.DirPerm)
			Expect(err).ShouldNot(HaveOccurred())

			By("fails without kernel")
			buildISO := action.NewBuildISOAction(cfg, iso)
			err = buildISO.ISORun()
			Expect(err).Should(HaveOccurred())

			By("fails without initrd")
			_, err = fs.Create("/local/dir/boot/vmlinuz")
			Expect(err).ShouldNot(HaveOccurred())
			buildISO = action.NewBuildISOAction(cfg, iso)
			err = buildISO.ISORun()
			Expect(err).Should(HaveOccurred())
		})
		It("Fails installing rootfs sources", func() {
			rootSrc, _ := v1.NewSrcFromURI("channel:system/elemental")
			iso.RootFS = []*v1.ImageSource{rootSrc}

			buildISO := action.NewBuildISOAction(cfg, iso)
			err := buildISO.ISORun()
			Expect(err).Should(HaveOccurred())
		})
		It("Fails installing uefi sources", func() {
			rootSrc, _ := v1.NewSrcFromURI("docker:elemental:latest")
			iso.RootFS = []*v1.ImageSource{rootSrc}
			uefiSrc, _ := v1.NewSrcFromURI("dir:/overlay/efi")
			iso.UEFI = []*v1.ImageSource{uefiSrc}

			buildISO := action.NewBuildISOAction(cfg, iso)
			err := buildISO.ISORun()
			Expect(err).Should(HaveOccurred())
		})
		It("Fails on ISO filesystem creation", func() {
			rootSrc, _ := v1.NewSrcFromURI("dir:/local/dir")
			iso.RootFS = []*v1.ImageSource{rootSrc}

			err := utils.MkdirAll(fs, "/local/dir/boot", constants.DirPerm)
			Expect(err).ShouldNot(HaveOccurred())
			_, err = fs.Create("/local/dir/boot/vmlinuz")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = fs.Create("/local/dir/boot/initrd")
			Expect(err).ShouldNot(HaveOccurred())

			runner.SideEffect = func(command string, args ...string) ([]byte, error) {
				if command == "xorriso" {
					return []byte{}, errors.New("Burn ISO error")
				}
				return []byte{}, nil
			}

			buildISO := action.NewBuildISOAction(cfg, iso)
			err = buildISO.ISORun()

			Expect(err).Should(HaveOccurred())
		})
	})
	Describe("Build disk", Label("disk", "build"), func() {
		var disk *v1.Disk

		BeforeEach(func() {
			tmpDir, err := utils.TempDir(fs, "", "test")
			Expect(err).ShouldNot(HaveOccurred())

			cfg.Date = false
			cfg.OutDir = tmpDir
			disk = config.NewDisk(cfg)
			disk.Recovery.Source = v1.NewDockerSrc("some/image/ref:tag")

			recoveryRoot := filepath.Join(tmpDir, "build", filepath.Base(disk.Recovery.File)+".root")

			// Create grub.cfg
			grubConf := filepath.Join(recoveryRoot, "/etc/cos/grub.cfg")
			Expect(utils.MkdirAll(fs, filepath.Dir(grubConf), constants.DirPerm)).To(Succeed())
			Expect(fs.WriteFile(grubConf, []byte{}, constants.FilePerm)).To(Succeed())

			// Create grub modules
			grubModulesDir := filepath.Join(recoveryRoot, "/usr/share/grub2/x86_64-efi")
			Expect(utils.MkdirAll(fs, grubModulesDir, constants.DirPerm)).To(Succeed())
			Expect(fs.WriteFile(filepath.Join(grubModulesDir, "loopback.mod"), []byte{}, constants.FilePerm)).To(Succeed())
			// HOW this could be! only one required? Potential bug here!
			//Expect(fs.WriteFile(filepath.Join(grubModulesDir, "squash4.mod"), []byte{}, constants.FilePerm)).To(Succeed())
			//Expect(fs.WriteFile(filepath.Join(grubModulesDir, "xzio.mod"), []byte{}, constants.FilePerm)).To(Succeed())

			// Create os-release
			Expect(fs.WriteFile(filepath.Join(recoveryRoot, "/etc/os-release"), []byte{}, constants.FilePerm)).To(Succeed())

			// Create efi files
			grubEfiDir := filepath.Join(recoveryRoot, "/usr/share/efi/x86_64")
			Expect(utils.MkdirAll(fs, grubEfiDir, constants.DirPerm)).To(Succeed())
			Expect(fs.WriteFile(filepath.Join(grubEfiDir, "grub.efi"), []byte{}, constants.FilePerm))
			Expect(fs.WriteFile(filepath.Join(grubEfiDir, "shim.efi"), []byte{}, constants.FilePerm))
			Expect(fs.WriteFile(filepath.Join(grubEfiDir, "MokManager.efi"), []byte{}, constants.FilePerm))
		})
		It("Successfully builds a full raw disk", func() {
			buildDisk := action.NewBuildDiskAction(cfg, disk)
			Expect(buildDisk.BuildDiskRun()).To(Succeed())

			Expect(runner.MatchMilestones([][]string{
				{"mksquashfs", "/tmp/test/build/recovery.img.root", "/tmp/test/build/state/cOS/active.img"},
				{"mksquashfs", "/tmp/test/build/recovery.img.root", "/tmp/test/build/state/cOS/passive.img"},
				{"mksquashfs", "/tmp/test/build/recovery.img.root", "/tmp/test/build/recovery/cOS/recovery.img"},
				{"mkfs.vfat", "-n", "COS_GRUB"},
				{"mkfs.ext4", "-L", "COS_OEM"},
				{"losetup", "--show", "-f", "/tmp/test/build/oem.part"},
				{"mkfs.ext4", "-L", "COS_RECOVERY"},
				{"losetup", "--show", "-f", "/tmp/test/build/recovery.part"},
				{"mkfs.ext4", "-L", "COS_STATE"},
				{"losetup", "--show", "-f", "/tmp/test/build/state.part"},
				{"mkfs.ext4", "-L", "COS_PERSISTENT"},
				{"losetup", "--show", "-f", "/tmp/test/build/persistent.part"},
				{"sgdisk", "-p", "/tmp/test/elemental.raw"},
				{"partprobe", "/tmp/test/elemental.raw"},
			})).To(Succeed())
		})
		It("Successfully builds a full raw disk with an unprivileged setup", func() {
			disk.Unprivileged = true
			disk.Active.FS = constants.LinuxImgFs
			disk.Passive.FS = constants.LinuxImgFs
			buildDisk := action.NewBuildDiskAction(cfg, disk)
			// Unprivileged setup, it should not run any mount
			mounter.ErrorOnMount = true

			Expect(buildDisk.BuildDiskRun()).To(Succeed())

			Expect(runner.MatchMilestones([][]string{
				{"mkfs.ext2", "-d", "/tmp/test/build/recovery.img.root", "/tmp/test/build/state/cOS/active.img"},
				{"mkfs.ext2", "-d", "/tmp/test/build/recovery.img.root", "/tmp/test/build/state/cOS/passive.img"},
				{"mksquashfs", "/tmp/test/build/recovery.img.root", "/tmp/test/build/recovery/cOS/recovery.img"},
				{"mkfs.vfat", "-n", "COS_GRUB"},
				{"mkfs.ext4", "-L", "COS_OEM"},
				{"mkfs.ext4", "-L", "COS_RECOVERY"},
				{"mkfs.ext4", "-L", "COS_STATE"},
				{"mkfs.ext4", "-L", "COS_PERSISTENT"},
				{"sgdisk", "-p", "/tmp/test/elemental.raw"},
				{"partprobe", "/tmp/test/elemental.raw"},
			})).To(Succeed())
		})
		It("Successfully builds an expandable disk with an unprivileged setup", func() {
			disk.Unprivileged = true
			disk.Expandable = true
			disk.Active.FS = constants.LinuxImgFs
			disk.Passive.FS = constants.LinuxImgFs
			buildDisk := action.NewBuildDiskAction(cfg, disk)
			// Unprivileged setup, it should not run any mount
			// test won't pass if any mount is called
			mounter.ErrorOnMount = true

			Expect(buildDisk.BuildDiskRun()).To(Succeed())

			Expect(runner.MatchMilestones([][]string{
				{"mksquashfs", "/tmp/test/build/recovery.img.root", "/tmp/test/build/recovery/cOS/recovery.img"},
				{"mkfs.vfat", "-n", "COS_GRUB"},
				{"mkfs.ext4", "-L", "COS_OEM"},
				{"mkfs.ext4", "-L", "COS_RECOVERY"},
				{"mkfs.ext4", "-L", "COS_STATE"},
				{"sgdisk", "-p", "/tmp/test/elemental.raw"},
				{"partprobe", "/tmp/test/elemental.raw"},
			})).To(Succeed())
		})
		It("Builds a raw image", func() {
			// temp dir for output, otherwise we write to .
			/*outputDir, _ := utils.TempDir(fs, "", "output")
			// temp dir for package files, create needed file
			filesDir, _ := utils.TempDir(fs, "", "elemental-build-disk-files")
			_ = utils.MkdirAll(fs, filepath.Join(filesDir, "root", "etc", "cos"), constants.DirPerm)
			_ = fs.WriteFile(filepath.Join(filesDir, "root", "etc", "cos", "grubenv_firstboot"), []byte(""), os.ModePerm)

			// temp dir for part files, create parts
			partsDir, _ := utils.TempDir(fs, "", "elemental-build-disk-parts")
			_ = fs.WriteFile(filepath.Join(partsDir, "rootfs.part"), []byte(""), os.ModePerm)
			_ = fs.WriteFile(filepath.Join(partsDir, "oem.part"), []byte(""), os.ModePerm)
			_ = fs.WriteFile(filepath.Join(partsDir, "efi.part"), []byte(""), os.ModePerm)

			err := action.BuildDiskRun(cfg, rawDisk.X86_64, "raw", "OEM", "REC", filepath.Join(outputDir, "disk.raw"))
			Expect(err).ToNot(HaveOccurred())
			_ = fs.RemoveAll(filesDir)
			_ = fs.RemoveAll(partsDir)
			// Check that we copied all needed files to final image
			Expect(memLog.String()).To(ContainSubstring("efi.part"))
			Expect(memLog.String()).To(ContainSubstring("rootfs.part"))
			Expect(memLog.String()).To(ContainSubstring("oem.part"))
			output, err2 := fs.Stat(filepath.Join(outputDir, "disk.raw"))
			Expect(err2).ToNot(HaveOccurred())
			// Even with empty parts, output image should never be zero due to the truncating
			// it should be exactly 20Mb(efi size) + 64Mb(oem size) + 2048Mb(recovery size) + 3Mb(hybrid boot) + 1Mb(GPT)
			partsSize := (20 + 64 + 2048 + 3 + 1) * 1024 * 1024
			Expect(output.Size()).To(BeNumerically("==", partsSize))
			_ = fs.RemoveAll(outputDir)
			// Check that mkfs commands set the label properly and copied the proper dirs
			err2 = runner.IncludesCmds([][]string{
				{"mkfs.ext2", "-L", "REC", "-d", "/tmp/elemental-build-disk-files/root", "/tmp/elemental-build-disk-parts/rootfs.part"},
				{"mkfs.vfat", "-n", constants.EfiLabel, "/tmp/elemental-build-disk-parts/efi.part"},
				{"mkfs.ext2", "-L", "OEM", "-d", "/tmp/elemental-build-disk-files/oem", "/tmp/elemental-build-disk-parts/oem.part"},
				// files should be copied to EFI
				{"mcopy", "-s", "-i", "/tmp/elemental-build-disk-parts/efi.part", "/tmp/elemental-build-disk-files/efi/EFI", "::EFI"},
			})
			Expect(err2).ToNot(HaveOccurred())*/
		})
		It("Builds a raw image with GCE output", func() {
			// temp dir for output, otherwise we write to .
			/*outputDir, _ := utils.TempDir(fs, "", "output")
			// temp dir for package files, create needed file
			filesDir, _ := utils.TempDir(fs, "", "elemental-build-disk-files")
			_ = utils.MkdirAll(fs, filepath.Join(filesDir, "root", "etc", "cos"), constants.DirPerm)
			_ = fs.WriteFile(filepath.Join(filesDir, "root", "etc", "cos", "grubenv_firstboot"), []byte(""), os.ModePerm)

			// temp dir for part files, create parts
			partsDir, _ := utils.TempDir(fs, "", "elemental-build-disk-parts")
			_ = fs.WriteFile(filepath.Join(partsDir, "rootfs.part"), []byte(""), os.ModePerm)
			_ = fs.WriteFile(filepath.Join(partsDir, "oem.part"), []byte(""), os.ModePerm)
			_ = fs.WriteFile(filepath.Join(partsDir, "efi.part"), []byte(""), os.ModePerm)

			err := action.BuildDiskRun(cfg, rawDisk.X86_64, "gce", "OEM", "REC", filepath.Join(outputDir, "disk.raw"))
			Expect(err).ToNot(HaveOccurred())
			_ = fs.RemoveAll(filesDir)
			_ = fs.RemoveAll(partsDir)
			// Check that we copied all needed files to final image
			Expect(memLog.String()).To(ContainSubstring("efi.part"))
			Expect(memLog.String()).To(ContainSubstring("rootfs.part"))
			Expect(memLog.String()).To(ContainSubstring("oem.part"))
			realPath, _ := fs.RawPath(outputDir)
			Expect(dockerArchive.IsArchivePath(filepath.Join(realPath, "disk.raw.tar.gz"))).To(BeTrue())
			_ = fs.RemoveAll(outputDir)
			// Check that mkfs commands set the label properly and copied the proper dirs
			err2 := runner.IncludesCmds([][]string{
				{"mkfs.ext2", "-L", "REC", "-d", "/tmp/elemental-build-disk-files/root", "/tmp/elemental-build-disk-parts/rootfs.part"},
				{"mkfs.vfat", "-n", constants.EfiLabel, "/tmp/elemental-build-disk-parts/efi.part"},
				{"mkfs.ext2", "-L", "OEM", "-d", "/tmp/elemental-build-disk-files/oem", "/tmp/elemental-build-disk-parts/oem.part"},
				// files should be copied to EFI
				{"mcopy", "-s", "-i", "/tmp/elemental-build-disk-parts/efi.part", "/tmp/elemental-build-disk-files/efi/EFI", "::EFI"},
			})
			Expect(err2).ToNot(HaveOccurred())*/

		})
		It("Builds a raw image with Azure output", func() {
			// temp dir for output, otherwise we write to .
			/*outputDir, _ := utils.TempDir(fs, "", "output")
			// temp dir for package files, create needed file
			filesDir, _ := utils.TempDir(fs, "", "elemental-build-disk-files")
			_ = utils.MkdirAll(fs, filepath.Join(filesDir, "root", "etc", "cos"), constants.DirPerm)
			_ = fs.WriteFile(filepath.Join(filesDir, "root", "etc", "cos", "grubenv_firstboot"), []byte(""), os.ModePerm)

			// temp dir for part files, create parts
			partsDir, _ := utils.TempDir(fs, "", "elemental-build-disk-parts")
			_ = fs.WriteFile(filepath.Join(partsDir, "rootfs.part"), []byte(""), os.ModePerm)
			_ = fs.WriteFile(filepath.Join(partsDir, "oem.part"), []byte(""), os.ModePerm)
			_ = fs.WriteFile(filepath.Join(partsDir, "efi.part"), []byte(""), os.ModePerm)

			err := action.BuildDiskRun(cfg, rawDisk.X86_64, "azure", "OEM", "REC", filepath.Join(outputDir, "disk.raw"))
			Expect(err).ToNot(HaveOccurred())
			_ = fs.RemoveAll(filesDir)
			_ = fs.RemoveAll(partsDir)
			// Check that we copied all needed files to final image
			Expect(memLog.String()).To(ContainSubstring("efi.part"))
			Expect(memLog.String()).To(ContainSubstring("rootfs.part"))
			Expect(memLog.String()).To(ContainSubstring("oem.part"))
			f, _ := fs.Open(filepath.Join(outputDir, "disk.raw.vhd"))
			info, _ := f.Stat()
			// Dump the header from the file into our VHDHeader
			buff := make([]byte, 512)
			_, _ = f.ReadAt(buff, info.Size()-512)
			_ = f.Close()

			header := utils.VHDHeader{}
			err2 := binary.Read(bytes.NewBuffer(buff[:]), binary.BigEndian, &header)
			Expect(err2).ToNot(HaveOccurred())
			// Just check the fields that we know the value of, that should indicate that the header is valid
			Expect(hex.EncodeToString(header.DiskType[:])).To(Equal("00000002"))
			Expect(hex.EncodeToString(header.Features[:])).To(Equal("00000002"))
			Expect(hex.EncodeToString(header.DataOffset[:])).To(Equal("ffffffffffffffff"))
			_ = fs.RemoveAll(outputDir)
			// Check that mkfs commands set the label properly and copied the proper dirs
			err2 = runner.IncludesCmds([][]string{
				{"mkfs.ext2", "-L", "REC", "-d", "/tmp/elemental-build-disk-files/root", "/tmp/elemental-build-disk-parts/rootfs.part"},
				{"mkfs.vfat", "-n", constants.EfiLabel, "/tmp/elemental-build-disk-parts/efi.part"},
				{"mkfs.ext2", "-L", "OEM", "-d", "/tmp/elemental-build-disk-files/oem", "/tmp/elemental-build-disk-parts/oem.part"},
				// files should be copied to EFI
				{"mcopy", "-s", "-i", "/tmp/elemental-build-disk-parts/efi.part", "/tmp/elemental-build-disk-files/efi/EFI", "::EFI"},
			})
			Expect(err2).ToNot(HaveOccurred())*/

		})
		It("Transforms raw image into GCE image", Label("gce"), func() {
			tmpDir, err := utils.TempDir(fs, "", "")
			defer fs.RemoveAll(tmpDir)
			Expect(err).ToNot(HaveOccurred())
			Expect(err).ToNot(HaveOccurred())
			f, err := fs.Create(filepath.Join(tmpDir, "disk.raw"))
			Expect(err).ToNot(HaveOccurred())
			// Set a non rounded size
			f.Truncate(34 * 1024 * 1024)
			f.Close()
			err = action.Raw2Gce(filepath.Join(tmpDir, "disk.raw"), fs, logger, false)
			Expect(err).ToNot(HaveOccurred())
			// Log should have the rounded size (1Gb)
			Expect(memLog.String()).To(ContainSubstring(strconv.Itoa(1 * 1024 * 1024 * 1024)))
			// Should be a tar file
			//realPath, _ := fs.RawPath(tmpDir)
			//Expect(dockerArchive.IsArchivePath(filepath.Join(realPath, "disk.raw.tar.gz"))).To(BeTrue())
		})
		It("Transforms raw image into Azure image", func() {
			tmpDir, err := utils.TempDir(fs, "", "")
			defer fs.RemoveAll(tmpDir)
			Expect(err).ToNot(HaveOccurred())
			Expect(err).ToNot(HaveOccurred())
			f, err := fs.Create(filepath.Join(tmpDir, "disk.raw"))
			Expect(err).ToNot(HaveOccurred())
			// write something
			_ = f.Truncate(23 * 1024 * 1024)
			_ = f.Close()
			err = action.Raw2Azure(filepath.Join(tmpDir, "disk.raw"), fs, logger, true)
			Expect(err).ToNot(HaveOccurred())
			info, err := fs.Stat(filepath.Join(tmpDir, "disk.raw.vhd"))
			Expect(err).ToNot(HaveOccurred())
			// Should have be rounded up to the next MB
			Expect(info.Size()).To(BeNumerically("==", 23*1024*1024))

			// Read the header
			f, _ = fs.Open(filepath.Join(tmpDir, "disk.raw.vhd"))
			info, _ = f.Stat()
			// Dump the header from the file into our VHDHeader
			buff := make([]byte, 512)
			_, _ = f.ReadAt(buff, info.Size()-512)
			_ = f.Close()

			header := utils.VHDHeader{}
			err = binary.Read(bytes.NewBuffer(buff[:]), binary.BigEndian, &header)
			Expect(err).ToNot(HaveOccurred())
			// Just check the fields that we know the value of, that should indicate that the header is valid
			Expect(hex.EncodeToString(header.DiskType[:])).To(Equal("00000002"))
			Expect(hex.EncodeToString(header.Features[:])).To(Equal("00000002"))
			Expect(hex.EncodeToString(header.DataOffset[:])).To(Equal("ffffffffffffffff"))
		})
		It("Transforms raw image into Azure image (tiny image)", func() {
			// This tests that the resize works for tiny images
			// Not sure if we ever will encounter them (less than 1 Mb images?) but just in case
			tmpDir, err := utils.TempDir(fs, "", "")
			defer fs.RemoveAll(tmpDir)
			Expect(err).ToNot(HaveOccurred())
			Expect(err).ToNot(HaveOccurred())
			f, err := fs.Create(filepath.Join(tmpDir, "disk.raw"))
			Expect(err).ToNot(HaveOccurred())
			// write something
			_, _ = f.WriteString("Hi")
			_ = f.Close()
			err = action.Raw2Azure(filepath.Join(tmpDir, "disk.raw"), fs, logger, true)
			Expect(err).ToNot(HaveOccurred())
			info, err := fs.Stat(filepath.Join(tmpDir, "disk.raw"))
			Expect(err).ToNot(HaveOccurred())
			// Should be smaller than 1Mb
			Expect(info.Size()).To(BeNumerically("<", 1*1024*1024))

			info, err = fs.Stat(filepath.Join(tmpDir, "disk.raw.vhd"))
			Expect(err).ToNot(HaveOccurred())
			// Should have be rounded up to the next MB
			Expect(info.Size()).To(BeNumerically("==", 1*1024*1024))

			// Read the header
			f, _ = fs.Open(filepath.Join(tmpDir, "disk.raw.vhd"))
			info, _ = f.Stat()
			// Dump the header from the file into our VHDHeader
			buff := make([]byte, 512)
			_, _ = f.ReadAt(buff, info.Size()-512)
			_ = f.Close()

			header := utils.VHDHeader{}
			err = binary.Read(bytes.NewBuffer(buff[:]), binary.BigEndian, &header)
			Expect(err).ToNot(HaveOccurred())
			// Just check the fields that we know the value of, that should indicate that the header is valid
			Expect(hex.EncodeToString(header.DiskType[:])).To(Equal("00000002"))
			Expect(hex.EncodeToString(header.Features[:])).To(Equal("00000002"))
			Expect(hex.EncodeToString(header.DataOffset[:])).To(Equal("ffffffffffffffff"))
		})
	})
})
