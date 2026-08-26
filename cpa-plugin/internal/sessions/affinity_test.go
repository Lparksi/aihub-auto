package sessions

import (
	"testing"
	"time"
)

func TestAffinityHonorsScopePoolModelTTLAndInvalidation(t *testing.T) {
	affinity := New(time.Minute)
	scope := Scope{AccountID: "account", Plan: "pro", Pool: DefaultPool}
	affinity.Bind("hashed-session", scope, "gpt-4", 7, time.Unix(0, 0))
	if groupID, ok := affinity.Resolve("hashed-session", scope, "gpt-4", time.Unix(30, 0)); !ok || groupID != 7 {
		t.Fatalf("Resolve matching = %d, %t; want group 7", groupID, ok)
	}
	if _, ok := affinity.Resolve("hashed-session", Scope{AccountID: "account", Plan: "team", Pool: DefaultPool}, "gpt-4", time.Unix(30, 0)); ok {
		t.Fatal("Resolve crossed plan boundary")
	}
	if _, ok := affinity.Resolve("hashed-session", Scope{AccountID: "account", Plan: "pro", Pool: LunaPool}, "gpt-4", time.Unix(30, 0)); ok {
		t.Fatal("Resolve crossed default/Luna boundary")
	}
	if _, ok := affinity.Resolve("hashed-session", scope, "claude", time.Unix(30, 0)); ok {
		t.Fatal("Resolve accepted incompatible model")
	}
	if _, ok := affinity.Resolve("hashed-session", scope, "gpt-4", time.Unix(91, 0)); ok {
		t.Fatal("Resolve retained expired session")
	}
	affinity.Bind("hashed-session", scope, "gpt-4", 7, time.Unix(62, 0))
	if !affinity.Invalidate("hashed-session", scope, 7) {
		t.Fatal("Invalidate() = false, want matching binding removed")
	}
}

func TestResponseAliasesExpireAndFallbackOnlyOccursBeforeOutput(t *testing.T) {
	affinity := New(time.Minute)
	scope := Scope{AccountID: "account", Plan: "pro", Pool: DefaultPool}
	affinity.Bind("hashed-session", scope, "gpt-4", 7, time.Unix(0, 0))
	affinity.BindAlias("hashed-response", "hashed-session", scope, "gpt-4", 7, time.Unix(0, 0))
	alias, ok := affinity.ResolveAlias("hashed-response", scope, "gpt-4", time.Unix(30, 0))
	if !ok || alias.SessionKey != "hashed-session" || alias.GroupID != 7 {
		t.Fatalf("ResolveAlias() = %#v, %t", alias, ok)
	}
	if _, ok := affinity.ResolveAlias("hashed-response", scope, "gpt-4", time.Unix(91, 0)); ok {
		t.Fatal("ResolveAlias retained expired alias")
	}
	if !CanFallback(false, false) || !CanFallback(true, false) || CanFallback(true, true) {
		t.Fatal("fallback decision must permit only pre-output retries")
	}
}
