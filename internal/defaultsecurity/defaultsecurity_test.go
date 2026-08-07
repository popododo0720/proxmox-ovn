package defaultsecurity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func TestClusterGlobalDeterministicIDsAreStableAndDistinct(t *testing.T) {
	ids := []string{DefaultSecurityGroupID(), DefaultEgressRuleID(), DefaultIngressRuleID()}
	seen := map[string]bool{}
	for _, id := range ids {
		if len(id) != 36 || seen[id] {
			t.Fatalf("invalid or duplicate deterministic ID %q", id)
		}
		seen[id] = true
	}
}

func TestAllPortsUsingDefaultSGShareOneRoutedSelfIngressTrustDomain(t *testing.T) {
	store := controlstore.NewMemory()
	manager := New(store, nil)

	first, err := manager.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.ID != DefaultSecurityGroupID() {
		t.Fatalf("default group IDs first=%q second=%q", first.ID, second.ID)
	}
	rules, err := store.List(context.Background(), model.KindSecurityGroupRule, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertClusterBaselineRules(t, rules)
}

func TestInspectIsReadOnlyThenReportsRealizedGlobalPolicyReady(t *testing.T) {
	store := controlstore.NewMemory()
	manager := New(store, nil)

	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantMissing := []string{DefaultSecurityGroupID(), DefaultEgressRuleID(), DefaultIngressRuleID()}
	if inspection.Ready || inspection.BlockedReason != BlockedNone || fmt.Sprint(inspection.MissingResourceIDs) != fmt.Sprint(wantMissing) {
		t.Fatalf("initial inspection=%+v", inspection)
	}
	groups, _ := store.List(context.Background(), model.KindSecurityGroup, controlstore.ListOptions{})
	if len(groups) != 0 {
		t.Fatalf("Inspect created %d groups", len(groups))
	}

	manager = New(store, &recordingReconciler{store: store})
	if _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	inspection, err = manager.Inspect(context.Background())
	if err != nil || !inspection.Ready || inspection.BlockedReason != BlockedNone {
		t.Fatalf("ready inspection=%+v error=%v", inspection, err)
	}
}

func TestConcurrentManagersEnsureOneGlobalPolicy(t *testing.T) {
	store := controlstore.NewMemory()
	const workers = 32
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := New(store, nil).Ensure(context.Background())
			errorsByWorker <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatal(err)
		}
	}
	groups, _ := store.List(context.Background(), model.KindSecurityGroup, controlstore.ListOptions{})
	rules, _ := store.List(context.Background(), model.KindSecurityGroupRule, controlstore.ListOptions{})
	if len(groups) != 1 || len(rules) != 2 {
		t.Fatalf("groups=%d rules=%d", len(groups), len(rules))
	}
}

func TestEnsureRepairsMissingGlobalBaselineRule(t *testing.T) {
	store := controlstore.NewMemory()
	manager := New(store, nil)
	if _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Get(context.Background(), model.KindSecurityGroupRule, DefaultIngressRuleID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Delete(context.Background(), model.KindSecurityGroupRule, rule.GetMetadata().ID, rule.GetMetadata().Revision, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	rules, _ := store.List(context.Background(), model.KindSecurityGroupRule, controlstore.ListOptions{})
	assertClusterBaselineRules(t, rules)
}

func TestEnsureRejectsGlobalDefaultNameCollision(t *testing.T) {
	store := controlstore.NewMemory()
	legacy := mustCreate(t, store, &model.SecurityGroup{Name: DefaultSecurityGroupName}).(*model.SecurityGroup)

	_, err := New(store, nil).Ensure(context.Background())
	if !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Ensure() error=%v want conflict", err)
	}
	if legacy.ID == DefaultSecurityGroupID() {
		t.Fatal("test collision unexpectedly used deterministic ID")
	}
	inspection, inspectErr := New(store, nil).Inspect(context.Background())
	if inspectErr != nil || inspection.BlockedReason != BlockedNameCollision {
		t.Fatalf("Inspect()=%+v error=%v", inspection, inspectErr)
	}
}

func TestEnsureRejectsDeterministicIDWithDifferentPolicy(t *testing.T) {
	store := controlstore.NewMemory()
	mustCreate(t, store, &model.SecurityGroup{
		Metadata: model.Metadata{ID: DefaultSecurityGroupID()},
		Name:     "not-default", Description: "untrusted",
	})

	_, err := New(store, nil).Ensure(context.Background())
	if !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Ensure() error=%v want conflict", err)
	}
	inspection, inspectErr := New(store, nil).Inspect(context.Background())
	if inspectErr != nil || inspection.BlockedReason != BlockedMalformedGroup {
		t.Fatalf("Inspect()=%+v error=%v", inspection, inspectErr)
	}
}

