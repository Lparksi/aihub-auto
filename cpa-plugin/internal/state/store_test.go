package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreRoundTripsVersionedNonSecretStateAtomically(t *testing.T) {
	directory := t.TempDir()
	store := NewStore(directory)
	written := Snapshot{
		Routing:  RoutingSnapshot{Breaker: map[string]BreakerRecord{"account|pro|1": {Failures: 2}}, Observations: map[string]ObservationRecord{"account|pro|1": {Samples: 3}}},
		Sessions: map[string]SessionRecord{"hashed-session": {AccountID: "account", Plan: "pro", Pool: PoolDefault, GroupID: 1, Model: "gpt-4", LastUsedAt: time.Unix(100, 0)}},
		Aliases:  map[string]AliasRecord{"hashed-response": {SessionKey: "hashed-session", AccountID: "account", Plan: "pro", Pool: PoolDefault, GroupID: 1, LastUsedAt: time.Unix(100, 0)}},
		Keys:     map[string]KeyMetadata{"account|pro|default|1": {AccountID: "account", Plan: "pro", Pool: PoolDefault, GroupID: 1, KeyID: "remote-id", LastUsedAt: time.Unix(100, 0)}},
	}
	if errSave := store.Save(written); errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}

	raw, errRead := os.ReadFile(filepath.Join(directory, DefaultFileName))
	if errRead != nil {
		t.Fatalf("read persisted state: %v", errRead)
	}
	if strings.Contains(string(raw), "sk-") || strings.Contains(string(raw), "access_token") {
		t.Fatalf("state contains secret-like material: %s", raw)
	}
	if permissions := fileMode(t, filepath.Join(directory, DefaultFileName)); permissions != 0o600 {
		t.Fatalf("state permissions = %o, want 600", permissions)
	}

	loaded, result := store.Load()
	if result != LoadOK || loaded.Version != CurrentVersion || loaded.Keys["account|pro|default|1"].KeyID != "remote-id" {
		t.Fatalf("Load() = %#v, %v; want versioned state", loaded, result)
	}
}

func TestStoreMigratesLegacySnapshotAndSafelyDiscardsCorruptState(t *testing.T) {
	directory := t.TempDir()
	store := NewStore(directory)
	legacy := `{"version":0,"sessions":{"session":{"account_id":"account","plan":"pro","pool":"default","group_id":1,"last_used_at":"1970-01-01T00:00:00Z"}}}`
	if errWrite := os.WriteFile(filepath.Join(directory, DefaultFileName), []byte(legacy), 0o600); errWrite != nil {
		t.Fatalf("write legacy state: %v", errWrite)
	}
	migrated, result := store.Load()
	if result != LoadMigrated || migrated.Version != CurrentVersion || migrated.Sessions["session"].Pool != PoolDefault {
		t.Fatalf("Load legacy = %#v, %v; want migrated default pool", migrated, result)
	}
	if errWrite := os.WriteFile(filepath.Join(directory, DefaultFileName), []byte(`{"version":`), 0o600); errWrite != nil {
		t.Fatalf("write corrupt state: %v", errWrite)
	}
	corrupt, result := store.Load()
	if result != LoadCorrupt || len(corrupt.Sessions) != 0 || corrupt.Version != CurrentVersion {
		t.Fatalf("Load corrupt = %#v, %v; want safe empty state", corrupt, result)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("stat state: %v", errStat)
	}
	return info.Mode().Perm()
}
