package ovsdbstore

import (
	"context"
	"sync"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/defaultsecurity"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func TestDefaultSecurityPolicyIsDurableAndConcurrent(t *testing.T) {
	database := newFakeDatabase()
	firstStore := deterministicStore(database)
	secondStore := deterministicStore(database)

	start := make(chan struct{})
	errorsByManager := make(chan error, 2)
	var wait sync.WaitGroup
	for _, store := range []controlstore.Store{firstStore, secondStore} {
		wait.Add(1)
		go func(store controlstore.Store) {
			defer wait.Done()
			<-start
			_, err := defaultsecurity.New(store, nil).Ensure(context.Background())
			errorsByManager <- err
		}(store)
	}
	close(start)
	wait.Wait()
	close(errorsByManager)
	for err := range errorsByManager {
		if err != nil {
			t.Fatal(err)
		}
	}

	group, err := secondStore.Get(context.Background(), model.KindSecurityGroup, defaultsecurity.DefaultSecurityGroupID())
	if err != nil {
		t.Fatal(err)
	}
	if group.(*model.SecurityGroup).Name != defaultsecurity.DefaultSecurityGroupName {
		t.Fatalf("group=%+v", group)
	}
	rules, err := secondStore.List(context.Background(), model.KindSecurityGroupRule, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("rules=%d want 2", len(rules))
	}
	// Every port using this one durable default security group belongs to the
	// same routed self-ingress trust domain across the cluster.
	for _, id := range []string{defaultsecurity.DefaultEgressRuleID(), defaultsecurity.DefaultIngressRuleID()} {
		if _, err := secondStore.Get(context.Background(), model.KindSecurityGroupRule, id); err != nil {
			t.Fatalf("Get(rule %s): %v", id, err)
		}
	}
}
