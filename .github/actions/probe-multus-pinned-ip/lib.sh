#!/usr/bin/env bash
# Shared assertions for the two observation steps of
# probe-multus-pinned-ip. Each step does
# `source "${GITHUB_ACTION_PATH}/lib.sh"`; do not execute this file.
#
# host-local refuses a requested address that lies outside its own
# range: it fails the sandbox with "failed to allocate all requested
# IPs". A pinned address on a host-local NAD must thus sit INSIDE that
# NAD's pool. What the interface holds after that is not fixed -
# host-local can hand back the requested address alone, or a second
# pool address with it - so these assertions look for the requested
# address in the observed set, never at the size of the set and never
# at one position of it.

set -euo pipefail

# ip_to_int prints the 32-bit value of a dotted-quad IPv4 address.
ip_to_int() {
  local a b c d
  local IFS=.
  read -r a b c d <<<"$1"
  echo $(( (a << 24) + (b << 16) + (c << 8) + d ))
}

# assert_pinned_address fails if the observed addresses do not hold the
# requested address. An entry can carry a prefix; the prefix is removed
# first. The whole observed set is printed either way, so a run shows
# what the ipam plugin actually produced.
# usage: assert_pinned_address <context> <requested-ip> <pool-start> <pool-end> [observed...]
assert_pinned_address() {
  local context="$1" requested_ip="$2" pool_start="$3" pool_end="$4"
  shift 4
  local observed=("$@")

  local start_int end_int requested_int
  start_int="$(ip_to_int "${pool_start}")"
  end_int="$(ip_to_int "${pool_end}")"
  requested_int="$(ip_to_int "${requested_ip}")"

  # The probe mirrors the shipped NAD, where the pinned address is
  # always inside the pool. A request from outside it never reaches the
  # assertions below - host-local rejects the sandbox instead.
  if [ "${requested_int}" -lt "${start_int}" ] || [ "${requested_int}" -gt "${end_int}" ]; then
    echo "::error::the probe inputs are wrong: the requested address ${requested_ip} is outside the pool ${pool_start}-${pool_end}, which host-local rejects with \"failed to allocate all requested IPs\"" >&2
    return 1
  fi

  local found_requested="" others=() entry ip
  for entry in "${observed[@]}"; do
    ip="${entry%%/*}"
    [ -n "${ip}" ] || continue
    if [ "${ip}" = "${requested_ip}" ]; then
      found_requested="${ip}"
      continue
    fi
    others+=("${ip}")
  done

  if [ -z "${found_requested}" ]; then
    echo "::error::FAIL observed=[${observed[*]}] expected to contain ${requested_ip} -- Multus did not honour the ips field of the default-network annotation, thus the tracker cannot pin its address (${context})" >&2
    return 1
  fi
  echo "PASS observed=[${observed[*]}] requested=${found_requested} other=[${others[*]-}] (${context})"
}
