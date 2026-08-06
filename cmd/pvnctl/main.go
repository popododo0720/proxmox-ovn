package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/diagnostic"
	"github.com/popododo0720/proxmox-ovn/internal/hostconfig"
	"github.com/popododo0720/proxmox-ovn/internal/model"
	"github.com/popododo0720/proxmox-ovn/internal/nodestate"
	"github.com/popododo0720/proxmox-ovn/internal/ovnnb"
	"github.com/popododo0720/proxmox-ovn/internal/ovsdbstore"
	"github.com/popododo0720/proxmox-ovn/internal/pki"
	"github.com/popododo0720/proxmox-ovn/internal/raftstatus"
	"github.com/popododo0720/proxmox-ovn/internal/reconcile"
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
	case "recovery":
		return recoveryCommand(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type forcedReconciler interface {
	ReconcileAll(context.Context) error
}

type recoverySnapshotter interface {
	Snapshot(context.Context, []model.Kind, controlstore.ListOptions) (controlstore.ResourceSnapshot, error)
}

type verifiedRecoveryReconciler struct {
	reconciler forcedReconciler
	store      recoverySnapshotter
}

func (reconciler verifiedRecoveryReconciler) ReconcileAll(ctx context.Context) error {
	if reconciler.reconciler == nil || reconciler.store == nil {
		return errors.New("verified recovery reconciler is not configured")
	}
	before, err := reconciler.store.Snapshot(ctx, []model.Kind{model.KindOperation}, controlstore.ListOptions{})
	if err != nil {
		return fmt.Errorf("read pre-reconciliation operation snapshot: %w", err)
	}
	if err := reconciler.reconciler.ReconcileAll(ctx); err != nil {
		return err
	}
	after, err := reconciler.store.Snapshot(ctx, model.Kinds(), controlstore.ListOptions{})
	if err != nil {
		return fmt.Errorf("verify post-reconciliation control state: %w", err)
	}
	return verifyRecoveryReconcilePass(before, after)
}

type recoveryTarget struct {
	kind     model.Kind
	id       string
	revision int64
}

func verifyRecoveryReconcilePass(before, after controlstore.ResourceSnapshot) error {
	previousRevisions := make(map[string]int64)
	for _, resource := range before[model.KindOperation] {
		operation, ok := resource.(*model.Operation)
		if !ok {
			return errors.New("pre-reconciliation operation snapshot has an invalid resource type")
		}
		previousRevisions[operation.ID] = operation.Revision
	}

	operations := make(map[recoveryTarget][]*model.Operation)
	for _, resource := range after[model.KindOperation] {
		operation, ok := resource.(*model.Operation)
		if !ok {
			return errors.New("post-reconciliation operation snapshot has an invalid resource type")
		}
		if operation.Action != "reconcile" {
			continue
		}
		target := recoveryTarget{kind: operation.TargetKind, id: operation.TargetID, revision: operation.TargetRevision}
		operations[target] = append(operations[target], operation)
	}

	var incomplete []string
	for _, kind := range model.Kinds() {
		if kind == model.KindOperation {
			continue
		}
		for _, resource := range after[kind] {
			meta := resource.GetMetadata()
			label := fmt.Sprintf("%s %q revision %d", kind, meta.ID, meta.Revision)
			if meta.State != model.ResourceReady || meta.AppliedRevision != meta.Revision {
				incomplete = append(incomplete, fmt.Sprintf("%s is %s at applied revision %d", label, meta.State, meta.AppliedRevision))
				continue
			}
			matching := operations[recoveryTarget{kind: kind, id: meta.ID, revision: meta.Revision}]
			if len(matching) != 1 {
				incomplete = append(incomplete, fmt.Sprintf("%s has %d matching reconcile operations", label, len(matching)))
				continue
			}
			operation := matching[0]
			if operation.OperationStatus != model.OperationSucceeded || operation.CompletedAt == nil {
				incomplete = append(incomplete, fmt.Sprintf("%s reconcile operation is %s", label, operation.OperationStatus))
				continue
			}
			if previous, found := previousRevisions[operation.ID]; found && operation.Revision <= previous {
				incomplete = append(incomplete, fmt.Sprintf("%s reconcile operation did not complete in this pass", label))
			}
		}
	}
	if len(incomplete) == 0 {
		return nil
	}
	const reportLimit = 20
	reported := incomplete
	if len(reported) > reportLimit {
		reported = append(append([]string(nil), reported[:reportLimit]...), fmt.Sprintf("and %d more", len(incomplete)-reportLimit))
	}
	return fmt.Errorf("forced OVN reconciliation did not complete every current desired revision (a manager or writer lease may still be active): %s", strings.Join(reported, "; "))
}

type recoveryRuntime struct {
	reconciler forcedReconciler
	close      func()
}

type recoveryDependencies struct {
	getEUID    func() int
	loadConfig func(string) (config.Config, error)
	open       func(context.Context, config.Config) (recoveryRuntime, error)
	output     io.Writer
}

func recoveryCommand(args []string) error {
	return recoveryCommandWith(args, recoveryDependencies{
		getEUID:    os.Geteuid,
		loadConfig: config.Load,
		open:       openRecoveryRuntime,
		output:     os.Stdout,
	})
}

func recoveryCommandWith(args []string, dependencies recoveryDependencies) error {
	if len(args) == 0 {
		return errors.New("usage: pvnctl recovery reconcile-ovn")
	}
	if args[0] != "reconcile-ovn" {
		return fmt.Errorf("unknown recovery command %q", args[0])
	}
	flags := flag.NewFlagSet("recovery reconcile-ovn", flag.ContinueOnError)
	configPath := flags.String("config", config.DefaultPath, "PVN config path")
	confirmation := flags.String("confirm", "", "exact PVN cluster ID")
	apply := flags.Bool("apply", false, "perform a forced OVN Northbound reconciliation")
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum reconciliation duration")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("recovery reconcile-ovn does not accept positional arguments")
	}
	if dependencies.getEUID == nil || dependencies.getEUID() != 0 {
		return errors.New("recovery reconcile-ovn must run as root")
	}
	if !*apply {
		return errors.New("recovery reconciliation writes PVN Control and OVN; freeze every manager and pass --apply")
	}
	if *timeout < time.Minute || *timeout > 30*time.Minute {
		return errors.New("--timeout must be between 1m and 30m")
	}
	if dependencies.loadConfig == nil || dependencies.open == nil || dependencies.output == nil {
		return errors.New("recovery dependencies are incomplete")
	}
	cfg, err := dependencies.loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *confirmation == "" || *confirmation != cfg.Cluster.ID {
		return errors.New("--confirm must exactly match cluster.id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	runtime, err := dependencies.open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open recovery control plane: %w", err)
	}
	if runtime.close != nil {
		defer runtime.close()
	}
	if runtime.reconciler == nil {
		return errors.New("recovery reconciler is unavailable")
	}
	if err := runtime.reconciler.ReconcileAll(ctx); err != nil {
		return fmt.Errorf("force OVN Northbound reconciliation: %w", err)
	}
	return writeJSON(dependencies.output, map[string]string{
		"action":  "reconcile-ovn",
		"cluster": cfg.Cluster.ID,
		"status":  "succeeded",
	})
}

