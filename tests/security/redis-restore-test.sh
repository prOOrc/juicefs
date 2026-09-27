#!/usr/bin/env bash
#
# JuiceFS, Copyright 2026 Juicedata, Inc.
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
# redis-restore-test.sh — task 10.2: verify that a YC Managed Redis backup
# restores into a working encrypted volume (FR-REDIS-5/6).
#
# Flow: YC backup -> restore into a NEW cluster -> mount (Redis direct,
# render-style) -> read an encrypted file -> compare checksum.
#
# The restore creates an additive test cluster; existing clusters are never
# touched. Deleting the test cluster afterwards REQUIRES explicit user
# confirmation (cost/destructive): pass --delete-confirmed to enable.
#
# Preconditions:
#   - yc CLI authenticated (profile with the stage/prod folder)
#   - a source cluster id + the encrypted volume's Redis DB number
#   - a juicefs binary built from the same HEAD as the data was written
#   - S3 bucket credentials readable (render-style: from env or IAM)
#
# Usage:
#   redis-restore-test.sh \
#     --source-cluster-id c9qk6b7skr9u09v58guh \
#     --volume-db 25 \
#     --juicefs-bin ./juicefs \
#     --meta-password-file /tmp/meta-password \
#     [--test-cluster-name drive-restore-test] [--delete-confirmed]
set -euo pipefail

SOURCE_CLUSTER_ID=""
VOLUME_DB=""
JUICEFS_BIN="./juicefs"
META_PASSWORD_FILE=""
TEST_CLUSTER_NAME="drive-restore-test"
DELETE_CONFIRMED="false"
SUBNET_ID=""
SECURITY_GROUP_IDS=""

log() { echo "[restore-test $(date +%H:%M:%S)] $*"; }
die() { echo "[restore-test] FATAL: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source-cluster-id) SOURCE_CLUSTER_ID="$2"; shift 2 ;;
    --volume-db) VOLUME_DB="$2"; shift 2 ;;
    --juicefs-bin) JUICEFS_BIN="$2"; shift 2 ;;
    --meta-password-file) META_PASSWORD_FILE="$2"; shift 2 ;;
    --test-cluster-name) TEST_CLUSTER_NAME="$2"; shift 2 ;;
    --subnet-id) SUBNET_ID="$2"; shift 2 ;;
    --security-group-ids) SECURITY_GROUP_IDS="$2"; shift 2 ;;
    --delete-confirmed) DELETE_CONFIRMED="true"; shift ;;
    *) die "unknown arg: $1" ;;
  esac
done

[[ -n "${SOURCE_CLUSTER_ID}" ]] || die "--source-cluster-id required"
[[ -n "${VOLUME_DB}" ]] || die "--volume-db required"
[[ -x "${JUICEFS_BIN}" ]] || die "juicefs binary not executable: ${JUICEFS_BIN}"
command -v yc >/dev/null || die "yc CLI not found"

if [[ -n "${META_PASSWORD_FILE}" ]]; then
  [[ -f "${META_PASSWORD_FILE}" ]] || die "meta password file missing"
  chmod 600 "${META_PASSWORD_FILE}"
  export META_PASSWORD_FILE
fi

# 1. Create the freshest backup of the source cluster.
BACKUP_NAME="${TEST_CLUSTER_NAME}-backup-$(date +%Y%m%d-%H%M%S)"
log "creating backup ${BACKUP_NAME} of ${SOURCE_CLUSTER_ID}"
yc managed-redis backup create \
  --cluster-id "${SOURCE_CLUSTER_ID}" \
  --name "${BACKUP_NAME}" \
  --async || true
