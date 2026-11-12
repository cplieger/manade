#!/usr/bin/env bash
# Runs every unit test of the shipped shell here, each as its own process,
# all even after a failure. The filename is the contract: cplieger/ci's
# shell-ci.yaml runs it through bash when it exists. The scope is the docker
# runner image's wrapper, whose zero-argument branch and 60-second bound no
# image smoke test takes: every smoke run passes the script as arguments,
# against a daemon that answers or is refused.
set -u

cd -- "$(dirname -- "$0")" || exit 1

failed=0
ran=0
for t in ./*_test.sh; do
  # An unmatched glob expands to itself: an empty suite is a harness fault.
  if [ ! -f "$t" ]; then
    printf 'harness error: no *_test.sh found in %s\n' "$PWD" >&2
    exit 1
  fi
  printf '=== %s\n' "$(basename "$t")"
  ran=$((ran + 1))
  bash "$t" || failed=$((failed + 1))
  printf '\n'
done

if [ "$failed" -ne 0 ]; then
  printf 'FAILED: %d of %d shell test files failed\n' "$failed" "$ran" >&2
  exit 1
fi
printf 'all %d shell test files passed\n' "$ran"
