#!/usr/bin/env bash
# Shared assertions for the two observation steps of
# probe-multus-pinned-ip. Each step does
# `source "${GITHUB_ACTION_PATH}/lib.sh"`; do not execute this file.
#
# Multus MERGES the per-pod "ips" override with the address that the
# NAD's own ipam plugin supplies. It does not replace that address. The
# interface thus holds two addresses, and their order is not
# guaranteed. Tests must look for an address in the set, and must not
# read one position of the set.

set -euo pipefail

# ip_to_int prints the 32-bit value of a dotted-quad IPv4 address.
ip_to_int() {
  local a b c d
  local IFS=.
  read -r a b c d <<<"$1"
  echo $(( (a << 24) + (b << 16) + (c << 8) + d ))
}

# assert_merged_addresses fails if the observed addresses do not hold
# BOTH the requested address and one address from the NAD's own ipam
# pool. An entry can carry a prefix; the prefix is removed first.
# usage: assert_merged_addresses <context> <requested-ip> <pool-start> <pool-end> [observed...]
assert_merged_addresses() {
  local context="$1" requested_ip="$2" pool_start="$3" pool_end="$4"
  shift 4
  local observed=("$@")

  local start_int end_int requested_int
  start_int="$(ip_to_int "${pool_start}")"
  end_int="$(ip_to_int "${pool_end}")"
  requested_int="$(ip_to_int "${requested_ip}")"

  # A requested address from inside the pool would make the two tests
  # below satisfy each other, and the probe would show nothing.
  if [ "${requested_int}" -ge "${start_int}" ] && [ "${requested_int}" -le "${end_int}" ]; then
    echo "::error::the probe inputs are wrong: the requested address ${requested_ip} is inside the pool ${pool_start}-${pool_end}, thus the merged pool address cannot be told from the requested address" >&2
    return 1
  fi

  local found_requested="" found_pool="" entry ip ip_int
  for entry in "${observed[@]}"; do
    ip="${entry%%/*}"
    [ -n "${ip}" ] || continue
    if [ "${ip}" = "${requested_ip}" ]; then
      found_requested="${ip}"
      continue
    fi
    ip_int="$(ip_to_int "${ip}")"
    if [ "${ip_int}" -ge "${start_int}" ] && [ "${ip_int}" -le "${end_int}" ]; then
      found_pool="${ip}"
    fi
  done

  if [ -z "${found_requested}" ]; then
    echo "::error::FAIL observed=[${observed[*]}] expected to contain ${requested_ip} -- Multus did not honour the ips field of the default-network annotation, thus the tracker cannot pin its address (${context})" >&2
    return 1
  fi
  if [ -z "${found_pool}" ]; then
    echo "::error::FAIL observed=[${observed[*]}] expected to also contain one address from ${pool_start}-${pool_end} -- the ipam address of the NAD is absent, thus this probe no longer shows the merge (${context})" >&2
    return 1
  fi
  echo "PASS observed=[${observed[*]}] requested=${found_requested} pool=${found_pool} (${context})"
}
