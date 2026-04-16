package k8s

import (
	"testing"

	"github.com/lexfrei/cozytempl/internal/auth"
)

// TestSchemaCacheScopedPerUser pins the cross-user leak fix
// in the cache key. An earlier revision keyed the cache on
// `kind` alone, so user A's read populated the bucket and
// user B (with different RBAC) received A's cached schema on
// the next lookup. The cache key is now (user, kind); two
// users' entries coexist without stepping on each other.
// Test drives both sides of the flip: same kind across two
// users returns their own cached entry, and distinct entries
// do not merge or overwrite each other.
func TestSchemaCacheScopedPerUser(t *testing.T) {
	t.Parallel()

	svc := &SchemaService{cache: make(map[schemaCacheKey]schemaCacheEntry)}

	alice := &auth.UserContext{Username: "alice"}
	bob := &auth.UserContext{Username: "bob"}

	aliceSchema := &AppSchema{Kind: "Postgres", Description: "alice-visible"}
	bobSchema := &AppSchema{Kind: "Postgres", Description: "bob-visible"}

	svc.cacheSet(schemaCacheKey{user: cacheUserKey(alice), kind: "Postgres"}, aliceSchema)
	svc.cacheSet(schemaCacheKey{user: cacheUserKey(bob), kind: "Postgres"}, bobSchema)

	aliceEntry, ok := svc.cache[schemaCacheKey{user: "alice", kind: "Postgres"}]
	if !ok || aliceEntry.schema.Description != "alice-visible" {
		t.Errorf("alice's entry lost or corrupted: %+v", aliceEntry)
	}

	bobEntry, ok := svc.cache[schemaCacheKey{user: "bob", kind: "Postgres"}]
	if !ok || bobEntry.schema.Description != "bob-visible" {
		t.Errorf("bob's entry lost or corrupted: %+v", bobEntry)
	}

	// The two entries must NOT be the same pointer — a
	// regression that flattened the key would silently alias
	// the two users' entries.
	if aliceEntry.schema == bobEntry.schema {
		t.Error("alice and bob share the same cache entry; per-user scoping broken")
	}
}

// TestCacheUserKeyAnonymousFallback documents the nil-user
// behaviour. Tests and bootstrap-only code paths sometimes
// call SchemaService with a nil UserContext; returning the
// same bucket would collide across fixtures, but returning
// the empty string would also collide with a real user named
// "". "anonymous" is the explicit sentinel.
func TestCacheUserKeyAnonymousFallback(t *testing.T) {
	t.Parallel()

	if got := cacheUserKey(nil); got != anonymousCacheUser {
		t.Errorf("cacheUserKey(nil) = %q, want %q", got, anonymousCacheUser)
	}

	if got := cacheUserKey(&auth.UserContext{Username: ""}); got != "" {
		t.Errorf("cacheUserKey(empty user) = %q, want empty (callers must set Username)", got)
	}
}
