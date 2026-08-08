#!/usr/bin/env bash
# Copyright 2026 Date Huang.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Runs the real dnsmasq supervisor (internal/bootd/lab_test.go's
# TestDnsmasqLab) against a real PXE-shaped client
# (internal/bootd/lab_client_test.go's TestDnsmasqLabClient), joined by
# a veth pair split across two network namespaces, and asserts on the
# packets that actually cross the wire. Not a GitHub Actions job: it
# needs CAP_NET_ADMIN/CAP_NET_RAW/CAP_SYS_ADMIN (network namespaces,
# veth, dnsmasq's own DHCP sockets) a shared CI runner does not grant.
# Run it locally, or in a privileged container - see the example below.
#
# It exercises the two scenarios documented in
# docs/physical-lab-deployment.md:
#
#   - Scenario 1: proxyDHCP alongside an on-segment DHCP server. An
#     enrolled MAC gets a proxyDHCP DHCPOFFER (yiaddr 0.0.0.0,
#     siaddr/file naming bootd's own TFTP/GRUB chain); an unenrolled
#     MAC gets nothing at all.
#   - Scenario 2: bootd's own lease mode (BOOTD_LAB_LEASE_MODE=1). An
#     enrolled MAC gets a DHCPOFFER with a real, non-zero leased
#     address; an unenrolled MAC gets nothing at all, same as scenario
#     1 - the MAC gate does not relax in lease mode.
#
# Usage (from the repository root, as root, with dnsmasq and iproute2
# installed):
#
#   ./hack/bootd-packet-lab.sh
#
# Or in a throwaway privileged container, which is how this script was
# verified (no host package installs):
#
#   docker run --rm --privileged -v "$PWD":/workspace -w /workspace \
#     golang:1.26 bash -c \
#     'apt-get update && apt-get install -y --no-install-recommends dnsmasq-base iproute2 &&
#      ./hack/bootd-packet-lab.sh'

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
	echo "bootd-packet-lab.sh must run as root (network namespaces, veth, dnsmasq)" >&2
	exit 1
fi
for bin in ip go; do
	command -v "$bin" >/dev/null || {
		echo "required command not found: $bin" >&2
		exit 1
	}
done
DNSMASQ_PATH=${BOOTD_DNSMASQ_PATH:-/usr/sbin/dnsmasq}
[[ -x $DNSMASQ_PATH ]] || {
	echo "dnsmasq binary not found or not executable at $DNSMASQ_PATH" >&2
	exit 1
}

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORKDIR=$(mktemp -d /tmp/bootd-packet-lab.XXXXXX)
TESTBIN="$WORKDIR/bootd.test"
FAIL=0

cleanup() {
	set +e
	[[ -n ${SERVER_PID:-} ]] && kill "$SERVER_PID" 2>/dev/null
	[[ -n ${SERVER_PID:-} ]] && wait "$SERVER_PID" 2>/dev/null
	ip netns del "$NS_SRV" 2>/dev/null
	ip netns del "$NS_CLI" 2>/dev/null
	rm -rf "$WORKDIR"
}
trap cleanup EXIT

echo "== building the packet lab's test binary =="
go test -c -o "$TESTBIN" "$REPO_ROOT/internal/bootd"

