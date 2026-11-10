#!/bin/sh
# Opt-in only: change the disposable upstream clone, never the source checkout.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
export VERIFY_STEERING_NATIVE_PATCH=1
UPSTREAM_TEST_RACE=${UPSTREAM_TEST_RACE:-1}
export UPSTREAM_TEST_RACE
UPSTREAM_TEST_FILTER=${UPSTREAM_TEST_FILTER:-'^TestUpstreamLifecycle$/^native-steering-admission$'}
export UPSTREAM_TEST_FILTER
exec "$ROOT/scripts/test-upstream.sh"
