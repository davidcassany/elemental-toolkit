#!/bin/bash -x

set -e

[ $# != 1 ] && exit 1

# osimage is the image reference build with "make build-os"
osimage=$1

# Create an empty raw disk image
qemu-img create -f raw disk.img 16G

loopdev=$(losetup -f --show disk.img)

# Just to create the basi disk layout and EFI partition
./build/elemental --debug install --firmware efi --disable-boot-entry --local --system.uri ${osimage} ${loopdev}

# Reformat STATE partition with btrfs
mkfs.btrfs -L COS_STATE ${loopdev}p4 -f

# mound partition and start feeding it as the installer should have done it
mount ${loopdev}p4 /mnt

# Enable quota management and create subvolumes
btrfs quota enable /mnt
btrfs subvolume create /mnt/@
btrfs subvolume create /mnt/@/.snapshots

#mkdir -p /mnt/.snapshots
mkdir -p /mnt/@/.snapshots/1

btrfs subvolume snapshot /mnt/@ /mnt/@/.snapshots/1/snapshot

# ID should be computed from a "btrfs subvolume list" command
# default set to the equivalent of the STATE partition (probably a /@/state subvolume would make more sense)
volid=$(btrfs subvolume list --sort path /mnt | head -n 1 | cut -d" " -f2)
btrfs subvolume set-default ${volid} /mnt

# Create snapshot 1 info
date=$(date +'%Y-%m-%d %H:%M:%S')
cat << EOF > /mnt/@/.snapshots/1/info.xml
<?xml version="1.0"?>
<snapshot>
  <type>single</type>
  <num>1</num>
  <date>${date}</date>
  <description>first root filesystem</description>
</snapshot>
EOF

# Dump the built image into the first snapshot subvolume
./build/elemental pull-image --local ${osimage} /mnt/@/.snapshots/1/snapshot/


# Create the quota group
btrfs qgroup create 1/0 /mnt
#chroot /mnt/@/.snapshots/1/snapshot snapper --no-dbus set-config QGROUP=1/0

# set first snapshot as readonly
btrfs property set /mnt/@/.snapshots/1/snapshot ro true

# Some state vars required for the bootloader to boot
grub2-editenv /mnt/@/grub_oem_env set oem_label=COS_OEM persistent_label=COS_PERSISTENT state_label=COS_STATE recovery_label=COS_RECOVERY active_snapshot=/@/.snapshots/1/snapshot

# Copy grub configuration where it is expected
mkdir -p /mnt/@/grub2
cp /mnt/@/.snapshots/1/snapshot/etc/cos/grub.cfg /mnt/@/grub2

sync
umount /mnt
