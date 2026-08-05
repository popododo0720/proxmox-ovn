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

mkdir "$WORK/bin" "$WORK/state" "$WORK/counts"
LOG=$WORK/calls.log
: > "$LOG"
cat > "$WORK/bin/ovs-appctl" <<'EOF'
#!/usr/bin/python3
import os
import pathlib
import sys

arguments = sys.argv[1:]
with open(os.environ["PVN_LISTENER_TEST_LOG"], "a", encoding="utf-8") as stream:
    stream.write(" ".join(arguments) + "\n")

target = next((item.split("=", 1)[1] for item in arguments if item.startswith("--target=")), None)
if target == "/run/ovn/ovnnb_db.ctl":
    database = "nb"
elif target == "/run/ovn/ovnsb_db.ctl":
    database = "sb"
else:
    print("unexpected control socket", file=sys.stderr)
    raise SystemExit(9)

commands = [item for item in arguments if item.startswith("ovsdb-server/")]
if len(commands) != 1:
    print("unexpected command", file=sys.stderr)
    raise SystemExit(9)
command = commands[0]
state = pathlib.Path(os.environ["PVN_LISTENER_TEST_STATE"]) / database
counts = pathlib.Path(os.environ["PVN_LISTENER_TEST_COUNTS"])

def invocation_count(name):
    path = counts / name
    value = int(path.read_text()) if path.exists() else 0
    value += 1
    path.write_text(str(value))
    return value

if command == "ovsdb-server/list-remotes":
    if invocation_count("list") <= int(os.environ.get("PVN_LISTENER_TEST_LIST_FAILS", "0")):
        print("database control socket is not ready", file=sys.stderr)
        raise SystemExit(1)
    sys.stdout.write(state.read_text())
elif command == "ovsdb-server/remove-remote":
    if invocation_count("remove") <= int(os.environ.get("PVN_LISTENER_TEST_REMOVE_FAILS", "0")):
        print("remove remote temporarily failed", file=sys.stderr)
        raise SystemExit(1)
    remote = arguments[-1]
    lines = state.read_text().splitlines()
    if lines.count(remote) != 1:
        print("remove target is not exact", file=sys.stderr)
        raise SystemExit(1)
    lines.remove(remote)
    state.write_text("".join(item + "\n" for item in lines))
elif command == "ovsdb-server/add-remote":
    if invocation_count("add") <= int(os.environ.get("PVN_LISTENER_TEST_ADD_FAILS", "0")):
        print("add remote temporarily failed", file=sys.stderr)
        raise SystemExit(1)
    remote = arguments[-1]
    lines = state.read_text().splitlines()
    if remote in lines:
        print("duplicate add", file=sys.stderr)
        raise SystemExit(1)
    lines.append(remote)
    state.write_text("".join(item + "\n" for item in lines))
else:
    print("unexpected ovs-appctl command", file=sys.stderr)
    raise SystemExit(9)
EOF
chmod 0755 "$WORK/bin/ovs-appctl"

TEST_SCRIPT=$WORK/pvn-ovn-db-listeners
sed "s#/usr/bin/ovs-appctl#$WORK/bin/ovs-appctl#g" "$SOURCE" > "$TEST_SCRIPT"
chmod 0755 "$TEST_SCRIPT"

export PVN_LISTENER_TEST_LOG=$LOG
export PVN_LISTENER_TEST_STATE=$WORK/state
export PVN_LISTENER_TEST_COUNTS=$WORK/counts

reset_test() {
    : > "$LOG"
    rm -f "$WORK/counts/list" "$WORK/counts/add" "$WORK/counts/remove"
}

write_migration_state() {
    printf '%s\n%s\n' \
        'db:OVN_Northbound,NB_Global,connections' \
        'punix:/var/run/ovn/ovnnb_db.sock' > "$WORK/state/nb"
    printf '%s\n%s\n' \
        'db:OVN_Southbound,SB_Global,connections' \
        'punix:/var/run/ovn/ovnsb_db.sock' > "$WORK/state/sb"
}

write_final_state() {
    printf '%s\n%s\n' \
        'punix:/var/run/ovn/ovnnb_db.sock' \
        'pssl:6641:192.0.2.10' > "$WORK/state/nb"
    printf '%s\n%s\n' \
        'punix:/var/run/ovn/ovnsb_db.sock' \
        'pssl:6642:192.0.2.10' > "$WORK/state/sb"
}

assert_final_state() {
    [ "$(cat "$WORK/state/nb")" = "$(printf '%s\n%s' \
        'punix:/var/run/ovn/ovnnb_db.sock' 'pssl:6641:192.0.2.10')" ]
    [ "$(cat "$WORK/state/sb")" = "$(printf '%s\n%s' \
        'punix:/var/run/ovn/ovnsb_db.sock' 'pssl:6642:192.0.2.10')" ]
}