func openRecoveryRuntime(ctx context.Context, cfg config.Config) (recoveryRuntime, error) {
	var controlTLS *tls.Config
	if endpointsUseSSL(cfg.OVN.ControlDB) {
		var err error
		controlTLS, err = loadRecoveryMutualTLS(cfg.OVN.TLSCA, cfg.OVN.TLSCert, cfg.OVN.TLSKey)
		if err != nil {
			return recoveryRuntime{}, err
		}
	}
	store, err := ovsdbstore.Open(ctx, ovsdbstore.Config{Endpoints: cfg.OVN.ControlDB, TLSConfig: controlTLS})
	if err != nil {
		return recoveryRuntime{}, fmt.Errorf("open PVN control store: %w", err)
	}
	client, err := ovnnb.NewClient(ovnnb.ClientConfig{
		Database: cfg.OVN.Northbound, TLSCA: cfg.OVN.TLSCA,
		TLSCert: cfg.OVN.TLSCert, TLSKey: cfg.OVN.TLSKey, Timeout: 30,
	})
	if err != nil {
		store.Close()
		return recoveryRuntime{}, fmt.Errorf("configure OVN Northbound client: %w", err)
	}
	if err := client.Probe(ctx); err != nil {
		store.Close()
		return recoveryRuntime{}, err
	}
	renderer, err := ovnnb.NewRenderer(client, store)
	if err != nil {
		store.Close()
		return recoveryRuntime{}, err
	}
	controller := reconcile.NewController(store, renderer, reconcile.WithLeaseDuration(cfg.Cluster.OrphanGrace))
	return recoveryRuntime{
		reconciler: verifiedRecoveryReconciler{reconciler: controller, store: store},
		close:      store.Close,
	}, nil
}

func endpointsUseSSL(endpoints []string) bool {
	for _, endpoint := range endpoints {
		if strings.HasPrefix(endpoint, "ssl:") {
			return true
		}
	}
	return false
}

func loadRecoveryMutualTLS(caPath, certificatePath, keyPath string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read OVN CA certificate %q: %w", caPath, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("OVN CA certificate %q contains no certificates", caPath)
	}
	certificate, err := tls.LoadX509KeyPair(certificatePath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load OVN client certificate: %w", err)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: roots,
		Certificates: []tls.Certificate{certificate},
	}, nil
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
	return doctorWith(args, os.Stdout, diagnostic.CorosyncRuntimeCheck)
}

func doctorWith(
	args []string,
	output io.Writer,
	corosyncCheck func(context.Context, diagnostic.Runner) diagnostic.Check,
) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	path := flags.String("config", config.DefaultPath, "PVN config path")
	nodeEnvPath := flags.String("node-env", config.DefaultNodeEnvPath, "node-local PVN environment path")
	check := ""
	checkSet := false
	flags.Func("check", "run one configuration-independent safety check", func(value string) error {
		if checkSet {
			return errors.New("--check may be specified only once")
		}
		checkSet = true
		check = value
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("doctor does not accept positional arguments")
	}
	if checkSet {
		if check != diagnostic.CorosyncRuntimeCheckName {
			return fmt.Errorf("unsupported standalone doctor check %q", check)
		}
		checks := []diagnostic.Check{
			corosyncCheck(context.Background(), diagnostic.ExecRunner{}),
		}
		if err := writeJSON(output, checks); err != nil {
			return err
		}
		if !diagnostic.Healthy(checks) {
			return errors.New("one or more checks failed")
		}
		return nil
	}
	cfg, err := config.LoadNode(*path, *nodeEnvPath)
	if err != nil {
		return err
	}
	checks := diagnostic.Run(context.Background(), cfg, diagnostic.ExecRunner{})
	if err := writeJSON(output, checks); err != nil {
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
