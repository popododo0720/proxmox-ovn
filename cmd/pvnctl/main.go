package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/buildinfo"
	"github.com/popododo0720/proxmox-ovn/internal/central"
	"github.com/popododo0720/proxmox-ovn/internal/centraldb"
	"github.com/popododo0720/proxmox-ovn/internal/config"
	"github.com/popododo0720/proxmox-ovn/internal/diagnostic"
	"github.com/popododo0720/proxmox-ovn/internal/hostconfig"
	"github.com/popododo0720/proxmox-ovn/internal/nodestate"
	"github.com/popododo0720/proxmox-ovn/internal/pki"
	"github.com/popododo0720/proxmox-ovn/internal/raftstatus"
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
	case "pki":
		return pkiCommand(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func pkiCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pvnctl pki <init-ca|issue-node>")
	}
	switch args[0] {
	case "init-ca":
		return pkiInitCA(args[1:])
	case "issue-node":
		return pkiIssueNode(args[1:])
	default:
		return fmt.Errorf("unknown pki command %q", args[0])
	}
}

func pkiInitCA(args []string) error {
	flags := flag.NewFlagSet("pki init-ca", flag.ContinueOnError)
	configPath := flags.String("config", config.DefaultPath, "PVN config path")
	directory := flags.String("directory", "/etc/pvn/pki/ca", "CA output directory")
	confirmation := flags.String("confirm", "", "exact PVN cluster ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := confirmedConfig(*configPath, *confirmation)
	if err != nil {
		return err
	}
	files, err := pki.CreateCA(pki.CAOptions{Directory: *directory, ClusterID: cfg.Cluster.ID})
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, files)
}

func pkiIssueNode(args []string) error {
	flags := flag.NewFlagSet("pki issue-node", flag.ContinueOnError)
	configPath := flags.String("config", config.DefaultPath, "PVN config path")
	caCertificate := flags.String("ca-cert", "/etc/pvn/pki/ca/ca.pem", "CA certificate path")
	caKey := flags.String("ca-key", "/etc/pvn/pki/ca/ca-key.pem", "CA private key path")
	directory := flags.String("directory", "/etc/pvn/pki/nodes", "node certificate output directory")
	name := flags.String("name", "", "PVE node/chassis name")
	dnsNames := flags.String("dns", "", "comma-separated DNS subject names")
	ipAddresses := flags.String("ips", "", "comma-separated IP subject addresses")
	confirmation := flags.String("confirm", "", "exact PVN cluster ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if _, err := confirmedConfig(*configPath, *confirmation); err != nil {
		return err
	}
	var addresses []net.IP
	for _, raw := range splitList(*ipAddresses) {
		address := net.ParseIP(raw)
		if address == nil {
			return fmt.Errorf("invalid certificate IP address %q", raw)
		}
		addresses = append(addresses, address)
	}
	files, err := pki.IssueNode(pki.IssueOptions{
		CACertificate: *caCertificate,
		CAKey:         *caKey,
		Directory:     *directory,
		Name:          *name,
		DNSNames:      splitList(*dnsNames),
		IPAddresses:   addresses,
	})
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, files)
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
		return errors.New("usage: pvnctl central <status|plan|init-control|promote-control>")
	}
	switch args[0] {
	case "status":
		return centralStatus(args[1:])
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

func centralStatus(args []string) error {
	return centralStatusWith(raftstatus.ExecRunner{}, os.Stdout, args)
}

func centralStatusWith(runner raftstatus.Runner, output io.Writer, args []string) error {
	flags := flag.NewFlagSet("central status", flag.ContinueOnError)
	controlSocket := flags.String("pvn-control-ctl", raftstatus.DefaultControlSocket, "exact PVN Control unixctl socket path")
	nbSocket := flags.String("ovn-nb-ctl", raftstatus.DefaultNBSocket, "exact OVN Northbound unixctl socket path")
	sbSocket := flags.String("ovn-sb-ctl", raftstatus.DefaultSBSocket, "exact OVN Southbound unixctl socket path")
	timeout := flags.Duration("timeout", raftstatus.DefaultTimeout, "timeout for each local database query")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("central status does not accept positional arguments")
	}
	if *timeout < time.Second || *timeout > time.Minute {
		return errors.New("--timeout must be between 1s and 1m")
	}

	report := raftstatus.Inspect(context.Background(), runner, raftstatus.DefaultAppctlPath, *timeout, []raftstatus.Target{
		{Component: "pvn-control", Database: centraldb.SchemaName, ControlSocket: *controlSocket},
		{Component: "ovn-northbound", Database: "OVN_Northbound", ControlSocket: *nbSocket},
		{Component: "ovn-southbound", Database: "OVN_Southbound", ControlSocket: *sbSocket},
	})
	if err := writeJSON(output, report); err != nil {
		return err
	}
	if !report.Healthy {
		return errors.New("one or more local Raft databases are unavailable or unhealthy")
	}
	return nil
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
	local := flags.String("local", "", "local Raft address (required format: ssl:IPv4:6646)")
	join := flags.String("join", "", "comma-separated existing Raft members")
	clusterID := flags.String("cid", "", "exact existing OVSDB Raft cluster ID for a join")
	confirmation := flags.String("confirm", "", "exact PVN cluster ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := confirmedConfig(*configPath, *confirmation)
	if err != nil {
		return err
	}
	if err := centraldb.Init(context.Background(), centraldb.ExecRunner{}, centraldb.InitOptions{
		Database:  *database,
		Schema:    *schema,
		Mode:      *mode,
		Local:     *local,
		Remotes:   splitList(*join),
		ClusterID: *clusterID,
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
	local := flags.String("local", "", "local Raft address (required format: ssl:IPv4:6646)")
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
	if len(args) == 0 {
		return errors.New("usage: pvnctl node <configure-ovn|can-remove>")
	}
	switch args[0] {
	case "configure-ovn":
		return nodeConfigureOVN(args[1:])
	case "can-remove":
		return nodeCanRemove(args[1:])
	default:
		return fmt.Errorf("unknown node command %q", args[0])
	}
}

func nodeConfigureOVN(args []string) error {
	flags := flag.NewFlagSet("node configure-ovn", flag.ContinueOnError)
	configPath := flags.String("config", config.DefaultPath, "PVN config path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := hostconfig.ApplyOVN(context.Background(), centraldb.ExecRunner{}, hostconfig.Config{
		IntegrationBridge: cfg.Agent.Bridge,
		ProviderBridge:    cfg.Networking.ProviderBridge,
		PhysicalNetwork:   cfg.Networking.Physnet,
		EncapType:         cfg.Networking.EncapType,
		EncapIP:           cfg.Networking.EncapIP,
		Southbound:        cfg.OVN.Southbound,
	}); err != nil {
		return err
	}
	fmt.Printf("configured local ovn-controller for %s\n", cfg.Cluster.NodeName)
	return nil
}

func nodeCanRemove(args []string) error {
	flags := flag.NewFlagSet("node can-remove", flag.ContinueOnError)
	_ = flags.Bool("local", false, "check the local node")
	path := flags.String("state", nodestate.DefaultPath, "node state path")
	configPath := flags.String("config", config.DefaultPath, "PVN config path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	state, err := nodestate.Load(*path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, configErr := os.Stat(*configPath); errors.Is(configErr, os.ErrNotExist) {
				fmt.Println("PVN is not configured on this node; package removal is safe")
				return nil
			}
		}
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

func writeJSON(file io.Writer, value any) error {
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
