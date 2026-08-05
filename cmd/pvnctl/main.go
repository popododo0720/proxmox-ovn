package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/pvnstack/proxmox-ovn/internal/buildinfo"
	"github.com/pvnstack/proxmox-ovn/internal/central"
	"github.com/pvnstack/proxmox-ovn/internal/centraldb"
	"github.com/pvnstack/proxmox-ovn/internal/config"
	"github.com/pvnstack/proxmox-ovn/internal/diagnostic"
	"github.com/pvnstack/proxmox-ovn/internal/nodestate"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pvnctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "version" {
		fmt.Printf("pvnctl %s (%s, %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		return nil
	}
	switch args[0] {
	case "doctor":
		return doctor(args[1:])
	case "central":
		return centralCommand(args[1:])
	case "node":
		return nodeCommand(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func doctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	path := flags.String("config", config.DefaultPath, "PVN config path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	checks := diagnostic.Run(context.Background(), cfg, diagnostic.ExecRunner{})
	if err := writeJSON(os.Stdout, checks); err != nil {
		return err
	}
	if !diagnostic.Healthy(checks) {
		return errors.New("one or more checks failed")
	}
	return nil
}

func centralCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pvnctl central <plan|init-control|promote-control>")
	}
	switch args[0] {
	case "plan":
		return centralPlan(args[1:])
	case "init-control":
		return centralInit(args[1:])
	case "promote-control":
		return centralPromote(args[1:])
	default:
		return fmt.Errorf("unknown central command %q", args[0])
	}
}

func centralPlan(args []string) error {
	flags := flag.NewFlagSet("central plan", flag.ContinueOnError)
	nodeNames := flags.String("nodes", "", "comma-separated online eligible nodes")
	existingNames := flags.String("existing", "", "comma-separated existing voters")
	if err := flags.Parse(args); err != nil {
		return err
	}
	names := splitList(*nodeNames)
	nodes := make([]central.Node, 0, len(names))
	for index, name := range names {
		nodes = append(nodes, central.Node{Name: name, Online: true, Eligible: true, Order: index})
	}
	plan, err := central.Select(nodes, splitList(*existingNames))
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, plan)
}

func centralInit(args []string) error {
	flags := flag.NewFlagSet("central init-control", flag.ContinueOnError)
	configPath := flags.String("config", config.DefaultPath, "PVN config path")
	database := flags.String("database", centraldb.DefaultDatabase, "control database path")
	schema := flags.String("schema", centraldb.DefaultSchema, "control schema path")
	mode := flags.String("mode", "standalone", "standalone or raft")
	local := flags.String("local", "", "local Raft address, normally ssl:IP:6646")
	join := flags.String("join", "", "comma-separated existing Raft members")
	confirmation := flags.String("confirm", "", "exact PVN cluster ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := confirmedConfig(*configPath, *confirmation)
	if err != nil {
		return err
	}
	if err := centraldb.Init(context.Background(), centraldb.ExecRunner{}, centraldb.InitOptions{
		Database: *database,
		Schema:   *schema,
		Mode:     *mode,
		Local:    *local,
		Remotes:  splitList(*join),
	}); err != nil {
		return err
	}
	fmt.Printf("initialized %s PVN control database for cluster %s\n", *mode, cfg.Cluster.ID)
	return nil
}

func centralPromote(args []string) error {
	flags := flag.NewFlagSet("central promote-control", flag.ContinueOnError)
	configPath := flags.String("config", config.DefaultPath, "PVN config path")
	database := flags.String("database", centraldb.DefaultDatabase, "control database path")
	local := flags.String("local", "", "local Raft address, normally ssl:IP:6646")
	confirmation := flags.String("confirm", "", "exact PVN cluster ID")
	apply := flags.Bool("apply", false, "perform the promotion")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*apply {
		return errors.New("promotion is destructive to the service model; stop pvn-manager and pvn-control-db, then pass --apply")
	}
	if _, err := confirmedConfig(*configPath, *confirmation); err != nil {
		return err
	}
	backup, err := centraldb.Promote(context.Background(), centraldb.ExecRunner{}, centraldb.PromoteOptions{
		Database: *database,
		Local:    *local,
	})
	if err != nil {
		return err
	}
	fmt.Printf("promoted PVN control database to Raft; standalone backup: %s\n", backup)
	return nil
}

func confirmedConfig(path, confirmation string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, err
	}
	if confirmation == "" || confirmation != cfg.Cluster.ID {
		return config.Config{}, errors.New("--confirm must exactly match cluster.id")
	}
	return cfg, nil
}

func nodeCommand(args []string) error {
	if len(args) == 0 || args[0] != "can-remove" {
		return errors.New("usage: pvnctl node can-remove [--local] [--state path]")
	}
	flags := flag.NewFlagSet("node can-remove", flag.ContinueOnError)
	_ = flags.Bool("local", false, "check the local node")
	path := flags.String("state", nodestate.DefaultPath, "node state path")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	state, err := nodestate.Load(*path)
	if err != nil {
		return fmt.Errorf("cannot prove node is drained: %w", err)
	}
	if blockers := state.RemovalBlockers(); len(blockers) > 0 {
		return errors.New(strings.Join(blockers, "; "))
	}
	fmt.Printf("node %s is safe to remove\n", state.Node)
	return nil
}

func splitList(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func writeJSON(file *os.File, value any) error {
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
