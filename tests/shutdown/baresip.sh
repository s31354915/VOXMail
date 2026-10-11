#!/bin/sh
set -eu
# Test-only Docker interface selection; the application still creates the
# normal configuration and supervises the real packaged Baresip process.
config_dir=''
previous=''
for arg in "$@"; do
  if [ "$previous" = '-f' ]; then config_dir=$arg; fi
  previous=$arg
done
test -n "$config_dir"
printf '\nnet_interface eth0\n' >> "$config_dir/config"
exec /usr/local/bin/baresip -v -s "$@"