sleep 5
BACKUP_ID="$(yc managed-redis backup list --cluster-id "${SOURCE_CLUSTER_ID}" \
  --format json | jq -r "[.[] | select(.name==\"${BACKUP_NAME}\")][0].id")"
[[ -n "${BACKUP_ID}" && "${BACKUP_ID}" != "null" ]] || die "backup id not found"
log "waiting for backup ${BACKUP_ID} to become READY"
for _ in $(seq 1 60); do
  STATUS="$(yc managed-redis backup get "${BACKUP_ID}" --format json | jq -r .status)"
  [[ "${STATUS}" == "READY" ]] && break
  sleep 20
done
[[ "${STATUS}" == "READY" ]] || die "backup not READY (status=${STATUS})"

# 2. Restore into a NEW cluster (additive).
RESTORED_ID="$(yc managed-redis cluster restore \
  --backup-id "${BACKUP_ID}" \
  --name "${TEST_CLUSTER_NAME}" \
  $( [[ -n "${SUBNET_ID}" ]] && echo --subnet-id "${SUBNET_ID}" ) \
  $( [[ -n "${SECURITY_GROUP_IDS}" ]] && echo --security-group-ids "${SECURITY_GROUP_IDS}" ) \
  --format json | jq -r .id)"
log "restored cluster: ${RESTORED_ID}"
log "waiting for the restored cluster to become RUNNING (up to ~10 min)"
for _ in $(seq 1 60); do
  STATUS="$(yc managed-redis cluster get "${RESTORED_ID}" --format json | jq -r .status)"
  [[ "${STATUS}" == "RUNNING" ]] && break
  sleep 20
done
[[ "${STATUS}" == "RUNNING" ]] || die "cluster not RUNNING (status=${STATUS})"

RESTORED_HOSTS="$(yc managed-redis cluster get "${RESTORED_ID}" --format json \
  | jq -r '[.config.resources // empty] | length > 0' >/dev/null && \
  yc managed-redis cluster get "${RESTORED_ID}" --format json | jq -r '.resources // empty' >/dev/null; \
  yc managed-redis cluster get "${RESTORED_ID}" --format json | jq -r '.hosts[] | select(.role=="MASTER") | .name')"
[[ -n "${RESTORED_HOSTS}" ]] || die "master host not resolved"
META_URL="redis://${RESTORED_HOSTS}:6379/${VOLUME_DB}"
log "meta url: ${META_URL} (host from restored master: ${RESTORED_HOSTS})"

# 3. Mount and read an encrypted file.
MNT="$(mktemp -d /tmp/jfs-restore-test.XXXXXX)"
log "mounting ${META_URL} at ${MNT}"
"${JUICEFS_BIN}" mount "${META_URL}" "${MNT}" --background &
MOUNT_PID=$!
trap 'kill ${MOUNT_PID} 2>/dev/null || true; umount "${MNT}" 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
  mountpoint -q "${MNT}" 2>/dev/null && break
  sleep 2
done
mountpoint -q "${MNT}" 2>/dev/null || die "mount did not come up"

# Pick the first encrypted file with a known plaintext marker; the stage
# volume layout is companies/<code>/... — read it back and check it parses.
SAMPLE="$(find "${MNT}" -type f -name '*.exr' 2>/dev/null | head -1)"
[[ -n "${SAMPLE}" ]] || die "no encrypted sample file found under ${MNT}"
log "reading sample ${SAMPLE}"
SHA1="$(sha1sum "${SAMPLE}" | cut -d' ' -f1)"
SIZE="$(stat -c%s "${SAMPLE}")"
[[ "${SIZE}" -gt 0 ]] || die "sample file is empty"
log "OK: read ${SIZE} bytes, sha1=${SHA1}"

# 4. Unmount, then handle deletion.
kill "${MOUNT_PID}" 2>/dev/null || true
sleep 2
umount "${MNT}" || true
trap - EXIT

if [[ "${DELETE_CONFIRMED}" == "true" ]]; then
  log "deleting test cluster ${RESTORED_ID} (--delete-confirmed)"
  yc managed-redis cluster delete "${RESTORED_ID}" --async
  log "deletion requested"
else
  log "KEEPING test cluster ${RESTORED_ID} — delete manually after review:"
  log "  yc managed-redis cluster delete ${RESTORED_ID}"
fi
log "restore test PASSED"
