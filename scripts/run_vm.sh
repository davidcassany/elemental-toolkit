#!/bin/bash

set -e

SCRIPT=$(realpath -s "${0}")
SCRIPTS_PATH=$(dirname "${SCRIPT}")
TESTS_PATH=$(realpath -s "${SCRIPTS_PATH}/../tests")

: "${ELMNTL_FIRMWARE:=/usr/share/qemu/ovmf-x86_64.bin}"
: "${ELMNTL_FWDIP:=127.0.0.1}"
: "${ELMNTL_FWDPORT:=2222}"
: "${ELMNTL_MEMORY:=2048}"
: "${ELMNTL_LOGFILE:=${TESTS_PATH}/serial.log}"
: "${ELMNTL_PIDFILE:=${TESTS_PATH}/testvm.pid}"
: "${ELMNTL_TESTDISK:=${TESTS_PATH}/testdisk.qcow2}"
: "${ELMNTL_DISPLAY:=none}"
: "${ELMNTL_ACCEL:=kvm}"

function _abort {
    echo "$@" && exit 1
}

function start {
  local base_disk=$1
  local usrnet_arg="-netdev user,id=user0,hostfwd=tcp:${ELMNTL_FWDIP}:${ELMNTL_FWDPORT}-:22 -device virtio-net-pci,netdev=user0"
  local accel_arg="-accel ${ELMNTL_ACCEL}"
  local memory_arg="-m ${ELMNTL_MEMORY}"
  local firmware_arg="-bios ${ELMNTL_FIRMWARE} -drive if=pflash,format=raw,readonly=on,file=${ELMNTL_FIRMWARE}"
  local disk_arg="-hda ${ELMNTL_TESTDISK}"
  local serial_arg="-serial file:${ELMNTL_LOGFILE}"
  local pidfile_arg="-pidfile ${ELMNTL_PIDFILE}"
  local display_arg="-display ${ELMNTL_DISPLAY}"
  local daemon_arg="-daemonize"
  local machine_arg="-machine type=q35"
  local cpu_arg
  local vmpid

  [ -f "${base_disk}" ] || _abort "Disk not found: ${base_disk}"

  if [ -f "${ELMNTL_PIDFILE}" ]; then
    vmpid=$(cat "${ELMNTL_PIDFILE}")
    if ps -p ${vmpid} > /dev/null; then
      echo "test VM is already running with pid ${vmpid}"
      exit 0
    else
      echo "removing outdated pidfile ${ELMNTL_PIDFILE}"
      rm "${ELMNTL_PIDFILE}"
    fi
  fi

  [ "kvm" == "${ELMNTL_ACCEL}" ] && cpu_arg="-cpu host"
  [ "hvf" == "${ELMNTL_ACCEL}" ] && cpu_arg="-cpu host"

  qemu-img create -f qcow2 -b "${base_disk}" -F qcow2 "${ELMNTL_TESTDISK}" > /dev/null
  qemu-system-x86_64 ${disk_arg} ${firmware_arg} ${usrnet_arg} ${kvm_arg} \
      ${memory_arg} ${graphics_arg} ${serial_arg} ${pidfile_arg} ${daemon_arg} \
      ${display_arg} ${machine_arg} ${accel_arg} ${cpu_arg}
}

function stop {
  local vmpid
  local killprog

  if [ -f "${ELMNTL_PIDFILE}" ]; then
    vmpid=$(cat "${ELMNTL_PIDFILE}")
    killprog=$(which kill)
    if ${killprog} --version | grep -q util-linux; then
        ${killprog} --verbose --timeout 1000 TERM --timeout 5000 KILL --signal QUIT ${vmpid}
    else
        ${killprog} -9 ${vmpid}
    fi
    rm -f "${ELMNTL_PIDFILE}"
  else
    echo "No pidfile ${ELMNTL_PIDFILE} found, nothing to stop"
  fi
}

function clean {
  ([ -f "${ELMNTL_LOGFILE}" ] && rm -f "${ELMNTL_LOGFILE}") || true
  ([ -f "${ELMNTL_TESTDISK}" ] && rm -f "${ELMNTL_TESTDISK}") || true
}

cmd=$1
disk=$2

case $cmd in
  start)
    start "${disk}"
    ;;
  stop)
    stop
    ;;
  clean)
    clean
    ;;
  *)
    _abort "Unknown command: ${cmd}"
    ;;
esac

exit 0
