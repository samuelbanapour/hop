#!/bin/sh
# Remove AppleDouble sidecar files that macOS writes into .git on exFAT/FAT
# volumes. See the project memory note on exFAT + AppleDouble corruption.
find "$(git rev-parse --git-dir 2>/dev/null || echo .git)" -name '._*' -delete 2>/dev/null
exit 0
