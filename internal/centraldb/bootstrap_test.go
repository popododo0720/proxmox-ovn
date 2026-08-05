package centraldb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	calls  []call
	active bool
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...)})
	if name == "systemctl" {
		if f.active {
			return nil, nil
		}
		return nil, errors.New("inactive")
	}
	if name == "ovsdb-tool" && len(args) > 0 {
		switch args[0] {
		case "create", "create-cluster", "join-cluster":
			if err := os.WriteFile(args[1], []byte("created"), 0o600); err != nil {
				return nil, err
			}
			return nil, os.WriteFile(databaseLockPath(args[1]), nil, 0o600)
		}
	}
	return nil, nil
}

func TestInitStandaloneAndRaftJoin(t *testing.T) {
	dir := t.TempDir()
	schema := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schema, []byte(`{"name":"PVN_Control"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	standalone := filepath.Join(dir, "standalone.db")
	if err := Init(context.Background(), runner, InitOptions{Database: standalone, Schema: schema}); err != nil {
		t.Fatal(err)
	}
	assertLockRemoved(t, standalone)
	bootstrap := filepath.Join(dir, "bootstrap.db")
	if err := Init(context.Background(), runner, InitOptions{
		Database: bootstrap,
		Schema:   schema,
		Mode:     "raft",
		Local:    "ssl:192.0.2.1:6646",
	}); err != nil {
		t.Fatal(err)
	}
	assertLockRemoved(t, bootstrap)
	joined := filepath.Join(dir, "joined.db")
	if err := Init(context.Background(), runner, InitOptions{
		Database: joined,
		Schema:   schema,
		Mode:     "raft",
		Local:    "ssl:192.0.2.2:6646",
		Remotes:  []string{"ssl:192.0.2.1:6646"},
	}); err != nil {
		t.Fatal(err)
	}
	assertLockRemoved(t, joined)
	want := []string{"join-cluster", joined, SchemaName, "ssl:192.0.2.2:6646", "ssl:192.0.2.1:6646"}
	if got := runner.calls[2].args; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func assertLockRemoved(t *testing.T, database string) {
	t.Helper()
	if _, err := os.Lstat(databaseLockPath(database)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database lock was not removed: %v", err)
	}
}

func TestClusterAddressRequiresMutualTLSIPv4Port(t *testing.T) {
	for _, address := range []string{
		"tcp:192.0.2.1:6646",
		"ssl:db.example.test:6646",
		"ssl:[2001:db8::1]:6646",
		"ssl:192.0.2.1:16646",
	} {
		if err := validateClusterAddress(address); err == nil {
			t.Fatalf("cluster address %q unexpectedly accepted", address)
		}
	}
	if err := validateClusterAddress("ssl:192.0.2.1:6646"); err != nil {
		t.Fatalf("secure v1 cluster address rejected: %v", err)
	}
}

func TestPromoteRequiresStoppedDatabaseAndKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "control.db")
	if err := os.WriteFile(database, []byte("standalone"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{active: true}
	if _, err := Promote(context.Background(), runner, PromoteOptions{Database: database, Local: "ssl:192.0.2.1:6646"}); err == nil {
		t.Fatal("active service must block promotion")
	}
	runner.active = false
	backup, err := Promote(context.Background(), runner, PromoteOptions{
		Database: database,
		Local:    "ssl:192.0.2.1:6646",
		Now:      func() time.Time { return time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	createClusterCall := runner.calls[len(runner.calls)-1]
	assertLockRemoved(t, createClusterCall.args[1])
	if backup != database+".standalone.20260805T010203Z" {
		t.Fatalf("unexpected backup %q", backup)
	}
	data, err := os.ReadFile(backup)
	if err != nil || string(data) != "standalone" {
		t.Fatalf("backup invalid: data=%q err=%v", data, err)
	}
}
