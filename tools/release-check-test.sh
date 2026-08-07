#!/bin/bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
work_root=$(mktemp -d)
trap 'rm -rf "$work_root"' EXIT HUP INT TERM
fixture=$work_root/repository
install -d "$fixture/deploy/scripts" "$fixture/deploy/tests" \
    "$fixture/packaging/debian" "$fixture/tools" "$fixture/web"
install -m 0644 "$repo_root/Makefile" "$fixture/Makefile"
install -m 0644 "$repo_root/packaging/debian/changelog" \
    "$fixture/packaging/debian/changelog"
install -m 0644 "$repo_root/web/package.json" "$repo_root/web/package-lock.json" \
    "$fixture/web/"
install -m 0755 "$repo_root/deploy/scripts/pvn-install.sh" \
    "$repo_root/deploy/scripts/pvn-update.sh" "$fixture/deploy/scripts/"
install -m 0755 "$repo_root/deploy/tests/pvn-install-test.sh" \
    "$repo_root/deploy/tests/pvn-update-test.sh" "$fixture/deploy/tests/"
install -m 0755 "$repo_root/tools/release-check" "$fixture/tools/release-check"
git -C "$fixture" init -q
git -C "$fixture" add -A
git -C "$fixture" -c user.name=PVN -c user.email=pvn@example.invalid \
    commit -qm fixture

version=$(sed -n 's/^DEB_VERSION ?= //p' "$fixture/Makefile")
if [[ -z $version || $version == *$'\n'* ]]; then
    printf 'could not resolve one release version from Makefile\n' >&2
    exit 1
fi
git -C "$fixture" tag -f "v$version" HEAD >/dev/null

expected_commit=$(git -C "$fixture" rev-parse --short=12 HEAD)
expected_epoch=$(git -C "$fixture" show -s --format=%ct HEAD)
expected_date=$(date -u -d "@$expected_epoch" +%Y-%m-%dT%H:%M:%SZ)

run_check() {
    local requested_version=${1:-$version}
    (
        cd "$fixture"
        CI=${TEST_CI:-true} \
        GITHUB_ACTIONS=${TEST_GITHUB_ACTIONS:-true} \
        COMMIT=${TEST_COMMIT:-$expected_commit} \
        SOURCE_DATE_EPOCH=${TEST_SOURCE_DATE_EPOCH:-$expected_epoch} \
        BUILD_DATE=${TEST_BUILD_DATE:-$expected_date} \
            tools/release-check "$requested_version"
    )
}

expect_failure() {
    local description=$1
    shift
    if "$@" >/dev/null 2>&1; then
        printf 'release check unexpectedly accepted %s\n' "$description" >&2
        exit 1
    fi
}

run_check >/dev/null
expect_failure 'a mismatched version' run_check 9.9.9

git -C "$fixture" tag -d "v$version" >/dev/null
expect_failure 'a missing release tag' run_check
git -C "$fixture" tag "v$version" HEAD

touch "$fixture/release-check-dirty"
expect_failure 'a dirty worktree' run_check
rm -f "$fixture/release-check-dirty"

TEST_COMMIT=wrong expect_failure 'wrong commit metadata' run_check
TEST_SOURCE_DATE_EPOCH=1 expect_failure 'wrong source epoch' run_check
TEST_BUILD_DATE=1970-01-01T00:00:01Z \
    expect_failure 'wrong build date' run_check
TEST_GITHUB_ACTIONS=false expect_failure 'a non-Actions publisher' run_check

release_workflow=$repo_root/.github/workflows/release.yml
if grep -Fq 'releases/tags/$GITHUB_REF_NAME' "$release_workflow"; then
    printf 'release workflow uses an API endpoint that cannot resolve drafts\n' >&2
    exit 1
fi
grep -Fq 'gh release view "$GITHUB_REF_NAME"' "$release_workflow" || {
    printf 'release workflow does not resolve releases through draft-aware gh lookup\n' >&2
    exit 1
}

printf 'release-check tests passed\n'
