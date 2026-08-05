package ovsdbstore

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/popododo0720/proxmox-ovn/internal/controlschema"
)

var errSerialization = errors.New("PVN control database serialization conflict")

const (
	controlDBReconnectTimeout         = 5 * time.Second
	controlDBReconnectInitialInterval = 250 * time.Millisecond
	controlDBReconnectMaxInterval     = 5 * time.Second
)

type rawDatabase map[string][]ovsdb.Row

type rawRuntimePortLookup struct {
	nodes    []ovsdb.Row
	projects []ovsdb.Row
	ports    []ovsdb.Row
}

type changeType uint8

const (
	changeInsert changeType = iota + 1
	changeUpdate
	changeDelete
)

type change struct {
	type_            changeType
	table            string
	id               string
	expectedRevision int64
	row              ovsdb.Row
}

type database interface {
	load(context.Context) (rawDatabase, error)
	lookupRuntimePorts(context.Context, int, string) (rawRuntimePortLookup, error)
	initialize(context.Context, ovsdb.Row) error
	commit(context.Context, int64, []change, string) error
	close()
}

type ovsDatabase struct {
	client client.Client
}

func openDatabase(ctx context.Context, cfg Config) (*ovsDatabase, error) {
	return openDatabaseWithLogOutput(ctx, cfg, os.Stderr)
}

func openDatabaseWithLogOutput(ctx context.Context, cfg Config, logOutput io.Writer) (*ovsDatabase, error) {
	if err := validateConnectionConfig(cfg); err != nil {
		return nil, err
	}
	dbModel, err := controlschema.FullDatabaseModel()
	if err != nil {
		return nil, fmt.Errorf("build PVN control database model: %w", err)
	}
	options := make([]client.Option, 0, len(cfg.Endpoints)+3)
	for _, endpoint := range cfg.Endpoints {
		options = append(options, client.WithEndpoint(endpoint))
	}
	libovsdbLogger := newLibovsdbLogger(logOutput)
	options = append(options,
		client.WithLeaderOnly(true),
		client.WithReconnect(controlDBReconnectTimeout, newControlDBReconnectBackOff()),
		client.WithLogger(&libovsdbLogger),
	)
	if cfg.TLSConfig != nil {
		options = append(options, client.WithTLSConfig(cfg.TLSConfig.Clone()))
	}
	ovsClient, err := client.NewOVSDBClient(dbModel, options...)
	if err != nil {
		return nil, fmt.Errorf("create PVN control database client: %w", err)
	}
	if err := ovsClient.Connect(ctx); err != nil {
		ovsClient.Close()
		return nil, fmt.Errorf("connect to PVN control database: %w", err)
	}
	if _, err := ovsClient.MonitorAll(ctx); err != nil {
		ovsClient.Close()
		return nil, fmt.Errorf("monitor PVN control database: %w", err)
	}
	return &ovsDatabase{client: ovsClient}, nil
}

func newControlDBReconnectBackOff() *backoff.ExponentialBackOff {
	retry := backoff.NewExponentialBackOff()
	retry.InitialInterval = controlDBReconnectInitialInterval
	retry.MaxInterval = controlDBReconnectMaxInterval
	// A transient or prolonged control-plane restart must not permanently
	// disable the long-running manager.
	retry.MaxElapsedTime = 0
	retry.Reset()
	return retry
}

func validateConnectionConfig(cfg Config) error {
	if len(cfg.Endpoints) == 0 {
		return errors.New("at least one PVN control database endpoint is required")
	}
	needsTLS := false
	for _, endpoint := range cfg.Endpoints {
		switch {
		case strings.HasPrefix(endpoint, "unix:") && len(strings.TrimPrefix(endpoint, "unix:")) > 0:
		case strings.HasPrefix(endpoint, "ssl:") && len(strings.TrimPrefix(endpoint, "ssl:")) > 0:
			needsTLS = true
		default:
			return fmt.Errorf("unsupported PVN control database endpoint %q; only unix: and ssl: are allowed", endpoint)
		}
	}
	if !needsTLS {
		return nil
	}
	if cfg.TLSConfig == nil {
		return errors.New("TLS configuration is required for ssl: PVN control database endpoints")
	}
	if cfg.TLSConfig.InsecureSkipVerify {
		return errors.New("insecure TLS verification is forbidden for the PVN control database")
	}
	if cfg.TLSConfig.RootCAs == nil {
		return errors.New("a trusted CA pool is required for the PVN control database")
	}
	if len(cfg.TLSConfig.Certificates) == 0 && cfg.TLSConfig.GetClientCertificate == nil {
		return errors.New("a client certificate is required for the PVN control database")
	}
	if cfg.TLSConfig.MinVersion != 0 && cfg.TLSConfig.MinVersion < tls.VersionTLS12 {
		return errors.New("PVN control database TLS must require TLS 1.2 or newer")
	}
	return nil
}