func TestEnsureRealizesRestrictiveGroupBeforeGlobalTrustRules(t *testing.T) {
	store := controlstore.NewMemory()
	reconciler := &recordingReconciler{store: store}
	group, err := New(store, reconciler).Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("%s/%s", model.KindSecurityGroup, DefaultSecurityGroupID()),
		fmt.Sprintf("%s/%s", model.KindSecurityGroupRule, DefaultEgressRuleID()),
		fmt.Sprintf("%s/%s", model.KindSecurityGroupRule, DefaultIngressRuleID()),
	}
	if fmt.Sprint(reconciler.calls) != fmt.Sprint(want) {
		t.Fatalf("reconcile order=%v want %v", reconciler.calls, want)
	}
	if group.State != model.ResourceReady || group.AppliedRevision != group.Revision {
		t.Fatalf("group was not required ready: %+v", group.Metadata)
	}
	if err := New(store, reconciler).EnsureAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reconciler.calls) != len(want) {
		t.Fatalf("ready policy was reconciled again: calls=%v", reconciler.calls)
	}
	if err := New(store, nil).Probe(context.Background()); err != nil {
		t.Fatalf("Probe rejected realized policy: %v", err)
	}
}

func TestEnsureStopsBeforeAllowRulesWhenGroupRealizationFails(t *testing.T) {
	store := controlstore.NewMemory()
	want := errors.New("OVN unavailable")
	reconciler := ReconcilerFunc(func(context.Context, model.Kind, string) error { return want })
	_, err := New(store, reconciler).Ensure(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Ensure() error=%v want %v", err, want)
	}
	if _, getErr := store.Get(context.Background(), model.KindSecurityGroupRule, DefaultEgressRuleID()); !errors.Is(getErr, controlstore.ErrNotFound) {
		t.Fatalf("allow rule exists after group realization failed: %v", getErr)
	}
}

func TestReservedResourcesAreOnlyTheGlobalBaseline(t *testing.T) {
	if !IsReserved(desiredGroup()) || !IsReserved(desiredEgressRule()) || !IsReserved(desiredIngressRule()) {
		t.Fatal("baseline resources were not reserved")
	}
	if IsReserved(&model.SecurityGroup{Metadata: model.Metadata{ID: "custom"}, Name: "custom"}) {
		t.Fatal("custom security group was reserved")
	}
}

type ReconcilerFunc func(context.Context, model.Kind, string) error

func (function ReconcilerFunc) Reconcile(ctx context.Context, kind model.Kind, id string) error {
	return function(ctx, kind, id)
}

type recordingReconciler struct {
	store controlstore.Store
	calls []string
}

func (r *recordingReconciler) Reconcile(ctx context.Context, kind model.Kind, id string) error {
	r.calls = append(r.calls, fmt.Sprintf("%s/%s", kind, id))
	resource, err := r.store.Get(ctx, kind, id)
	if err != nil {
		return err
	}
	_, err = r.store.MarkReconciled(ctx, kind, id, resource.GetMetadata().Revision, nil)
	return err
}

func mustCreate(t *testing.T, store controlstore.Store, resource model.Resource) model.Resource {
	t.Helper()
	created, replayed, err := store.Create(context.Background(), resource, "")
	if err != nil || replayed {
		t.Fatalf("Create(%s) replayed=%v: %v", resource.ResourceKind(), replayed, err)
	}
	return created
}

func assertClusterBaselineRules(t *testing.T, resources []model.Resource) {
	t.Helper()
	if len(resources) != 2 {
		t.Fatalf("rules=%d want 2", len(resources))
	}
	byID := make(map[string]*model.SecurityGroupRule, len(resources))
	for _, resource := range resources {
		byID[resource.GetMetadata().ID] = resource.(*model.SecurityGroupRule)
	}
	egress := byID[DefaultEgressRuleID()]
	if egress == nil || egress.Direction != model.DirectionEgress || egress.EtherType != model.EtherTypeIPv4 ||
		egress.Action != model.ActionAllow || egress.Protocol != "" || egress.RemoteCIDR != "" || egress.RemoteGroupID != "" {
		t.Fatalf("egress rule=%+v", egress)
	}
	ingress := byID[DefaultIngressRuleID()]
	if ingress == nil || ingress.Direction != model.DirectionIngress || ingress.EtherType != model.EtherTypeIPv4 ||
		ingress.Action != model.ActionAllow || ingress.RemoteGroupID != DefaultSecurityGroupID() ||
		ingress.Protocol != "" || ingress.RemoteCIDR != "" {
		t.Fatalf("ingress rule=%+v", ingress)
	}
}
