#!/bin/bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
work_root=$(mktemp -d)
trap 'rm -rf "$work_root"' EXIT HUP INT TERM
fixture=$work_root/repository
install -d "$fixture/deploy/scripts" "$fixture/deploy/tests" \
    "$fixture/packaging/debian" "$fixture/tools"
install -m 0644 "$repo_root/Makefile" "$fixture/Makefile"
install -m 0644 "$repo_root/packaging/debian/changelog" \
    "$fixture/packaging/debian/changelog"
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
ci_workflow=$repo_root/.github/workflows/ci.yml

extract_workflow_job() {
    local workflow=$1
    local job=$2
    awk -v job="$job" '
        $0 ~ "^  " job ":[[:space:]]*(#.*)?$" {
            found = 1
            active = 1
            print
            next
        }
        active && (/^[^[:space:]]/ || /^  [^[:space:]][^:]*:/) { exit }
        active { print }
        END { exit found ? 0 : 1 }
    ' "$workflow"
}

job_needs() {
    local job_file=$1
    local dependency=$2
    awk -v dependency="$dependency" '
        function contains_dependency(value, fields, count, field_index) {
            sub(/[[:space:]]+#.*/, "", value)
            gsub(/[\[\],"]/, " ", value)
            gsub(/\047/, " ", value)
            count = split(value, fields, /[[:space:]]+/)
            for (field_index = 1; field_index <= count; field_index++) {
                if (fields[field_index] == dependency) return 1
            }
            return 0
        }
        /^    needs:/ {
            in_needs = 1
            value = $0
            sub(/^    needs:[[:space:]]*/, "", value)
            if (contains_dependency(value)) found = 1
            next
        }
        in_needs && /^      -[[:space:]]*/ {
            value = $0
            sub(/^      -[[:space:]]*/, "", value)
            if (contains_dependency(value)) found = 1
            next
        }
        in_needs && /^    [^[:space:]]/ { in_needs = 0 }
        END { exit found ? 0 : 1 }
    ' "$job_file"
}

ci_push_block=$work_root/ci-push.yml
if ! awk '
    /^  push:[[:space:]]*(#.*)?$/ {
        found = 1
        active = 1
        print
        next
    }
    active && (/^[^[:space:]]/ || /^  [^[:space:]][^:]*:/) { exit }
    active { print }
    END { exit found ? 0 : 1 }
' "$ci_workflow" > "$ci_push_block"; then
    printf 'CI workflow is missing a push trigger\n' >&2
    exit 1
fi
if ! awk '
    /^    branches(-ignore)?:/ {
        found = 1
        in_filter = 1
        value = $0
        sub(/^    branches(-ignore)?:[[:space:]]*/, "", value)
        sub(/[[:space:]]+#.*/, "", value)
        if (value != "" && value != "[]") populated = 1
        next
    }
    in_filter && /^      -[[:space:]]*[^[:space:]#]/ { populated = 1; next }
    in_filter && /^    [^[:space:]]/ { in_filter = 0 }
    END { exit found && populated ? 0 : 1 }
' "$ci_push_block"; then
    printf 'CI push trigger must use a nonempty branch filter so tag pushes are ignored\n' >&2
    exit 1
fi
if grep -Eq '^    tags(-ignore)?:' "$ci_push_block"; then
    printf 'CI push trigger must not configure tag execution\n' >&2
    exit 1
fi

package_check_job=$work_root/release-package-check-job.yml
if ! extract_workflow_job "$release_workflow" package-check > "$package_check_job"; then
    printf 'release workflow is missing the package-check matrix job\n' >&2
    exit 1
fi
mapfile -t package_check_suites < <(awk '
    /^        suite:[[:space:]]*(#.*)?$/ { in_suite = 1; found = 1; next }
    in_suite && /^          -[[:space:]]*/ {
        value = $0
        sub(/^          -[[:space:]]*/, "", value)
        sub(/[[:space:]]+#.*/, "", value)
        gsub(/^[\047"]|[\047"]$/, "", value)
        if (value != "") print value
        next
    }
    in_suite && /^        [^[:space:]]/ { in_suite = 0 }
    END { if (!found) exit 1 }
' "$package_check_job")
if (( ${#package_check_suites[@]} != 4 )); then
    printf 'release package-check matrix must contain exactly four suites\n' >&2
    exit 1
fi
declare -A expected_package_check_suites=(
    [fast]=1
    [topology]=1
    [control-plane]=1
    [backup]=1
)
declare -A found_package_check_suites=()
for suite in "${package_check_suites[@]}"; do
    if [[ ! ${expected_package_check_suites[$suite]+expected} || \
        ${found_package_check_suites[$suite]+duplicate} ]]; then
        printf 'release package-check matrix has an unexpected or duplicate suite: %s\n' \
            "$suite" >&2
        exit 1
    fi
    found_package_check_suites[$suite]=1
done
if ! grep -Fq '${{ matrix.suite }}' "$package_check_job" || \
    ! grep -Eq '(^|[[:space:]])make[[:space:]]+package-check-' "$package_check_job"; then
    printf 'release package-check job does not execute the selected matrix suite\n' >&2
    exit 1
fi

release_job=$work_root/release-publish-build-job.yml
if ! extract_workflow_job "$release_workflow" release > "$release_job"; then
    printf 'release workflow is missing the publish/build job\n' >&2
    exit 1
fi
if ! job_needs "$release_job" package-check; then
    printf 'release publish/build job must depend on the package-check matrix\n' >&2
    exit 1
fi
release_artifact_calls=$(grep -Ec \
    '(^|[[:space:]])make[[:space:]]+release-artifact([[:space:]\\]|$)' \
    "$release_job")
if (( release_artifact_calls != 1 )); then
    printf 'release publish/build job must invoke release-artifact exactly once\n' >&2
    exit 1
fi
if grep -Eq '(^|[[:space:]])make[[:space:]]+release([[:space:]\\]|$)' \
    "$release_workflow"; then
    printf 'release workflow must not rerun the full make release target\n' >&2
    exit 1
fi

ci_package_check_job=$work_root/ci-package-check-job.yml
if ! extract_workflow_job "$ci_workflow" package-check > "$ci_package_check_job"; then
    printf 'CI workflow is missing the package-check matrix job\n' >&2
    exit 1
fi
ci_package_check_suites=$(awk '
    /^        suite:[[:space:]]*(#.*)?$/ { in_suite = 1; next }
    in_suite && /^          -[[:space:]]*/ {
        value = $0
        sub(/^          -[[:space:]]*/, "", value)
        sub(/[[:space:]]+#.*/, "", value)
        gsub(/^[\047"]|[\047"]$/, "", value)
        if (value != "") print value
        next
    }
    in_suite && /^        [^[:space:]]/ { in_suite = 0 }
' "$ci_package_check_job" | LC_ALL=C sort)
expected_package_check_suites=$(printf '%s\n' \
    backup control-plane fast topology | LC_ALL=C sort)
if [[ $ci_package_check_suites != "$expected_package_check_suites" ]]; then
    printf 'CI package-check matrix must contain the exact four suites\n' >&2
    exit 1
fi
grep -Fq "if: github.event_name == 'push' && github.ref == 'refs/heads/main'" \
    "$ci_package_check_job" || {
    printf 'CI package-check matrix must run only for main branch pushes\n' >&2
    exit 1
}
if ! grep -Fq 'make package-check-${{ matrix.suite }}' \
    "$ci_package_check_job"; then
    printf 'CI package-check matrix does not execute the selected suite\n' >&2
    exit 1
fi

ci_package_job=$work_root/ci-package-artifact-job.yml
if ! extract_workflow_job "$ci_workflow" package > "$ci_package_job"; then
    printf 'CI workflow is missing the package artifact job\n' >&2
    exit 1
fi
if ! job_needs "$ci_package_job" go || \
    ! job_needs "$ci_package_job" package-check; then
    printf 'CI package artifact job must depend on go and every package check\n' >&2
    exit 1
fi
ci_deb_artifact_calls=$(grep -Ec \
    '(^|[[:space:]])make[[:space:]]+deb-artifact([[:space:]\\]|$)' \
    "$ci_package_job")
if (( ci_deb_artifact_calls != 1 )); then
    printf 'CI package artifact job must invoke deb-artifact exactly once\n' >&2
    exit 1
fi
if grep -Eq '(^|[[:space:]])make[[:space:]]+deb([[:space:]\\]|$)' \
    "$ci_workflow"; then
    printf 'CI workflow must not rerun the aggregate make deb target\n' >&2
    exit 1
fi

grep -Fq 'libhttp-message-perl' "$release_workflow" || {
    printf 'release workflow is missing HTTP::Response prerequisites\n' >&2
    exit 1
}
awk '
    /^  package:/ { in_package = 1 }
    in_package && /libhttp-message-perl/ { found = 1 }
    END { exit found ? 0 : 1 }
' "$ci_workflow" || {
    printf 'package CI is missing HTTP::Response prerequisites\n' >&2
    exit 1
}
if grep -Fq 'releases/tags/$GITHUB_REF_NAME' "$release_workflow"; then
    printf 'release workflow uses an API endpoint that cannot resolve drafts\n' >&2
    exit 1
fi
grep -Fq 'gh release view "$GITHUB_REF_NAME"' "$release_workflow" || {
    printf 'release workflow does not resolve releases through draft-aware gh lookup\n' >&2
    exit 1
}
if grep -Fq \
    '.assets[] | select(.uploader.login != "github-actions[bot]")' \
    "$release_workflow"; then
    printf 'release workflow trusts uploader data omitted by gh release view\n' >&2
    exit 1
fi
rest_uploader_checks=$(grep -Fc \
    '"repos/$GITHUB_REPOSITORY/releases/assets/$asset_id"' \
    "$release_workflow")
if (( rest_uploader_checks != 2 )); then
    printf 'release workflow does not verify claim and publish assets through REST\n' >&2
    exit 1
fi

printf 'release-check tests passed\n'
