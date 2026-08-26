// Package sessions maintains scoped, expiring request affinities.
package sessions

import (
	"strings"
	"sync"
	"time"
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
}
type Binding struct {
	Scope      Scope
	Model      string
	GroupID    int
	LastUsedAt time.Time
}
type Alias struct {
	SessionKey string
	Scope      Scope
	Model      string
	GroupID    int
	LastUsedAt time.Time
}
type Affinity struct {
	mutex    sync.Mutex
	ttl      time.Duration
	bindings map[string]Binding
	aliases  map[string]Alias
}

func New(ttl time.Duration) *Affinity {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &Affinity{ttl: ttl, bindings: map[string]Binding{}, aliases: map[string]Alias{}}
}
func (affinity *Affinity) Bind(sessionKey string, scope Scope, model string, groupID int, now time.Time) {
	affinity.mutex.Lock()
	defer affinity.mutex.Unlock()
	if validScope(scope) && sessionKey != "" && groupID > 0 {
		affinity.bindings[scopedKey(sessionKey, scope)] = Binding{Scope: scope, Model: model, GroupID: groupID, LastUsedAt: now}
	}
}
func (affinity *Affinity) Resolve(sessionKey string, scope Scope, model string, now time.Time) (int, bool) {
	affinity.mutex.Lock()
	defer affinity.mutex.Unlock()
	bindingKey := scopedKey(sessionKey, scope)
	binding, exists := affinity.bindings[bindingKey]
	if !exists || affinity.expired(binding.LastUsedAt, now) || binding.Scope != scope || binding.Model != model {
		if exists && affinity.expired(binding.LastUsedAt, now) {
			delete(affinity.bindings, bindingKey)
		}
		return 0, false
	}
	binding.LastUsedAt = now
	affinity.bindings[bindingKey] = binding
	return binding.GroupID, true
}
func (affinity *Affinity) Invalidate(sessionKey string, scope Scope, groupID int) bool {
	affinity.mutex.Lock()
	defer affinity.mutex.Unlock()
	bindingKey := scopedKey(sessionKey, scope)
	binding, exists := affinity.bindings[bindingKey]
	if !exists || binding.Scope != scope || binding.GroupID != groupID {
		return false
	}
	delete(affinity.bindings, bindingKey)
	for aliasKey, alias := range affinity.aliases {
		if alias.SessionKey == sessionKey && alias.Scope == scope && alias.GroupID == groupID {
			delete(affinity.aliases, aliasKey)
		}
	}
	return true
}
func (affinity *Affinity) BindAlias(aliasKey, sessionKey string, scope Scope, model string, groupID int, now time.Time) {
	affinity.mutex.Lock()
	defer affinity.mutex.Unlock()
	if validScope(scope) && aliasKey != "" && sessionKey != "" && groupID > 0 {
		affinity.aliases[scopedKey(aliasKey, scope)] = Alias{SessionKey: sessionKey, Scope: scope, Model: model, GroupID: groupID, LastUsedAt: now}
	}
}
func (affinity *Affinity) ResolveAlias(aliasKey string, scope Scope, model string, now time.Time) (Alias, bool) {
	affinity.mutex.Lock()
	defer affinity.mutex.Unlock()
	storedAliasKey := scopedKey(aliasKey, scope)
	alias, exists := affinity.aliases[storedAliasKey]
	if !exists || affinity.expired(alias.LastUsedAt, now) || alias.Scope != scope || alias.Model != model {
		if exists && affinity.expired(alias.LastUsedAt, now) {
			delete(affinity.aliases, storedAliasKey)
		}
		return Alias{}, false
	}
	alias.LastUsedAt = now
	affinity.aliases[storedAliasKey] = alias
	return alias, true
}

// ClearBindings discards only in-memory session bindings.
func (affinity *Affinity) ClearBindings() int {
	affinity.mutex.Lock()
	defer affinity.mutex.Unlock()
	count := len(affinity.bindings)
	affinity.bindings = map[string]Binding{}
	return count
}

// ClearAliases discards only in-memory Responses aliases.
func (affinity *Affinity) ClearAliases() int {
	affinity.mutex.Lock()
	defer affinity.mutex.Unlock()
	count := len(affinity.aliases)
	affinity.aliases = map[string]Alias{}
	return count
}

// Counts returns aggregate values that do not expose session identifiers.
func (affinity *Affinity) Counts() (bindings, aliases int) {
	affinity.mutex.Lock()
	defer affinity.mutex.Unlock()
	return len(affinity.bindings), len(affinity.aliases)
}

// Snapshot returns copies of non-secret affinity records for durable state.
func (affinity *Affinity) Snapshot() (map[string]Binding, map[string]Alias) {
	affinity.mutex.Lock()
	defer affinity.mutex.Unlock()
	bindings := make(map[string]Binding, len(affinity.bindings))
	aliases := make(map[string]Alias, len(affinity.aliases))
	for key, value := range affinity.bindings {
		bindings[key] = value
	}
	for key, value := range affinity.aliases {
		aliases[key] = value
	}
	return bindings, aliases
}

// Restore replaces affinity records loaded from a validated durable snapshot.
func (affinity *Affinity) Restore(bindings map[string]Binding, aliases map[string]Alias) {
	affinity.mutex.Lock()
	defer affinity.mutex.Unlock()
	affinity.bindings, affinity.aliases = map[string]Binding{}, map[string]Alias{}
	for key, value := range bindings {
		if validScope(value.Scope) && value.GroupID > 0 {
			affinity.bindings[key] = value
		}
	}
	for key, value := range aliases {
		if validScope(value.Scope) && value.GroupID > 0 && value.SessionKey != "" {
			affinity.aliases[key] = value
		}
	}
}

// CanFallback permits a replacement route only before response output begins.
func CanFallback(_ bool, outputStarted bool) bool { return !outputStarted }
func (affinity *Affinity) expired(lastUsedAt, now time.Time) bool {
	return now.Sub(lastUsedAt) > affinity.ttl
}
func validScope(scope Scope) bool {
	return scope.AccountID != "" && scope.Plan != "" && (scope.Pool == DefaultPool || scope.Pool == LunaPool)
}

// scopedKey prevents an identical non-secret hash from overwriting affinity
// belonging to another account, plan, or default/Luna namespace.
func scopedKey(value string, scope Scope) string {
	return strings.Join([]string{scope.AccountID, scope.Plan, string(scope.Pool), value}, "\x00")
}
