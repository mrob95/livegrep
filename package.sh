#!/bin/bash

# use bazelisk or bazel, whichever is available
if ! command -v bazelisk >/dev/null; then
    if ! command -v bazel >/dev/null; then
        echo "bazel or bazelisk must be installed"
        exit 1
    fi
    bazel=bazel
else
    bazel=bazelisk
fi

set -ex
mkdir -p builds
rev=$(git rev-parse HEAD | head -c10)
builddir="livegrep-$rev"
rm -rf "$builddir" && mkdir "$builddir"
$bazel build :livegrep
tar -C "$builddir" -xf "$($bazel info bazel-bin)"/livegrep.tar
tar -czf "builds/$builddir.tgz" "$builddir"
rm -rf "$builddir"

# send the name of the built file, so that github actions can upload it
echo $builddir
