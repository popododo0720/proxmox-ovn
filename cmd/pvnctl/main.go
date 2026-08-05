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
	if len(args) == 0 || args[0] != "plan" {
		return errors.New("usage: pvnctl central plan --nodes pve-a,pve-b [--existing pve-a]")
	}
	flags := flag.NewFlagSet("central plan", flag.ContinueOnError)
	nodeNames := flags.String("nodes", "", "comma-separated online eligible nodes")
	existingNames := flags.String("existing", "", "comma-separated existing voters")
	if err := flags.Parse(args[1:]); err != nil {
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