func (d *ovsDatabase) load(ctx context.Context) (rawDatabase, error) {
	tables := allTables()
	operations := make([]ovsdb.Operation, 0, len(tables))
	for _, table := range tables {
		operations = append(operations, ovsdb.Operation{Op: ovsdb.OperationSelect, Table: table})
	}
	results, err := d.client.Transact(ctx, operations...)
	if err != nil {
		return nil, fmt.Errorf("read PVN control database: %w", err)
	}
	if _, err := ovsdb.CheckOperationResults(results, operations); err != nil {
		return nil, fmt.Errorf("read PVN control database: %w", err)
	}
	if len(results) != len(operations) {
		return nil, fmt.Errorf("read PVN control database: expected %d results, got %d", len(operations), len(results))
	}
	raw := make(rawDatabase, len(tables))
	for index, table := range tables {
		raw[table] = results[index].Rows
	}
	return raw, nil
}

func (d *ovsDatabase) lookupRuntimePorts(ctx context.Context, vmid int, nic string) (rawRuntimePortLookup, error) {
	operations := runtimePortLookupOperations(vmid, nic)
	results, err := d.client.Transact(ctx, operations...)
	if err != nil {
		return rawRuntimePortLookup{}, fmt.Errorf("lookup runtime ports in PVN control database: %w", err)
	}
	if _, err := ovsdb.CheckOperationResults(results, operations); err != nil {
		return rawRuntimePortLookup{}, fmt.Errorf("lookup runtime ports in PVN control database: %w", err)
	}
	if len(results) != len(operations) {
		return rawRuntimePortLookup{}, fmt.Errorf("lookup runtime ports in PVN control database: expected %d results, got %d", len(operations), len(results))
	}
	return rawRuntimePortLookup{nodes: results[0].Rows, projects: results[1].Rows, ports: results[2].Rows}, nil
}

func runtimePortLookupOperations(vmid int, nic string) []ovsdb.Operation {
	return []ovsdb.Operation{
		{
			Op:      ovsdb.OperationSelect,
			Table:   controlschema.NodeTable,
			Columns: []string{"_uuid", "id", "name", "chassis_id"},
		},
		{
			Op:      ovsdb.OperationSelect,
			Table:   controlschema.ProjectTable,
			Columns: []string{"_uuid", "id"},
		},
		{
			Op:    ovsdb.OperationSelect,
			Table: controlschema.PortTable,
			Where: []ovsdb.Condition{
				ovsdb.NewCondition("vmid", ovsdb.ConditionEqual, vmid),
				ovsdb.NewCondition("nic", ovsdb.ConditionEqual, nic),
			},
			Columns: []string{
				"id", "revision", "applied_revision", "state", "project", "node",
				"vmid", "nic", "lsp_name", "generation", "requested_chassis",
				"mac_address", "admin_state_up", "binding_status",
			},
		},
	}
}

func (d *ovsDatabase) initialize(ctx context.Context, lock ovsdb.Row) error {
	durable := true
	operations := []ovsdb.Operation{
		{Op: ovsdb.OperationInsert, Table: controlschema.OperationTable, Row: lock},
		{Op: ovsdb.OperationCommit, Durable: &durable},
	}
	results, err := d.client.Transact(ctx, operations...)
	if err != nil {
		return fmt.Errorf("initialize PVN store lock: %w", err)
	}
	if operationErrors, err := ovsdb.CheckOperationResults(results, operations); err != nil {
		if hasConstraintError(operationErrors, err) {
			return errSerialization
		}
		return fmt.Errorf("initialize PVN store lock: %w (%v)", err, operationErrors)
	}
	return nil
}

func (d *ovsDatabase) commit(ctx context.Context, epoch int64, changes []change, updatedAt string) error {
	operations := buildOperations(epoch, changes, updatedAt)
	results, err := d.client.Transact(ctx, operations...)
	if err != nil {
		return fmt.Errorf("commit PVN control transaction: %w", err)
	}
	if operationErrors, err := ovsdb.CheckOperationResults(results, operations); err != nil {
		if hasWaitError(operationErrors, err) {
			return errSerialization
		}
		if hasConstraintError(operationErrors, err) {
			return &constraintError{cause: fmt.Errorf("%w (%v)", err, operationErrors), reference: hasReferentialError(operationErrors, err)}
		}
		return fmt.Errorf("commit PVN control transaction: %w (%v)", err, operationErrors)
	}
	if len(results) != len(operations) {
		return fmt.Errorf("commit PVN control transaction: expected %d results, got %d", len(operations), len(results))
	}
	// The lock update is operation 1. A zero count would permit later operations
	// to commit, so the preceding zero-timeout wait is the actual CAS barrier.
	if results[1].Count != 1 {
		return fmt.Errorf("commit PVN control transaction: store lock update affected %d rows", results[1].Count)
	}
	for index, item := range changes {
		result := results[index+2]
		if (item.type_ == changeUpdate || item.type_ == changeDelete) && result.Count != 1 {
			return fmt.Errorf("commit PVN control transaction: %s %q affected %d rows", item.table, item.id, result.Count)
		}
	}
	return nil
}

