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
			return nil, os.WriteFile(args[1], []byte("created"), 0o600)
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
	want := []string{"join-cluster", joined, SchemaName, "ssl:192.0.2.2:6646", "ssl:192.0.2.1:6646"}
	if got := runner.calls[1].args; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
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
	if backup != database+".standalone.20260805T010203Z" {
		t.Fatalf("unexpected backup %q", backup)
	}
	data, err := os.ReadFile(backup)
	if err != nil || string(data) != "standalone" {
		t.Fatalf("backup invalid: data=%q err=%v", data, err)
	}
}
