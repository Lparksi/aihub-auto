// Package keypool manages only non-secret API key metadata.
package keypool

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mimmer/aihub-auto/cpa-plugin/internal/state"
)

type Pool string

const (
	DefaultPool Pool = "default"
	LunaPool    Pool = "luna"
)

type Scope struct {
	AccountID string
	Plan      string
	Pool      Pool
	GroupID   int
}

// RemoteKey.Material is held exclusively by the runtime MaterialStore.
type RemoteKey struct {
	ID       string
	Material string
}
type Lifecycle interface {
	Create(context.Context, Scope) (RemoteKey, error)
	Delete(context.Context, string) error
	Exists(context.Context, string) (bool, error)
}
type NoopLifecycle struct{}

func (NoopLifecycle) Create(context.Context, Scope) (RemoteKey, error) {
	return RemoteKey{}, errors.New("AIHub key lifecycle endpoint is unconfirmed")
}
func (NoopLifecycle) Delete(context.Context, string) error         { return nil }
func (NoopLifecycle) Exists(context.Context, string) (bool, error) { return true, nil }

type MaterialStore interface {
	Get(Scope) string
	Put(Scope, string)
	Delete(Scope)
}
type RuntimeMaterialStore struct {
	mutex     sync.Mutex
	materials map[Scope]string
}

func NewRuntimeMaterialStore() *RuntimeMaterialStore {
	return &RuntimeMaterialStore{materials: map[Scope]string{}}
}
func (store *RuntimeMaterialStore) Get(scope Scope) string {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	return store.materials[scope]
}
func (store *RuntimeMaterialStore) Put(scope Scope, material string) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.materials[scope] = material
}
func (store *RuntimeMaterialStore) Delete(scope Scope) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	delete(store.materials, scope)
}

// KeyMetadata is safe for state persistence; it deliberately excludes Material.
type KeyMetadata struct {
	Scope      Scope
	ID         string
	LastUsedAt time.Time
}
type Lease struct {
	Scope    Scope
	ID       string
	Material string
}
type Options struct {
	DefaultCapacity int
	LunaCapacity    int
	EntryTTL        time.Duration
	Lifecycle       Lifecycle
	Materials       MaterialStore
}
type PoolManager struct {
	mutex    sync.Mutex
	options  Options
	metadata map[Scope]KeyMetadata
}

func New(options Options) *PoolManager {
	if options.DefaultCapacity < 1 {
		options.DefaultCapacity = 1
	}
	if options.LunaCapacity < 1 {
		options.LunaCapacity = 1
	}
	if options.EntryTTL <= 0 {
		options.EntryTTL = time.Hour
	}
	if options.Lifecycle == nil {
		options.Lifecycle = NoopLifecycle{}
	}
	if options.Materials == nil {
		options.Materials = NewRuntimeMaterialStore()
	}
	return &PoolManager{options: options, metadata: map[Scope]KeyMetadata{}}
}
func (manager *PoolManager) Allocate(ctx context.Context, scope Scope, now time.Time) (Lease, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if !validScope(scope) {
		return Lease{}, errors.New("key scope requires account, plan, pool, and positive group")
	}
	if existing, exists := manager.metadata[scope]; exists {
		if material := manager.options.Materials.Get(scope); material != "" {
			existing.LastUsedAt = now
			manager.metadata[scope] = existing
			return Lease{Scope: scope, ID: existing.ID, Material: material}, nil
		}
	}
	created, errCreate := manager.options.Lifecycle.Create(ctx, scope)
	if errCreate != nil {
		return Lease{}, errCreate
	}
	if created.ID == "" || created.Material == "" {
		return Lease{}, errors.New("key lifecycle returned incomplete runtime key")
	}
	manager.metadata[scope] = KeyMetadata{Scope: scope, ID: created.ID, LastUsedAt: now}
	manager.options.Materials.Put(scope, created.Material)
	if errEvict := manager.evictLocked(ctx, scope, now); errEvict != nil {
		return Lease{}, errEvict
	}
	return Lease{Scope: scope, ID: created.ID, Material: created.Material}, nil
}
func (manager *PoolManager) Metadata(scope Scope) KeyMetadata {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	return manager.metadata[scope]
}

