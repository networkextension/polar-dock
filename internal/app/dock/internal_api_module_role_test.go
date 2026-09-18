package dock

import (
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
)

// 这一组钉的是「两条 verify 分支对模块角色的处理必须一样」。
// 曾经只有浏览器会话那条查了 module_user_roles，polar_ API token 那条直接回
// users.role —— 于是同一个人在浏览器里是 opera 管理员、拿 token 调就是普通
// user，插件只读 `role`，就按自己的默认角色兜底，观众域整个 403。
// 仓里的测试不连 DB，所以这里测的是两条分支共用的那几个纯函数。

func TestModuleRoleOverride_GrantWins(t *testing.T) {
	role, moduleRole := moduleRoleOverride("user", "admin")
	if role != "admin" {
		t.Errorf("role = %q, want admin —— 模块授权必须盖过 users.role", role)
	}
	if moduleRole != "admin" {
		t.Errorf("module_role = %q, want admin", moduleRole)
	}
}

func TestModuleRoleOverride_NoGrantKeepsPlatformRole(t *testing.T) {
	role, moduleRole := moduleRoleOverride("admin", "")
	if role != "admin" {
		t.Errorf("role = %q, want admin —— 没有授权的用户行为应与加模块角色之前完全一致", role)
	}
	if moduleRole != "" {
		t.Errorf("module_role = %q, want 空", moduleRole)
	}
}

// 没有插件名（路由没走 hmacPluginAuth）时不查库：Server.db 是 nil，真去查就 panic。
func TestLookupModuleRole_NoPluginDoesNotTouchDB(t *testing.T) {
	if got := (&Server{}).lookupModuleRole("", "u_abc"); got != "" {
		t.Errorf("lookupModuleRole(\"\", …) = %q, want 空", got)
	}
}

func TestCallingPlugin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if got := callingPlugin(c); got != "" {
		t.Errorf("未设置 plugin_name 时 = %q, want 空", got)
	}
	c.Set("plugin_name", "opera")
	if got := callingPlugin(c); got != "opera" {
		t.Errorf("= %q, want opera", got)
	}
}

// 两条分支共用这一个构造函数，字段集就不会再各长各的。
// 契约见 doc/arch/internal-api-v1.md §1；插件缓存 30s 后只读 `role`。
func TestAuthVerifyBody_FieldSet(t *testing.T) {
	body := authVerifyBody("u_abc", "Kong", "admin", "user", "opera", "admin", "t_root", "2026-09-18T00:00:00Z")
	got := make([]string, 0, len(body))
	for k := range body {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"expires_at", "module", "module_role", "platform_role", "role", "username", "user_id", "workspace_id"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("字段集 = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("字段集 = %v, want %v", got, want)
		}
	}
	if body["role"] != "admin" || body["platform_role"] != "user" {
		t.Errorf("role/platform_role 接反了: %v / %v", body["role"], body["platform_role"])
	}
}
