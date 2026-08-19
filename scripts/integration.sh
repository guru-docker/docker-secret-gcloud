#!/usr/bin/env bash
#
# End-to-end test for the Secret Manager secret provider.
#
# Builds and installs the managed plugin, then drives it two ways: directly over
# its unix socket, where the driver's own answers are visible, and through
# `docker secret create --driver` in a swarm, which is how it is really used.
#
# Cases that need no credentials always run. Set the following to also read a
# real secret:
#
#   GOOGLE_CLOUD_PROJECT   project holding the secret
#   GCLOUD_SECRET          id of an existing secret in that project
#   GCLOUD_EXPECTED        its expected value (optional; else any non-empty)
#   GCLOUD_CREDENTIALS     service account / workload identity JSON, inline
#     or GOOGLE_APPLICATION_CREDENTIALS  path to that JSON on the host
#     or nothing at all, on a GCE/GKE host whose service account may read it
#
# Requires: docker with swarm and plugin support, permission to install plugins,
# and curl.
#
#   ./scripts/integration.sh
#
set -euo pipefail

cd "$(dirname "$0")/.."

DOCKER=${DOCKER:-docker}
PLUGIN_NAME=${PLUGIN_NAME:-glabservices/gcloud-secret}
PLUGIN_TAG=${PLUGIN_TAG:-test}
PLUGIN="${PLUGIN_NAME}:${PLUGIN_TAG}"
SECRET=${SECRET:-gcloud-itest-secret}
SERVICE=${SERVICE:-gcloud-itest}

creds_dir=$(mktemp -d)
swarm_created=0
failures=0
socket=""

log()  { echo; echo "=== $*"; }
fail() { echo "!!! FAIL: $*" >&2; failures=$((failures + 1)); }

cleanup() {
	local rc=$?
	log "cleanup"
	$DOCKER service rm "$SERVICE" >/dev/null 2>&1 || true
	$DOCKER secret rm "$SECRET" >/dev/null 2>&1 || true
	$DOCKER plugin disable -f "$PLUGIN" >/dev/null 2>&1 || true
	$DOCKER plugin rm -f "$PLUGIN" >/dev/null 2>&1 || true
	[ "$swarm_created" -eq 1 ] && $DOCKER swarm leave --force >/dev/null 2>&1 || true
	rm -rf "$creds_dir"
	exit $rc
}
trap cleanup EXIT

# --- driving the plugin directly -------------------------------------------

# ask posts a SecretProvider.GetSecret request and prints the raw JSON reply.
# The argument is the request body.
ask() {
	curl -s --max-time 60 --unix-socket "$socket" \
		-H 'Content-Type: application/json' \
		-d "$1" http://localhost/SecretProvider.GetSecret
}

# value_of extracts and decodes the Value of a reply. Go renders []byte as
# base64, so the reply is not readable as it stands.
value_of() {
	local encoded
	encoded=$(printf '%s' "$1" | sed -n 's/.*"Value":"\([^"]*\)".*/\1/p')
	[ -n "$encoded" ] && printf '%s' "$encoded" | base64 -d
}

# refuses asserts that a request is rejected with a message the operator can act
# on, rather than silently returning nothing.
refuses() {
	local name=$1 body=$2 want=$3 reply
	reply=$(ask "$body")

	if ! echo "$reply" | grep -q '"Err"'; then
		fail "$name: expected a rejection, got: $reply"
	elif ! echo "$reply" | grep -qF "$want"; then
		fail "$name: rejection does not mention $want: $reply"
	else
		echo "--- ok: $name"
	fi
}

# --- driving the plugin through swarm --------------------------------------

reset_service() {
	$DOCKER service rm "$SERVICE" >/dev/null 2>&1 || true
	$DOCKER secret rm "$SECRET" >/dev/null 2>&1 || true
}

task_state() {
	$DOCKER service ps "$SERVICE" --no-trunc --format '{{.CurrentState}} {{.Error}}' 2>/dev/null | head -1
}

# wait_for_task blocks until the service's task settles and prints its state. A
# task stuck before "Running" means the driver never answered.
wait_for_task() {
	local state=""
	for _ in $(seq 60); do
		state=$(task_state)
		case "$state" in
			Complete*|Failed*|Rejected*|Shutdown*) echo "$state"; return 0 ;;
		esac
		sleep 1
	done
	echo "${state:-no task}"
	return 1
}

# deliver creates a driver-backed secret with the given `docker secret create`
# arguments, mounts it into a one-shot service, and prints what the container
# read from /run/secrets.
deliver() {
	reset_service

	$DOCKER secret create --driver "$PLUGIN" "$@" "$SECRET" >/dev/null
	$DOCKER service create --detach \
		--name "$SERVICE" \
		--restart-condition none \
		--secret "source=$SECRET,target=probe" \
		busybox sh -c 'cat /run/secrets/probe' >/dev/null

	wait_for_task >/dev/null || true
	$DOCKER service logs --raw "$SERVICE" 2>/dev/null || true
}

# --- setup ------------------------------------------------------------------

command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }

log "pull fixtures"
$DOCKER pull -q busybox

