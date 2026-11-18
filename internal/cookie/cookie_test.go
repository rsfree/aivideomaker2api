package cookie

import "testing"

func TestNormalizeAcceptsRealPastes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare token", "abcdef0123456789abcdef", "auth_session=abcdef0123456789abcdef"},
		{"already a header", "auth_session=abc", "auth_session=abc"},
		{"two cookies", "auth_session=abc;NEXT_LOCALE=zh", "auth_session=abc; NEXT_LOCALE=zh"},
		{"cookie label", "Cookie: auth_session=abc; NEXT_LOCALE=zh", "auth_session=abc; NEXT_LOCALE=zh"},
		{"curl wrapper", "-H 'Cookie: auth_session=abc'", "auth_session=abc"},
		{"curl long flag", `--cookie "auth_session=abc; NEXT_LOCALE=zh"`, "auth_session=abc; NEXT_LOCALE=zh"},
		{"set-cookie attributes", "Set-Cookie: auth_session=abc; Path=/; HttpOnly; Max-Age=100", "auth_session=abc"},
		{"jar json", `[{"name":"auth_session","value":"abc"},{"name":"NEXT_LOCALE","value":"zh"}]`,
			"auth_session=abc; NEXT_LOCALE=zh"},
	}
	for _, c := range cases {
		got, _ := Normalize(c.in)
		if got != c.want {
			t.Errorf("%s: Normalize(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestNormalizeWarnsOnBareTokenOnly(t *testing.T) {
	if _, notes := Normalize("abcdef0123456789abcdef"); len(notes) == 0 {
		t.Error("bare token must be reported (it is a silent-failure shape otherwise)")
	}
	if _, notes := Normalize("auth_session=abc"); len(notes) != 0 {
		t.Errorf("already-correct header must not warn, got %v", notes)
	}
}

func TestNormalizeDoesNotFoldMalformedInput(t *testing.T) {
	// 换行但缺 `;` 是畸形输入：必须原样下发，让 HTTP 层明确报错，
	// 而不是被折叠成"看似合法的一对"而静默发错。
	in := "auth_session=abc\nNEXT_LOCALE=zh"
	got, _ := Normalize(in)
	if got != in {
		t.Errorf("malformed input must pass through unchanged, got %q", got)
	}
}

func TestHasAuthSession(t *testing.T) {
	if !HasAuthSession("NEXT_LOCALE=zh; auth_session=abc") {
		t.Error("must find auth_session among other cookies")
	}
	if HasAuthSession("NEXT_LOCALE=zh") {
		t.Error("must not claim a session when auth_session is absent")
	}
}
