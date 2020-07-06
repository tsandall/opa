#!/usr/bin/env bash
# Script to draft and edit OPA GitHub releases. Assumes execution environment is Github Action runner.

set -x

usage() {
    echo "github-release.sh [--asset-dir=<path>] [--tag=<git tag>]"
    echo "    Default --asset-dir is $PWD and --tag $TAG_NAME "
}

TAG_NAME=${TAG_NAME}
ASSET_DIR=${PWD:-"./"}

for i in "$@"; do
    case $i in
    --asset-dir=*)
        ASSET_DIR="${i#*=}"
        shift
        ;;
    --tag=*)
        TAG_NAME="${i#*=}"
        shift
        ;;
    *)
        usage
        exit 1
        ;;
    esac
done

# Collect a list of opa binaries (expect binaries in the form: opa_<platform>_<arch>[extension])
ASSETS=()
for asset in "${ASSET_DIR}"/opa_*_*; do
  ASSETS+=("-a" "$asset")
done

if hub release show "${TAG_NAME}" > /dev/null; then
  # Occurs when the tag is created via GitHub UI w/ a release
  # Use -m "" to preserve the existing text.
  hub release edit "${ASSETS[@]}" -m "" "${TAG_NAME}"
else
  # Create a draft release
  # TODO: Auto-populate the text from CHANGELOG
  hub release create "${ASSETS[@]}" -m "${TAG_NAME}" --draft "${TAG_NAME}"
fi
