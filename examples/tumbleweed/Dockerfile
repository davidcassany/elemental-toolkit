# run `make build` to build local/elemental-toolkit image
ARG TOOLKIT_REPO
ARG VERSION
FROM ${TOOLKIT_REPO}:${VERSION} as TOOLKIT

# OS base image of our choice
FROM opensuse/tumbleweed:latest as OS
ARG REPO
ARG VERSION
ENV VERSION=${VERSION}

# install kernel, systemd, dracut, grub2 and other required tools
RUN ARCH=$(uname -m); \
    if [[ $ARCH == "aarch64" ]]; then ARCH="arm64"; fi; \
    zypper --non-interactive install --no-recommends -- \
      kernel-default \
      device-mapper \
      dracut \
      grub2 \
      grub2-${ARCH}-efi \
      shim \
      haveged \
      systemd \
      NetworkManager \
      iproute2 \
      openssh-server \
      openssh-clients \
      timezone \
      parted \
      e2fsprogs \
      dosfstools \
      mtools \
      xorriso \
      findutils \
      gptfdisk \
      rsync \
      squashfs \
      lvm2 \
      tar \
      gzip \
      vim \
      which \
      less \
      sudo \
      curl \
      patch \
      sed

# Just add the elemental cli
COPY --from=TOOLKIT /usr/bin/elemental /usr/bin/elemental

# Enable essential services
RUN systemctl enable NetworkManager.service && \
    systemctl enable sshd.service

# This is for automatic testing purposes, do not do this in production.
RUN echo "PermitRootLogin yes" > /etc/ssh/sshd_config.d/rootlogin.conf

# This is patches are fix upstream dracut, see https://github.com/dracutdevs/dracut/pull/2525
ADD patches /

RUN cd /usr/lib/dracut && \
    patch -p 1 -f -i /0001-fix-dmsquash-live-restore-compatibility-with-earlier.patch && \
    patch -p 1 -f -i /0001-fix-overlayfs-split-overlayfs-mount-in-two-steps.patch && \
    rm /*.patch

# Generate initrd with required elemental services
RUN elemental --debug init -f 

# Update os-release file with some metadata
RUN echo IMAGE_REPO=\"${REPO}\"         >> /etc/os-release && \
    echo IMAGE_TAG=\"${VERSION}\"           >> /etc/os-release && \
    echo IMAGE=\"${REPO}:${VERSION}\" >> /etc/os-release && \
    echo TIMESTAMP="`date +'%Y%m%d%H%M%S'`" >> /etc/os-release && \
    echo GRUB_ENTRY_NAME=\"Elemental\" >> /etc/os-release

# Good for validation after the build
CMD /bin/bash


