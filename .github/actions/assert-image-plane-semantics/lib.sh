#!/usr/bin/env bash
# Shared helpers for assert-image-plane-semantics' step groups. Sourced,
# not executed - every step group does `source "${GITHUB_ACTION_PATH}/lib.sh"`
# before using these.
#
# jp is the one jsonpath entry point every step uses: SSA prunes a field
# holding its zero value from the object entirely (kezio has been bitten by
# this before), so a jsonpath read against an absent field must come back
# empty rather than error - `2>/dev/null || true` is load-bearing, not
# decorative, and every caller must still explicitly default an empty
# result before using it (jp itself cannot know 0 vs "" vs "<none>" is the
# right default for a given caller).

set -euo pipefail

# jp reads one jsonpath field off kind/name in ns, printing "" if the
# object or the field is absent.
jp() {
  local ns="$1" kind="$2" name="$3" path="$4"
  kubectl get "${kind}" "${name}" -n "${ns}" -o jsonpath="${path}" 2>/dev/null || true
}

# count_objects counts kind objects in ns matching selector, printing 0
# rather than erroring when the list is empty or the type has no matches.
count_objects() {
  local ns="$1" kind="$2" selector="$3"
  kubectl get "${kind}" -n "${ns}" -l "${selector}" -o json 2>/dev/null | jq '(.items // []) | length'
}

# wait_for_image_ready polls name's status.state until Ready, Failed, or
# timeout_seconds elapses, dumping diagnostics and failing on either of the
# latter two.
wait_for_image_ready() {
  local ns="$1" name="$2" timeout_seconds="$3"
  local deadline state
  deadline=$(( $(date +%s) + timeout_seconds ))
  while true; do
    state="$(jp "${ns}" images.kezio.kojuro.date "${name}" '{.status.state}')"
    echo "image ${name} state=${state:-<none>}"
    if [ "${state}" = "Ready" ]; then
      return 0
    fi
    if [ "${state}" = "Failed" ]; then
      echo "::error::Image ${name} reached Failed" >&2
      dump_image_diagnostics "${ns}" "${name}"
      return 1
    fi
    if [ "$(date +%s)" -ge "${deadline}" ]; then
      echo "::error::Image ${name} did not reach Ready within ${timeout_seconds}s" >&2
      dump_image_diagnostics "${ns}" "${name}"
      return 1
    fi
    sleep 3
  done
}

# wait_for_gone polls until kind/name in ns no longer exists, failing after
# timeout_seconds.
wait_for_gone() {
  local kind="$1" name="$2" ns="$3" timeout_seconds="$4"
  local deadline
  deadline=$(( $(date +%s) + timeout_seconds ))
  while kubectl get "${kind}" "${name}" -n "${ns}" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "${deadline}" ]; then
      echo "::error::${kind} ${name} was not removed within ${timeout_seconds}s" >&2
      return 1
    fi
    sleep 3
  done
}

# assert_shared_content_singleton asserts that content_name is the only
# PartitionContent in ns, with exactly one content PVC and one publish Job
# - the dedupe invariant: an Image sharing already-Ready content never
# grows a second PartitionContent, PVC, or publish Job for it.
assert_shared_content_singleton() {
  local ns="$1" content_name="$2"
  local pc_count pvc_count job_count ok=1

  pc_count="$(kubectl get partitioncontents.kezio.kojuro.date -n "${ns}" -o json | jq '(.items // []) | length')"
  pvc_count="$(count_objects "${ns}" pvc "app.kubernetes.io/component=partition-content")"
  job_count="$(count_objects "${ns}" job "app.kubernetes.io/component=partition-content-publish")"

  if [ "${pc_count:-0}" != "1" ]; then
    echo "::error::expected exactly 1 PartitionContent in ${ns}, found ${pc_count:-0}" >&2
    ok=0
  fi
  if [ "${pvc_count:-0}" != "1" ]; then
    echo "::error::expected exactly 1 content PVC in ${ns}, found ${pvc_count:-0}" >&2
    ok=0
  fi
  if [ "${job_count:-0}" != "1" ]; then
    echo "::error::expected exactly 1 publish Job in ${ns}, found ${job_count:-0}" >&2
    ok=0
  fi
  if ! kubectl get partitioncontents.kezio.kojuro.date "${content_name}" -n "${ns}" >/dev/null 2>&1; then
    echo "::error::PartitionContent ${content_name} does not exist" >&2
    ok=0
  fi

  if [ "${ok}" != "1" ]; then
    dump_content_diagnostics "${ns}" "${content_name}"
    return 1
  fi
}