func buildOperations(epoch int64, changes []change, updatedAt string) []ovsdb.Operation {
	zero := 0
	durable := true
	operations := make([]ovsdb.Operation, 0, len(changes)+3)
	operations = append(operations, ovsdb.Operation{
		Op:      ovsdb.OperationWait,
		Table:   controlschema.OperationTable,
		Timeout: &zero,
		Where: []ovsdb.Condition{
			ovsdb.NewCondition("id", ovsdb.ConditionEqual, storeLockID),
		},
		Columns: []string{"revision"},
		Until:   string(ovsdb.WaitConditionEqual),
		Rows:    []ovsdb.Row{{"revision": epoch}},
	})
	operations = append(operations, ovsdb.Operation{
		Op:    ovsdb.OperationUpdate,
		Table: controlschema.OperationTable,
		Where: []ovsdb.Condition{
			ovsdb.NewCondition("id", ovsdb.ConditionEqual, storeLockID),
			ovsdb.NewCondition("revision", ovsdb.ConditionEqual, epoch),
		},
		Row: ovsdb.Row{"revision": epoch + 1, "updated_at": updatedAt},
	})
	for _, item := range changes {
		operation := ovsdb.Operation{Table: item.table}
		switch item.type_ {
		case changeInsert:
			operation.Op = ovsdb.OperationInsert
			operation.Row = item.row
		case changeUpdate:
			operation.Op = ovsdb.OperationUpdate
			operation.Row = item.row
			operation.Where = []ovsdb.Condition{
				ovsdb.NewCondition("id", ovsdb.ConditionEqual, item.id),
				ovsdb.NewCondition("revision", ovsdb.ConditionEqual, item.expectedRevision),
			}
		case changeDelete:
			operation.Op = ovsdb.OperationDelete
			operation.Where = []ovsdb.Condition{
				ovsdb.NewCondition("id", ovsdb.ConditionEqual, item.id),
				ovsdb.NewCondition("revision", ovsdb.ConditionEqual, item.expectedRevision),
			}
		}
		operations = append(operations, operation)
	}
	operations = append(operations, ovsdb.Operation{Op: ovsdb.OperationCommit, Durable: &durable})
	return operations
}

func (d *ovsDatabase) close() {
	if d != nil && d.client != nil {
		d.client.Close()
	}
}

type constraintError struct {
	cause     error
	reference bool
}

func (e *constraintError) Error() string { return e.cause.Error() }
func (e *constraintError) Unwrap() error { return e.cause }

func hasWaitError(operationErrors []ovsdb.OperationError, transactionError error) bool {
	for _, operationError := range operationErrors {
		var timedOut *ovsdb.TimedOut
		if errors.As(operationError, &timedOut) {
			return true
		}
	}
	var timedOut *ovsdb.TimedOut
	if errors.As(transactionError, &timedOut) {
		return true
	}
	value := strings.ToLower(transactionError.Error())
	return strings.Contains(value, "timed out") || strings.Contains(value, "timeout")
}

func hasConstraintError(operationErrors []ovsdb.OperationError, transactionError error) bool {
	for _, operationError := range operationErrors {
		var constraint *ovsdb.ConstraintViolation
		var reference *ovsdb.ReferentialIntegrityViolation
		if errors.As(operationError, &constraint) || errors.As(operationError, &reference) {
			return true
		}
	}
	var constraint *ovsdb.ConstraintViolation
	var reference *ovsdb.ReferentialIntegrityViolation
	if errors.As(transactionError, &constraint) || errors.As(transactionError, &reference) {
		return true
	}
	value := strings.ToLower(transactionError.Error())
	return strings.Contains(value, "constraint violation") ||
		strings.Contains(value, "referential integrity") ||
		strings.Contains(value, "duplicate")
}

func hasReferentialError(operationErrors []ovsdb.OperationError, transactionError error) bool {
	for _, operationError := range operationErrors {
		var reference *ovsdb.ReferentialIntegrityViolation
		if errors.As(operationError, &reference) {
			return true
		}
	}
	var reference *ovsdb.ReferentialIntegrityViolation
	if errors.As(transactionError, &reference) {
		return true
	}
	return strings.Contains(strings.ToLower(transactionError.Error()), "referential integrity")
}
