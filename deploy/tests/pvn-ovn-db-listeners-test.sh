#!/bin/sh
set -eu

TEST_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
REPO=$(CDPATH= cd -P "$TEST_DIR/../.." && pwd)
SOURCE=$REPO/deploy/scripts/pvn-ovn-db-listeners
WORK=$(mktemp -d)

cleanup() {
    rm -rf "$WORK"
}
trap cleanup 0 HUP INT TERM

mkdir "$WORK/bin"
LOG=$WORK/calls.log
: > "$LOG"
cat > "$WORK/bin/ovnctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$PVN_LISTENER_TEST_LOG"
EOF
chmod 0755 "$WORK/bin/ovnctl"

TEST_SCRIPT=$WORK/pvn-ovn-db-listeners
sed \
    -e "s#/usr/bin/ovn-nbctl#$WORK/bin/ovnctl#g" \
    -e "s#/usr/bin/ovn-sbctl#$WORK/bin/ovnctl#g" \
    "$SOURCE" > "$TEST_SCRIPT"
chmod 0755 "$TEST_SCRIPT"

export PVN_LISTENER_TEST_LOG=$LOG
PVN_OVN_LISTEN=192.0.2.10 "$TEST_SCRIPT"
[ "$(wc -l < "$LOG")" -eq 2 ]
grep -Fq 'set-connection pssl:6641:192.0.2.10' "$LOG"
grep -Fq 'set-connection pssl:6642:192.0.2.10' "$LOG"

for invalid in '' 0.0.0.0 192.0.2 192.0.2.999 example.invalid; do
    if PVN_OVN_LISTEN=$invalid "$TEST_SCRIPT" >/dev/null 2>&1; then
        echo "listener test accepted unsafe address: ${invalid:-empty}" >&2
        exit 1
    fi
done

echo "pvn-ovn-db-listeners tests passed"
