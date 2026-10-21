package dock

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// 契约（windtunnel-2027 token-contract §1）：verify 两条分支都带 kind/token_id/scopes。
// opera 靠 scopes 的 null 与 [] 区分「空 scope 过渡放行」和「有 scope 但不含本模块 → 拒」，
// 所以这两个值绝不能互相塌缩。

func TestFilterScopesForModule(t *testing.T) {
	cases := []struct {
		name   string
		scopes []string
		module string
		want   []string // nil = JSON null
	}{
		{"nil stored scopes → null", nil, "opera", nil},
		{"empty stored scopes → null", []string{}, "opera", nil},
		{"scoped, nothing for module → []", []string{"hosts:write", "chat:read"}, "opera", []string{}},
		{"prefix kept, others dropped", []string{"opera:sessions:read", "hosts:write", "zone:dns:write", "opera:markers:write"}, "opera", []string{"opera:sessions:read", "opera:markers:write"}},
		{"wildcard passes", []string{"*", "hosts:write"}, "opera", []string{"*"}},
		{"module wildcard passes as prefix", []string{"opera:*"}, "opera", []string{"opera:*"}},
		{"no false prefix match", []string{"operax:foo:bar", "opera"}, "opera", []string{}},
		{"no calling module → only *", []string{"opera:sessions:read", "*"}, "", []string{"*"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterScopesForModule(tc.scopes, tc.module)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("nil-ness: got %#v want %#v", got, tc.want)
			}
			if tc.want != nil && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestAuthVerifyBody_JSONShapes(t *testing.T) {
	sess, _ := json.Marshal(authVerifyBody("u", "n", "r", "r", "opera", "", "w", "e", verifyKindSession, nil, nil))
	var m map[string]any
	_ = json.Unmarshal(sess, &m)
	if m["kind"] != "session" || m["token_id"] != nil || m["scopes"] != nil {
		t.Fatalf("session branch: %s", sess)
	}
	if _, ok := m["scopes"]; !ok {
		t.Fatalf("session branch must carry scopes key (null): %s", sess)
	}

	id := int64(42)
	unscoped, _ := json.Marshal(authVerifyBody("u", "n", "r", "r", "opera", "", "w", "e", verifyKindAPIToken, &id, filterScopesForModule(nil, "opera")))
	m = nil
	_ = json.Unmarshal(unscoped, &m)
	if m["kind"] != "api_token" || m["token_id"] != float64(42) || m["scopes"] != nil {
		t.Fatalf("unscoped api token: %s", unscoped)
	}

	scoped, _ := json.Marshal(authVerifyBody("u", "n", "r", "r", "opera", "", "w", "e", verifyKindAPIToken, &id, filterScopesForModule([]string{"hosts:write"}, "opera")))
	m = nil
	_ = json.Unmarshal(scoped, &m)
	arr, ok := m["scopes"].([]any)
	if !ok || len(arr) != 0 {
		t.Fatalf("scoped-but-not-for-module must be [] not null: %s", scoped)
	}
}

func TestVerifyCacheKeyFormats(t *testing.T) {
	s := &Server{}
	if got, want := s.verifyKey("api", "deadbeef"), "polar:verify:api:deadbeef"; got != want {
		t.Fatalf("api key: got %q want %q", got, want)
	}
	if got, want := s.verifyKey("modrole", moduleRoleCacheKey(" Opera ", " u_1 ")), "polar:verify:modrole:opera:u_1"; got != want {
		t.Fatalf("modrole key: got %q want %q", got, want)
	}
	s.redisPrefix = "zen"
	if got, want := s.apiTokenTouchKey(7), "zen:api_token_touch:7"; got != want {
		t.Fatalf("touch key: got %q want %q", got, want)
	}
}

func TestAPITokenCacheTTL(t *testing.T) {
	now := time.Now()
	if got := apiTokenCacheTTL(nil, now); got != verifyCachePosTTL {
		t.Fatalf("never-expiring: %v", got)
	}
	soon := now.Add(10 * time.Second)
	if got := apiTokenCacheTTL(&soon, now); got != 10*time.Second {
		t.Fatalf("expires in 10s: %v", got)
	}
	later := now.Add(time.Hour)
	if got := apiTokenCacheTTL(&later, now); got != verifyCachePosTTL {
		t.Fatalf("expires later: %v", got)
	}
	past := now.Add(-time.Second)
	if got := apiTokenCacheTTL(&past, now); got > 0 {
		t.Fatalf("expired must not cache: %v", got)
	}
}

// redis 为 nil 时新加的缓存 / 节流 helper 都必须是安全的空操作或走进程内兜底。
func TestAPITokenHelpers_NilRedisSafe(t *testing.T) {
	s := &Server{verifyCache: true}
	s.verifyCacheSetTTL("api", "h", apiTokenEntry{}, time.Second)
	s.verifyCacheSetTTL("api", "h", apiTokenEntry{}, 0)
	s.verifyCacheDel("modrole", moduleRoleCacheKey("opera", "u"))

	const id = int64(-987654) // not a real token id; keeps the shared sync.Map clean for others
	apiTokenTouchLocal.Delete(id)
	now := time.Now()
	if !s.shouldTouchAPIToken(id, now) {
		t.Fatal("first touch must pass")
	}
	if s.shouldTouchAPIToken(id, now.Add(10*time.Second)) {
		t.Fatal("touch within interval must be throttled")
	}
	if !s.shouldTouchAPIToken(id, now.Add(apiTokenTouchInterval+time.Second)) {
		t.Fatal("touch after interval must pass")
	}
	apiTokenTouchLocal.Delete(id)
}

func TestWriteAPITokenVerify_OwnerMissing(t *testing.T) {
	if (&Server{}).writeAPITokenVerify(nil, &apiTokenEntry{OwnerFound: false}) {
		t.Fatal("owner-less token must not verify")
	}
	if (&Server{}).writeAPITokenVerify(nil, nil) {
		t.Fatal("nil entry must not verify")
	}
}