# run_scenario sets up a fresh netns pair, starts the lab dnsmasq with
# the given extra env assignments (passed through to TestDnsmasqLab -
# see lab_test.go's BOOTD_LAB_* variables), then drives the client
# against an enrolled and a denied MAC and asserts the outcome each
# should produce. Each scenario gets its own netns pair rather than
# reusing one, so a stuck dnsmasq or leftover route from a previous
# scenario can never leak into the next assertion.
run_scenario() {
	local name=$1 enrolled_mac=$2 denied_mac=$3 offer_timeout=$4 client_expect_extra=$5
	shift 5
	local extra_env=("$@")
	local client_extra=()
	# shellcheck disable=SC2206 # word-splitting is intentional: each element is one KEY=VAL assignment with no embedded spaces
	[[ -n $client_expect_extra ]] && client_extra=($client_expect_extra)

	NS_SRV="kezio-lab-srv-$$" NS_CLI="kezio-lab-cli-$$"
	local veth_srv="veth-srv" veth_cli="veth-cli"
	local run_dir="$WORKDIR/$name-rundir" ctrl_fifo="$WORKDIR/$name-ctrl" srv_log="$WORKDIR/$name-server.log"
	mkdir -p "$run_dir"
	mkfifo "$ctrl_fifo"

	ip netns add "$NS_SRV"
	ip netns add "$NS_CLI"
	ip link add "$veth_srv" netns "$NS_SRV" type veth peer name "$veth_cli" netns "$NS_CLI"
	ip -n "$NS_SRV" addr add 192.0.2.2/24 dev "$veth_srv"
	ip -n "$NS_SRV" link set "$veth_srv" up
	ip -n "$NS_SRV" link set lo up
	# The client interface deliberately carries no address (a real PXE
	# ROM has none yet either), so the kernel's reverse-path filter
	# would otherwise treat dnsmasq's broadcast replies as martian
	# (no route back through an addressless interface) and silently
	# drop them before they ever reach the client's socket - lab
	# observed: tcpdump sees the reply, the client's UDP socket does
	# not. Each network namespace starts with its own copy of these
	# sysctls, so this must run inside the client namespace, not once
	# on the host.
	ip netns exec "$NS_CLI" sysctl -qw net.ipv4.conf.all.rp_filter=0
	ip netns exec "$NS_CLI" sysctl -qw net.ipv4.conf.default.rp_filter=0
	ip netns exec "$NS_CLI" sysctl -qw "net.ipv4.conf.$veth_cli.rp_filter=0"
	ip -n "$NS_CLI" link set "$veth_cli" up
	ip -n "$NS_CLI" link set lo up

	echo "-- [$name] starting bootd's dnsmasq supervisor --"
	ip netns exec "$NS_SRV" env \
		BOOTD_LAB=1 \
		BOOTD_LAB_IFACE="$veth_srv" \
		BOOTD_LAB_SERVER_IP=192.0.2.2 \
		BOOTD_LAB_CIDR=192.0.2.0/24 \
		BOOTD_LAB_RUN_DIR="$run_dir" \
		BOOTD_DNSMASQ_PATH="$DNSMASQ_PATH" \
		BOOTD_LAB_CTRL="$ctrl_fifo" \
		"${extra_env[@]}" \
		"$TESTBIN" -test.run TestDnsmasqLab -test.v >"$srv_log" 2>&1 &
	SERVER_PID=$!

	# Opening the FIFO for writing blocks until TestDnsmasqLab's own
	# os.Open (reading) reaches its matching call - see lab_test.go.
	# That happens right after Dnsmasq.Start is kicked off in a
	# goroutine, so this does not race dnsmasq's own startup.
	exec 3>"$ctrl_fifo"

	local waited=0
	until grep -q "dnsmasq started" "$srv_log" 2>/dev/null; do
		if ! kill -0 "$SERVER_PID" 2>/dev/null; then
			echo "[$name] server exited before dnsmasq started; log:" >&2
			cat "$srv_log" >&2
			FAIL=1
			exec 3>&-
			return
		fi
		sleep 0.2
		waited=$((waited + 1))
		if ((waited > 50)); then
			echo "[$name] timed out waiting for dnsmasq to start; log:" >&2
			cat "$srv_log" >&2
			FAIL=1
			exec 3>&-
			return
		fi
	done

	echo "allow $enrolled_mac" >&3
	sleep 1 # hostsfile debounce (200ms) plus SIGHUP settling margin

	echo "-- [$name] enrolled MAC $enrolled_mac expects a DHCPOFFER --"
	if ! ip netns exec "$NS_CLI" env \
		BOOTD_LAB_CLIENT=1 \
		BOOTD_LAB_CLIENT_IFACE="$veth_cli" \
		BOOTD_LAB_CLIENT_MAC="$enrolled_mac" \
		BOOTD_LAB_CLIENT_EXPECT=offer \
		BOOTD_LAB_CLIENT_EXPECT_SIADDR=192.0.2.2 \
		BOOTD_LAB_CLIENT_TIMEOUT="$offer_timeout" \
		"${client_extra[@]}" \
		"$TESTBIN" -test.run TestDnsmasqLabClient -test.v; then
		echo "[$name] enrolled-MAC assertion FAILED" >&2
		FAIL=1
	fi

	echo "deny" >&3
	sleep 1

	echo "-- [$name] denied MAC $denied_mac expects no reply --"
	if ! ip netns exec "$NS_CLI" env \
		BOOTD_LAB_CLIENT=1 \
		BOOTD_LAB_CLIENT_IFACE="$veth_cli" \
		BOOTD_LAB_CLIENT_MAC="$denied_mac" \
		BOOTD_LAB_CLIENT_EXPECT=none \
		"$TESTBIN" -test.run TestDnsmasqLabClient -test.v; then
		echo "[$name] denied-MAC assertion FAILED" >&2
		FAIL=1
	fi

	echo "quit" >&3
	exec 3>&-
	wait "$SERVER_PID" || {
		echo "[$name] server exited with an error; log:" >&2
		cat "$srv_log" >&2
		FAIL=1
	}
	unset SERVER_PID

	ip netns del "$NS_SRV"
	ip netns del "$NS_CLI"
}

# Scenario 1's proxyDHCP offer answers almost immediately (no lease to
# check for conflicts), so a short timeout is enough. Its offer carries
# no boot filename - see TestDnsmasqLabClient's own comment on
# BOOTD_LAB_CLIENT_EXPECT_FILE - so client_expect_extra stays empty.
run_scenario "scenario1-proxydhcp" \
	"52:54:00:00:00:01" "52:54:00:00:00:02" 3s ""

# Scenario 2 is a real lease allocation: dnsmasq ICMP-pings the
# candidate address before offering it (lab-observed: ~3s), so the
# offer-side timeout needs headroom the proxyDHCP scenario does not.
run_scenario "scenario2-lease-mode" \
	"52:54:00:00:00:03" "52:54:00:00:00:04" 8s \
	"BOOTD_LAB_CLIENT_EXPECT_FILE=shimx64.efi BOOTD_LAB_CLIENT_EXPECT_LEASE=1" \
	BOOTD_LAB_LEASE_MODE=1

if [[ $FAIL -ne 0 ]]; then
	echo "== bootd packet lab: FAIL ==" >&2
	exit 1
fi
echo "== bootd packet lab: PASS (scenario 1 proxyDHCP, scenario 2 lease mode) =="