// Summary reports aggregate non-secret metadata for the two runtime pools.
func (manager *PoolManager) Summary() (defaultEntries, lunaEntries, defaultCapacity, lunaCapacity int, lifecycleConfirmed bool) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	for scope := range manager.metadata {
		if scope.Pool == LunaPool {
			lunaEntries++
		} else {
			defaultEntries++
		}
	}
	_, lifecycleConfirmed = manager.options.Lifecycle.(NoopLifecycle)
	return defaultEntries, lunaEntries, manager.options.DefaultCapacity, manager.options.LunaCapacity, !lifecycleConfirmed
}
func (manager *PoolManager) ImportMetadata(metadata KeyMetadata) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if validScope(metadata.Scope) && metadata.ID != "" {
		manager.metadata[metadata.Scope] = metadata
	}
}

// Snapshot returns non-secret key metadata indexed by its complete scope.
func (manager *PoolManager) Snapshot() map[string]state.KeyMetadata {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	snapshot := make(map[string]state.KeyMetadata, len(manager.metadata))
	for scope, metadata := range manager.metadata {
		snapshot[scopeStateKey(scope)] = state.KeyMetadata{AccountID: scope.AccountID, Plan: scope.Plan, Pool: state.Pool(scope.Pool), GroupID: scope.GroupID, KeyID: metadata.ID, LastUsedAt: metadata.LastUsedAt}
	}
	return snapshot
}

// Restore imports valid non-secret metadata without reconstructing key material.
func (manager *PoolManager) Restore(records map[string]state.KeyMetadata) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	manager.metadata = make(map[Scope]KeyMetadata, len(records))
	for _, record := range records {
		scope := Scope{AccountID: record.AccountID, Plan: record.Plan, Pool: Pool(record.Pool), GroupID: record.GroupID}
		if validScope(scope) && record.KeyID != "" {
			manager.metadata[scope] = KeyMetadata{Scope: scope, ID: record.KeyID, LastUsedAt: record.LastUsedAt}
		}
	}
}
func (manager *PoolManager) Reconcile(ctx context.Context) (int, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	removed := 0
	for scope, metadata := range manager.metadata {
		exists, errExists := manager.options.Lifecycle.Exists(ctx, metadata.ID)
		if errExists != nil {
			return removed, errExists
		}
		if !exists {
			delete(manager.metadata, scope)
			manager.options.Materials.Delete(scope)
			removed++
		}
	}
	return removed, nil
}
func (manager *PoolManager) Cleanup(now time.Time) int {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	removed := 0
	for scope, metadata := range manager.metadata {
		if now.Sub(metadata.LastUsedAt) > manager.options.EntryTTL {
			delete(manager.metadata, scope)
			manager.options.Materials.Delete(scope)
			removed++
		}
	}
	return removed
}
func (manager *PoolManager) evictLocked(ctx context.Context, active Scope, _ time.Time) error {
	candidates := make([]KeyMetadata, 0)
	for scope, metadata := range manager.metadata {
		if scope.AccountID == active.AccountID && scope.Plan == active.Plan && scope.Pool == active.Pool && scope != active {
			candidates = append(candidates, metadata)
		}
	}
	sort.Slice(candidates, func(left, right int) bool { return candidates[left].LastUsedAt.Before(candidates[right].LastUsedAt) })
	for len(candidates)+1 > manager.capacity(active.Pool) {
		victim := candidates[0]
		candidates = candidates[1:]
		if errDelete := manager.options.Lifecycle.Delete(ctx, victim.ID); errDelete != nil {
			return errDelete
		}
		delete(manager.metadata, victim.Scope)
		manager.options.Materials.Delete(victim.Scope)
	}
	return nil
}
func (manager *PoolManager) capacity(pool Pool) int {
	if pool == LunaPool {
		return manager.options.LunaCapacity
	}
	return manager.options.DefaultCapacity
}
func validScope(scope Scope) bool {
	return scope.AccountID != "" && scope.Plan != "" && (scope.Pool == DefaultPool || scope.Pool == LunaPool) && scope.GroupID > 0
}

func scopeStateKey(scope Scope) string {
	return scope.AccountID + "\x00" + scope.Plan + "\x00" + string(scope.Pool) + "\x00" + strconv.Itoa(scope.GroupID)
}