# The no-argument oneshot handles both processes without consulting Raft role.
# Readiness failures and a transient add failure consume only the bounded retry
# budget; the replicated database remote is migrated to one local listener.
write_migration_state
reset_test
PVN_LISTENER_TEST_LIST_FAILS=2 PVN_LISTENER_TEST_ADD_FAILS=1 \
    PVN_LISTENER_TEST_REMOVE_FAILS=1 \
    PVN_OVN_LISTENER_RETRY_DELAY=0 PVN_OVN_LISTEN=192.0.2.10 "$TEST_SCRIPT"
assert_final_state
grep -Fq -- '--target=/run/ovn/ovnnb_db.ctl ovsdb-server/remove-remote db:OVN_Northbound,NB_Global,connections' "$LOG"
grep -Fq -- '--target=/run/ovn/ovnnb_db.ctl ovsdb-server/add-remote pssl:6641:192.0.2.10' "$LOG"
grep -Fq -- '--target=/run/ovn/ovnsb_db.ctl ovsdb-server/remove-remote db:OVN_Southbound,SB_Global,connections' "$LOG"
grep -Fq -- '--target=/run/ovn/ovnsb_db.ctl ovsdb-server/add-remote pssl:6642:192.0.2.10' "$LOG"
nb_add_line=$(grep -Fn -- 'ovsdb-server/add-remote pssl:6641:192.0.2.10' "$LOG" | head -1 | cut -d: -f1)
nb_remove_line=$(grep -Fn -- 'ovsdb-server/remove-remote db:OVN_Northbound,NB_Global,connections' "$LOG" | head -1 | cut -d: -f1)
[ "$nb_add_line" -lt "$nb_remove_line" ] || {
    echo "replicated NB remote was removed before the local listener was added" >&2
    exit 1
}
if grep -Eq 'cluster/status|leader|set-connection' "$LOG"; then
    echo "listener configuration depended on replicated state or Raft role" >&2
    exit 1
fi

# With one attempt, an add failure leaves the exact live migration state
# untouched and never reaches remove-remote.
write_migration_state
cp "$WORK/state/nb" "$WORK/add-failure.before"
reset_test
if PVN_LISTENER_TEST_ADD_FAILS=99 PVN_OVN_LISTENER_ATTEMPTS=1 \
    PVN_OVN_LISTENER_RETRY_DELAY=0 PVN_OVN_LISTEN=192.0.2.10 \
    "$TEST_SCRIPT" nb > "$WORK/add-failure.log" 2>&1; then
    echo "listener test accepted a failed local endpoint add" >&2
    exit 1
fi
cmp "$WORK/add-failure.before" "$WORK/state/nb"
grep -Fq 'add remote temporarily failed' "$WORK/add-failure.log"
if grep -Fq 'remove-remote' "$LOG"; then
    echo "listener source was removed after the local endpoint add failed" >&2
    exit 1
fi

# An already-correct follower/leader process is idempotent: one read per DB and
# no add/remove mutation.
write_final_state
reset_test
PVN_OVN_LISTENER_RETRY_DELAY=0 PVN_OVN_LISTEN=192.0.2.10 "$TEST_SCRIPT"
[ "$(wc -l < "$LOG")" -eq 2 ]
grep -Fq -- '--target=/run/ovn/ovnnb_db.ctl ovsdb-server/list-remotes' "$LOG"
grep -Fq -- '--target=/run/ovn/ovnsb_db.ctl ovsdb-server/list-remotes' "$LOG"
if grep -Eq 'add-remote|remove-remote' "$LOG"; then
    echo "idempotent listener verification mutated a process remote" >&2
    exit 1
fi

# A per-process invocation, as used by ExecStartPost, touches only that DB.
write_migration_state
reset_test
PVN_OVN_LISTENER_RETRY_DELAY=0 PVN_OVN_LISTEN=192.0.2.10 "$TEST_SCRIPT" nb
if grep -Fq -- '--target=/run/ovn/ovnsb_db.ctl' "$LOG"; then
    echo "NB-only listener invocation touched SB" >&2
    exit 1
fi

# Any foreign/wrong process remote is rejected before remove/add, and the
# original state remains byte-exact.
printf '%s\n%s\n%s\n' \
    'db:OVN_Northbound,NB_Global,connections' \
    'punix:/var/run/ovn/ovnnb_db.sock' \
    'pssl:6641:192.0.2.99' > "$WORK/state/nb"
