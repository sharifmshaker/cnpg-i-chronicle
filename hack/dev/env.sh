#!/usr/bin/env bash
#
# Print the rclone configuration for the dev object store as shell exports.
#
#   eval $(./hack/dev/env.sh)
#   rclone tree dev:chronicle
#
# Environment variables rather than a config file, so nothing is written to
# ~/.config/rclone and the settings vanish with the shell.

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$HERE/config.sh"

dev_rclone_env
