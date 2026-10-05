#!/bin/sh
# Runs the Kafka sink's MSK test against Floci (https://floci.io): starts Floci if it is not
# running, then runs the test in a Go container on the same Docker network, because Floci's MSK
# broker is reachable only there. Needs Docker. Run from go/ or from this directory.
set -eu
cd "$(dirname "$0")/../.."          # go/

name=ignatius-floci
if ! docker ps --format '{{.Names}}' | grep -qx "$name"; then
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" -p 127.0.0.1:4566:4566 -v /var/run/docker.sock:/var/run/docker.sock floci/floci:latest >/dev/null
  started=1
fi
trap '[ "${started:-}" = 1 ] && docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
ip=$(docker inspect "$name" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
i=0; until curl -s -o /dev/null "http://127.0.0.1:4566/"; do i=$((i+1)); [ $i -gt 30 ] && { echo "floci did not start" >&2; exit 1; }; sleep 1; done

modcache=$(go env GOMODCACHE)
docker run --rm \
  -e IGNATIUS_TEST_FLOCI_ENDPOINT="http://$ip:4566" \
  -e GOFLAGS=-mod=mod -e GOPROXY=off \
  -v "$PWD":/src -v "$modcache":/go/pkg/mod:ro \
  -w /src/plugins/kafka golang:1.26-alpine \
  go test -count=1 -run MSK -v ./...
