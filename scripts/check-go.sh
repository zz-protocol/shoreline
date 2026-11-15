#!/bin/sh
set -e

if [ "$SKIP_FMT" != "1" ]; then
  bad=$(gofmt -l .)
  if [ -n "$bad" ]; then
    echo "gofmt needed:" >&2
    echo "$bad" >&2
    exit 1
  fi
fi

if [ "$SKIP_VET" != "1" ]; then
  go vet ./...
fi

if [ -n "$RUN_ARG" ]; then
  go test -count="$COUNT" -run "$RUN_ARG" ./...
else
  go test -count="$COUNT" ./...
fi
