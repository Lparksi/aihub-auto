// Package state persists only non-secret plugin routing state.
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	CurrentVersion  = 1
	DefaultFileName = "state.json"
)

type Pool string

const (
	PoolDefault Pool = "default"
	PoolLuna    Pool = "luna"
)

type BreakerRecord struct {
	State    string    `json:"state,omitempty"`
	Failures int       `json:"failures"`
	Opens    int       `json:"opens,omitempty"`
	OpenedAt time.Time `json:"opened_at,omitempty"`
}
type ObservationRecord struct {
	Samples   int     `json:"samples"`
	Successes int     `json:"successes"`
	EWMA      float64 `json:"ewma"`
}
type RoutingSnapshot struct {
	Breaker      map[string]BreakerRecord     `json:"breaker"`
	Observations map[string]ObservationRecord `json:"observations"`
}
type SessionRecord struct {
	AccountID  string    `json:"account_id"`
	Plan       string    `json:"plan"`
	Pool       Pool      `json:"pool"`
	GroupID    int       `json:"group_id"`
	Model      string    `json:"model,omitempty"`
	LastUsedAt time.Time `json:"last_used_at"`
}
type AliasRecord struct {
	SessionKey string    `json:"session_key"`
	AccountID  string    `json:"account_id"`
	Plan       string    `json:"plan"`
	Pool       Pool      `json:"pool"`
	GroupID    int       `json:"group_id"`
	Model      string    `json:"model,omitempty"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// KeyMetadata intentionally has no API key material field.
type KeyMetadata struct {
	AccountID  string    `json:"account_id"`
	Plan       string    `json:"plan"`
	Pool       Pool      `json:"pool"`
	GroupID    int       `json:"group_id"`
	KeyID      string    `json:"key_id"`
	LastUsedAt time.Time `json:"last_used_at"`
}
type Snapshot struct {
	Version  int                      `json:"version"`
	Routing  RoutingSnapshot          `json:"routing"`
	Sessions map[string]SessionRecord `json:"sessions"`
	Aliases  map[string]AliasRecord   `json:"aliases"`
	Keys     map[string]KeyMetadata   `json:"keys"`
}
type LoadResult string

const (
	LoadOK       LoadResult = "ok"
	LoadMissing  LoadResult = "missing"
	LoadMigrated LoadResult = "migrated"
	LoadCorrupt  LoadResult = "corrupt"
)

type Store struct {
	directory string
	fileName  string
}

func NewStore(directory string) *Store {
	return &Store{directory: directory, fileName: DefaultFileName}
}
func (store *Store) Load() (Snapshot, LoadResult) {
	raw, errRead := os.ReadFile(filepath.Join(store.directory, store.fileName))
	if errors.Is(errRead, os.ErrNotExist) {
		return EmptySnapshot(), LoadMissing
	}
	if errRead != nil {
		return EmptySnapshot(), LoadCorrupt
	}
	var loaded Snapshot
	if json.Unmarshal(raw, &loaded) != nil || !validSnapshot(loaded) {
		return EmptySnapshot(), LoadCorrupt
	}
	if loaded.Version == 0 {
		loaded = migrateV0(loaded)
		return loaded, LoadMigrated
	}
	if loaded.Version != CurrentVersion {
		return EmptySnapshot(), LoadCorrupt
	}
	return normalize(loaded), LoadOK
}
func (store *Store) Save(snapshot Snapshot) error {
	snapshot = normalize(snapshot)
	if errDirectory := os.MkdirAll(store.directory, 0o700); errDirectory != nil {
		return errDirectory
	}
	encoded, errMarshal := json.Marshal(snapshot)
	if errMarshal != nil {
		return errMarshal
	}
	temporary, errCreate := os.CreateTemp(store.directory, ".state-*")
	if errCreate != nil {
		return errCreate
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if runtime.GOOS != "windows" {
		_ = temporary.Chmod(0o600)
	}
	if _, errWrite := temporary.Write(encoded); errWrite != nil {
		temporary.Close()
		return errWrite
	}
	if errSync := temporary.Sync(); errSync != nil {
		temporary.Close()
		return errSync
	}
	if errClose := temporary.Close(); errClose != nil {
		return errClose
	}
	return os.Rename(temporaryName, filepath.Join(store.directory, store.fileName))
}
func EmptySnapshot() Snapshot {
	return Snapshot{Version: CurrentVersion, Routing: RoutingSnapshot{Breaker: map[string]BreakerRecord{}, Observations: map[string]ObservationRecord{}}, Sessions: map[string]SessionRecord{}, Aliases: map[string]AliasRecord{}, Keys: map[string]KeyMetadata{}}
}
func normalize(snapshot Snapshot) Snapshot {
	if snapshot.Version == 0 {
		snapshot.Version = CurrentVersion
	}
	if snapshot.Routing.Breaker == nil {
		snapshot.Routing.Breaker = map[string]BreakerRecord{}
	}
	if snapshot.Routing.Observations == nil {
		snapshot.Routing.Observations = map[string]ObservationRecord{}
	}
	if snapshot.Sessions == nil {
		snapshot.Sessions = map[string]SessionRecord{}
	}
	if snapshot.Aliases == nil {
		snapshot.Aliases = map[string]AliasRecord{}
	}
	if snapshot.Keys == nil {
		snapshot.Keys = map[string]KeyMetadata{}
	}
	return snapshot
}
func migrateV0(snapshot Snapshot) Snapshot {
	snapshot.Version = CurrentVersion
	for key, session := range snapshot.Sessions {
		if session.Pool == "" {
			session.Pool = PoolDefault
			snapshot.Sessions[key] = session
		}
	}
	for key, alias := range snapshot.Aliases {
		if alias.Pool == "" {
			alias.Pool = PoolDefault
			snapshot.Aliases[key] = alias
		}
	}
	return normalize(snapshot)
}
func validSnapshot(snapshot Snapshot) bool {
	if snapshot.Version < 0 || snapshot.Version > CurrentVersion {
		return false
	}
	for _, metadata := range snapshot.Keys {
		if strings.Contains(strings.ToLower(metadata.KeyID), "sk-") {
			return false
		}
	}
	return true
}
