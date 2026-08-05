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
count=0
[ ! -f "$PVN_LISTENER_TEST_COUNT" ] || count=$(cat "$PVN_LISTENER_TEST_COUNT")
count=$((count + 1))
printf '%s\n' "$count" > "$PVN_LISTENER_TEST_COUNT"
if [ "$count" -le "${PVN_LISTENER_TEST_FAILS:-0}" ]; then
    echo "database socket is not ready" >&2
    exit 1
fi
EOF
chmod 0755 "$WORK/bin/ovnctl"

TEST_SCRIPT=$WORK/pvn-ovn-db-listeners
sed \
    -e "s#/usr/bin/ovn-nbctl#$WORK/bin/ovnctl#g" \
    -e "s#/usr/bin/ovn-sbctl#$WORK/bin/ovnctl#g" \
    "$SOURCE" > "$TEST_SCRIPT"
chmod 0755 "$TEST_SCRIPT"

export PVN_LISTENER_TEST_LOG=$LOG
export PVN_LISTENER_TEST_COUNT=$WORK/count
PVN_LISTENER_TEST_FAILS=2 PVN_OVN_LISTENER_RETRY_DELAY=0 \
    PVN_OVN_LISTEN=192.0.2.10 "$TEST_SCRIPT"
[ "$(wc -l < "$LOG")" -eq 4 ]
grep -Fq 'set-connection pssl:6641:192.0.2.10' "$LOG"
grep -Fq 'set-connection pssl:6642:192.0.2.10' "$LOG"

: > "$LOG"
rm -f "$PVN_LISTENER_TEST_COUNT"
if PVN_LISTENER_TEST_FAILS=99 PVN_OVN_LISTENER_ATTEMPTS=3 \
    PVN_OVN_LISTENER_RETRY_DELAY=0 PVN_OVN_LISTEN=192.0.2.10 \
    "$TEST_SCRIPT" >"$WORK/failure.log" 2>&1; then
    echo "listener test did not fail after its retry budget" >&2
    exit 1
fi
[ "$(wc -l < "$LOG")" -eq 3 ]
grep -Fq 'NB database did not become ready after 3 attempts' "$WORK/failure.log"

for invalid in '' 0.0.0.0 192.0.2 192.0.2.999 example.invalid; do
    if PVN_OVN_LISTENER_RETRY_DELAY=0 PVN_OVN_LISTEN=$invalid "$TEST_SCRIPT" >/dev/null 2>&1; then
        echo "listener test accepted unsafe address: ${invalid:-empty}" >&2
        exit 1
    fi
done

echo "pvn-ovn-db-listeners tests passed"