if ! $DOCKER node ls >/dev/null 2>&1; then
	log "init swarm (secret drivers are a swarm feature)"
	$DOCKER swarm init >/dev/null
	swarm_created=1
fi

log "build plugin $PLUGIN"
PLUGIN_NAME="$PLUGIN_NAME" PLUGIN_TAG="$PLUGIN_TAG" DOCKER="$DOCKER" make

if [ -n "${GCLOUD_CREDENTIALS:-}" ]; then
	printf '%s' "$GCLOUD_CREDENTIALS" > "$creds_dir/credentials.json"
elif [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]; then
	cp "$GOOGLE_APPLICATION_CREDENTIALS" "$creds_dir/credentials.json"
fi

log "configure and enable the plugin"
$DOCKER plugin set "$PLUGIN" DEBUG=1
$DOCKER plugin set "$PLUGIN" gcloud.source="$creds_dir"
[ -n "${GOOGLE_CLOUD_PROJECT:-}" ] && \
	$DOCKER plugin set "$PLUGIN" GOOGLE_CLOUD_PROJECT="$GOOGLE_CLOUD_PROJECT"
$DOCKER plugin enable "$PLUGIN"
$DOCKER plugin ls

socket="/run/docker/plugins/$($DOCKER plugin inspect "$PLUGIN" -f '{{.Id}}')/secret-gcloud.sock"
if [ ! -S "$socket" ]; then
	echo "the plugin did not open $socket" >&2
	exit 1
fi

# --- cases that need no credentials ----------------------------------------

if [ -z "${GOOGLE_CLOUD_PROJECT:-}" ]; then
	log "case: a request with no project is refused"
	refuses "no project" '{"SecretName":"api-key"}' "gcloud.project"
fi

log "case: a docker name that is not a valid secret id is refused"
refuses "bad secret id" \
	'{"SecretName":"api key/v3","SecretLabels":{"gcloud.project":"acme-prod"}}' \
	"valid Secret Manager id"

log "case: a malformed resource label is refused"
refuses "bad resource" \
	'{"SecretName":"api-key","SecretLabels":{"gcloud.resource":"acme/api-key"}}' \
	"not a Secret Manager resource"

log "case: an unparseable do_not_reuse label is refused"
refuses "bad do_not_reuse" \
	'{"SecretName":"api-key","SecretLabels":{"gcloud.project":"acme-prod","gcloud.do_not_reuse":"sometimes"}}' \
	"not a boolean"

log "case: a rejected secret fails the task instead of hanging"
reset_service
$DOCKER secret create --driver "$PLUGIN" \
	-l gcloud.resource=not-a-resource "$SECRET" >/dev/null
$DOCKER service create --detach \
	--name "$SERVICE" \
	--restart-condition none \
	--secret "source=$SECRET,target=probe" \
	busybox true >/dev/null
state=$(wait_for_task || true)
case "$state" in
	Complete*) fail "rejected secret: the task started with no secret to mount" ;;
	Failed*|Rejected*) echo "--- ok: a rejected secret fails the task" ;;
	*) fail "rejected secret: task never settled ($state)" ;;
esac

# --- cases that need a real project ----------------------------------------

if [ -z "${GOOGLE_CLOUD_PROJECT:-}" ] || [ -z "${GCLOUD_SECRET:-}" ]; then
	log "skipping the credentialed cases"
	echo "set GOOGLE_CLOUD_PROJECT and GCLOUD_SECRET to exercise a real secret"
else
	log "case: the driver reads a real secret version"
	reply=$(ask "{\"SecretName\":\"$GCLOUD_SECRET\"}")
	got=$(value_of "$reply")
	if [ -z "$got" ]; then
		fail "direct read: nothing came back: $reply"
	elif [ -n "${GCLOUD_EXPECTED:-}" ] && [ "$got" != "$GCLOUD_EXPECTED" ]; then
		fail "direct read: value does not match GCLOUD_EXPECTED"
	else
		echo "--- ok: the driver reads a real secret version (${#got} bytes)"
	fi

	log "case: a full resource name reads the same value"
	resource="projects/$GOOGLE_CLOUD_PROJECT/secrets/$GCLOUD_SECRET/versions/latest"
	reply=$(ask "{\"SecretName\":\"ignored\",\"SecretLabels\":{\"gcloud.resource\":\"$resource\"}}")
	if [ "$(value_of "$reply")" != "$got" ]; then
		fail "resource label: got a different value than the label form: $reply"
	else
		echo "--- ok: a full resource name reads the same value"
	fi

	log "case: a secret that does not exist is refused"
	refuses "missing secret" \
		'{"SecretName":"no-such-secret-here-42"}' \
		"failed to access"

	log "case: the value reaches a container"
	delivered=$(deliver -l "gcloud.secret=$GCLOUD_SECRET")
	if [ "$delivered" != "$got" ]; then
		fail "swarm delivery: container read '${delivered}'; $(task_state)"
	else
		echo "--- ok: the value reaches a container"
	fi
fi

log "result"
if [ "$failures" -ne 0 ]; then
	echo "$failures case(s) failed" >&2
	exit 1
fi
echo "all cases passed"