# dump_image_diagnostics dumps name and every kezio object/Job/Pod state
# useful to diagnose why it failed to reach Ready (or grew an ingest Job it
# should not have).
dump_image_diagnostics() {
  local ns="$1" name="$2"
  echo "::group::Image ${name}"
  kubectl get images.kezio.kojuro.date "${name}" -n "${ns}" -o yaml || true
  echo "::endgroup::"
  echo "::group::PartitionContents (${ns})"
  kubectl get partitioncontents.kezio.kojuro.date -n "${ns}" -o yaml || true
  echo "::endgroup::"
  echo "::group::Jobs (${ns})"
  kubectl get jobs -n "${ns}" -o wide || true
  echo "::endgroup::"
  echo "::group::ingest job pods/logs"
  kubectl describe pods -n "${ns}" -l "app.kubernetes.io/component=image-ingest-job" || true
  kubectl logs -n "${ns}" -l "app.kubernetes.io/component=image-ingest-job" --all-containers --tail=1000 || true
  echo "::endgroup::"
  echo "::group::events (${ns})"
  kubectl get events -n "${ns}" --sort-by=.lastTimestamp || true
  echo "::endgroup::"
}

# dump_content_diagnostics dumps content_name and its owned objects -
# useful for both the dedupe-singleton and deletion-semantics failures.
dump_content_diagnostics() {
  local ns="$1" content_name="$2"
  echo "::group::PartitionContent ${content_name}"
  kubectl get partitioncontents.kezio.kojuro.date "${content_name}" -n "${ns}" -o yaml || true
  echo "::endgroup::"
  echo "::group::PartitionContents (${ns}, all)"
  kubectl get partitioncontents.kezio.kojuro.date -n "${ns}" -o wide || true
  echo "::endgroup::"
  echo "::group::Images referencing ${content_name} (${ns}, all Images)"
  kubectl get images.kezio.kojuro.date -n "${ns}" -o yaml || true
  echo "::endgroup::"
  echo "::group::PVCs (${ns})"
  kubectl get pvc -n "${ns}" -o wide || true
  echo "::endgroup::"
  echo "::group::Jobs (${ns})"
  kubectl get jobs -n "${ns}" -o wide || true
  echo "::endgroup::"
  echo "::group::events (${ns})"
  kubectl get events -n "${ns}" --sort-by=.lastTimestamp || true
  echo "::endgroup::"
}

# dump_seeder_diagnostics dumps deployment_name and its pods/logs.
dump_seeder_diagnostics() {
  local ns="$1" deployment_name="$2"
  local selector="app.kubernetes.io/name=kezio,app.kubernetes.io/component=partition-content-seeder"
  echo "::group::seeder Deployment ${deployment_name}"
  kubectl get deployment "${deployment_name}" -n "${ns}" -o yaml || true
  echo "::endgroup::"
  echo "::group::seeder pods"
  kubectl get pods -n "${ns}" -l "${selector}" -o wide || true
  kubectl describe pods -n "${ns}" -l "${selector}" || true
  echo "::endgroup::"
  echo "::group::seeder pod logs"
  kubectl logs -n "${ns}" -l "${selector}" --all-containers --tail=1000 || true
  echo "::endgroup::"
  echo "::group::events (${ns})"
  kubectl get events -n "${ns}" --sort-by=.lastTimestamp || true
  echo "::endgroup::"
}