cp "$WORK/state/nb" "$WORK/foreign.before"
reset_test
if PVN_OVN_LISTENER_RETRY_DELAY=0 PVN_OVN_LISTEN=192.0.2.10 \
    "$TEST_SCRIPT" nb > "$WORK/foreign.log" 2>&1; then
    echo "listener test accepted an unexpected process remote" >&2
    exit 1
fi
cmp "$WORK/foreign.before" "$WORK/state/nb"
grep -Fq 'unexpected process remote' "$WORK/foreign.log"
[ "$(wc -l < "$LOG")" -eq 1 ]
if grep -Eq 'add-remote|remove-remote' "$LOG"; then
    echo "unexpected process remote was mutated" >&2
    exit 1
fi

# A missing canonical Unix remote is also drift, not an invitation to rewrite
# arbitrary process state.
printf '%s\n' 'db:OVN_Northbound,NB_Global,connections' > "$WORK/state/nb"
reset_test
if PVN_OVN_LISTENER_RETRY_DELAY=0 PVN_OVN_LISTEN=192.0.2.10 \
    "$TEST_SCRIPT" nb > "$WORK/missing.log" 2>&1; then
    echo "listener test accepted a missing canonical Unix remote" >&2
    exit 1
fi
grep -Fq 'incomplete or duplicated' "$WORK/missing.log"
[ "$(wc -l < "$LOG")" -eq 1 ]

# Socket readiness is retried exactly to the configured bound.
write_migration_state
reset_test
if PVN_LISTENER_TEST_LIST_FAILS=99 PVN_OVN_LISTENER_ATTEMPTS=3 \
    PVN_OVN_LISTENER_RETRY_DELAY=0 PVN_OVN_LISTEN=192.0.2.10 \
    "$TEST_SCRIPT" nb > "$WORK/failure.log" 2>&1; then
    echo "listener test did not fail after its retry budget" >&2
    exit 1
fi
[ "$(wc -l < "$LOG")" -eq 3 ]
grep -Fq 'NB database did not become ready after 3 attempts' "$WORK/failure.log"
grep -Fq 'database control socket is not ready' "$WORK/failure.log"

for invalid in '' 0.0.0.0 192.0.2 192.0.2.999 example.invalid; do
    if PVN_OVN_LISTENER_RETRY_DELAY=0 PVN_OVN_LISTEN=$invalid \
        "$TEST_SCRIPT" >/dev/null 2>&1; then
        echo "listener test accepted unsafe address: ${invalid:-empty}" >&2
        exit 1
    fi
done
if PVN_OVN_LISTEN=192.0.2.10 "$TEST_SCRIPT" wrong >/dev/null 2>&1; then
    echo "listener test accepted an invalid database selector" >&2
    exit 1
fi
if PVN_OVN_LISTENER_ATTEMPTS=21 PVN_OVN_LISTEN=192.0.2.10 \
    "$TEST_SCRIPT" nb >/dev/null 2>&1; then
    echo "listener test accepted a retry budget beyond its systemd timeout" >&2
    exit 1
fi
if PVN_OVN_LISTENER_RETRY_DELAY=2 PVN_OVN_LISTEN=192.0.2.10 \
    "$TEST_SCRIPT" nb >/dev/null 2>&1; then
    echo "listener test accepted a retry delay beyond its systemd timeout" >&2
    exit 1
fi

NB_DROPIN=$REPO/deploy/systemd/ovn-ovsdb-server-nb.service.d/90-pvn.conf
SB_DROPIN=$REPO/deploy/systemd/ovn-ovsdb-server-sb.service.d/90-pvn.conf
grep -Fxq 'EnvironmentFile=/etc/pvn/central/ovn-listeners.env' "$NB_DROPIN"
grep -Fxq 'EnvironmentFile=/etc/pvn/central/ovn-listeners.env' "$SB_DROPIN"
grep -Fxq 'ExecStartPost=/usr/lib/pvn/pvn-ovn-db-listeners nb' "$NB_DROPIN"
grep -Fxq 'ExecStartPost=/usr/lib/pvn/pvn-ovn-db-listeners sb' "$SB_DROPIN"
grep -Fxq 'TimeoutStartSec=120' "$NB_DROPIN"
grep -Fxq 'TimeoutStartSec=120' "$SB_DROPIN"
grep -Fxq 'ExecStart=/usr/lib/pvn/pvn-ovn-db-listeners' \
    "$REPO/deploy/systemd/pvn-ovn-db-listeners.service"
grep -Fxq 'TimeoutStartSec=240' \
    "$REPO/deploy/systemd/pvn-ovn-db-listeners.service"
if grep -Eq 'ovn-(nb|sb)ctl|set-connection' "$SOURCE"; then
    echo "listener helper still writes replicated Connection rows" >&2
    exit 1
fi

echo "pvn-ovn-db-listeners tests passed"
