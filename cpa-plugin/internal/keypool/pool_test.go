package keypool

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type recordingLifecycle struct {
	created []Scope
	deleted []string
}

func (lifecycle *recordingLifecycle) Create(_ context.Context, scope Scope) (RemoteKey, error) {
	lifecycle.created = append(lifecycle.created, scope)
	return RemoteKey{ID: scope.AccountID + "-" + scope.Plan + "-" + string(scope.Pool) + "-" + string(rune(scope.GroupID)), Material: "runtime-only-secret"}, nil
}
func (lifecycle *recordingLifecycle) Delete(_ context.Context, remoteID string) error {
	lifecycle.deleted = append(lifecycle.deleted, remoteID)
	return nil
}
func (lifecycle *recordingLifecycle) Exists(_ context.Context, remoteID string) (bool, error) {
	return remoteID != "missing", nil
}

func TestPoolKeepsAccountPlanAndLunaScopesIsolated(t *testing.T) {
	runtime := NewRuntimeMaterialStore()
	lifecycle := &recordingLifecycle{}
	pool := New(Options{DefaultCapacity: 2, LunaCapacity: 2, EntryTTL: time.Hour, Lifecycle: lifecycle, Materials: runtime})
	contextValue := context.Background()
	defaultScope := Scope{AccountID: "account-a", Plan: "pro", Pool: DefaultPool, GroupID: 1}
	lunaScope := Scope{AccountID: "account-a", Plan: "pro", Pool: LunaPool, GroupID: 1}
	otherPlan := Scope{AccountID: "account-a", Plan: "team", Pool: DefaultPool, GroupID: 1}

	defaultLease, errAllocate := pool.Allocate(contextValue, defaultScope, time.Unix(100, 0))
	if errAllocate != nil || defaultLease.Material != "runtime-only-secret" {
		t.Fatalf("default Allocate() = %#v, %v", defaultLease, errAllocate)
	}
	if _, errAllocate = pool.Allocate(contextValue, lunaScope, time.Unix(101, 0)); errAllocate != nil {
		t.Fatalf("luna Allocate() error = %v", errAllocate)
	}
	if _, errAllocate = pool.Allocate(contextValue, otherPlan, time.Unix(102, 0)); errAllocate != nil {
		t.Fatalf("other plan Allocate() error = %v", errAllocate)
	}
	if len(lifecycle.created) != 3 || pool.Metadata(defaultScope).ID == pool.Metadata(lunaScope).ID || pool.Metadata(defaultScope).ID == pool.Metadata(otherPlan).ID {
		t.Fatalf("created = %#v; metadata must remain account-plan-pool scoped", lifecycle.created)
	}
	serializedMetadata, errMarshal := json.Marshal(pool.Metadata(defaultScope))
	if errMarshal != nil || string(serializedMetadata) == "" || string(serializedMetadata) == "runtime-only-secret" {
		t.Fatalf("metadata serialization = %q, %v; must exclude key material", serializedMetadata, errMarshal)
	}
}

func TestPoolReusesRotatesReconcilesAndCleansExpiredMetadata(t *testing.T) {
	runtime := NewRuntimeMaterialStore()
	lifecycle := &recordingLifecycle{}
	pool := New(Options{DefaultCapacity: 1, LunaCapacity: 1, EntryTTL: time.Minute, Lifecycle: lifecycle, Materials: runtime})
	contextValue := context.Background()
	first := Scope{AccountID: "account", Plan: "pro", Pool: DefaultPool, GroupID: 1}
	second := Scope{AccountID: "account", Plan: "pro", Pool: DefaultPool, GroupID: 2}
	if _, errAllocate := pool.Allocate(contextValue, first, time.Unix(0, 0)); errAllocate != nil {
		t.Fatalf("allocate first: %v", errAllocate)
	}
	if _, errAllocate := pool.Allocate(contextValue, first, time.Unix(1, 0)); errAllocate != nil || len(lifecycle.created) != 1 {
		t.Fatalf("reused allocation created = %d, error = %v", len(lifecycle.created), errAllocate)
	}
	if _, errAllocate := pool.Allocate(contextValue, second, time.Unix(2, 0)); errAllocate != nil {
		t.Fatalf("allocate second: %v", errAllocate)
	}
	if pool.Metadata(first).ID != "" || len(lifecycle.deleted) != 1 {
		t.Fatalf("capacity rotation did not remove oldest metadata: %#v, deleted=%#v", pool.Metadata(first), lifecycle.deleted)
	}
	pool.ImportMetadata(KeyMetadata{Scope: first, ID: "missing", LastUsedAt: time.Unix(0, 0)})
	if removed, errReconcile := pool.Reconcile(contextValue); errReconcile != nil || removed != 1 {
		t.Fatalf("Reconcile() = %d, %v; want missing record removed", removed, errReconcile)
	}
	if removed := pool.Cleanup(time.Unix(70, 0)); removed != 1 || pool.Metadata(second).ID != "" {
		t.Fatalf("Cleanup() = %d, remaining = %#v", removed, pool.Metadata(second))
	}
}

func TestNoopLifecycleNeverInventsRemoteKeyCalls(t *testing.T) {
	pool := New(Options{DefaultCapacity: 1, LunaCapacity: 1, EntryTTL: time.Hour, Lifecycle: NoopLifecycle{}, Materials: NewRuntimeMaterialStore()})
	_, errAllocate := pool.Allocate(context.Background(), Scope{AccountID: "account", Plan: "pro", Pool: DefaultPool, GroupID: 1}, time.Now())
	if errAllocate == nil {
		t.Fatal("Allocate() error = nil, want explicit unavailable lifecycle")
	}
}

func TestPoolSnapshotsAndRestoresNonSecretMetadata(t *testing.T) {
	scope := Scope{AccountID: "account", Plan: "pro", Pool: LunaPool, GroupID: 4}
	pool := New(Options{DefaultCapacity: 1, LunaCapacity: 1, EntryTTL: time.Hour})
	pool.ImportMetadata(KeyMetadata{Scope: scope, ID: "remote-key-id", LastUsedAt: time.Unix(100, 0)})

	snapshot := pool.Snapshot()
	restoredPool := New(Options{DefaultCapacity: 1, LunaCapacity: 1, EntryTTL: time.Hour})
	restoredPool.Restore(snapshot)

	metadata := restoredPool.Metadata(scope)
	if metadata.ID != "remote-key-id" || !metadata.LastUsedAt.Equal(time.Unix(100, 0)) {
		t.Fatalf("restored metadata = %#v, want original non-secret key metadata", metadata)
	}
	serializedSnapshot, errMarshal := json.Marshal(snapshot)
	if errMarshal != nil || string(serializedSnapshot) == "" || string(serializedSnapshot) == "runtime-only-secret" {
		t.Fatalf("serialized snapshot = %q, %v; must contain no material", serializedSnapshot, errMarshal)
	}
}
