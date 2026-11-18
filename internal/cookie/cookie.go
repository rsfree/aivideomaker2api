// Package cookie 把"人实际会粘贴的东西"规范成真正的 Cookie 请求头。
//
// 为什么需要它：会话凭据是**逐字**当 `Cookie` 头发出去的 —— 它是一个请求头，
// **不是一个 token**。格式不对时不会报错，服务端只是把你当成未登录，而这与
// 「会话已过期」表现完全一致 ⇒ 排查时会去追一个根本没到期的 TTL。
//
// 接受的实际形态（每一条都来自真实粘贴）：
//
//	'abc…'                            （裸 token）      -> 'auth_session=abc…' + 告警
//	'auth_session=abc…'                                 -> 原样
//	'auth_session=abc…;NEXT_LOCALE=zh'                  -> 原样（分隔符规范成 "; "）
//	'Cookie: auth_session=abc…; NEXT_LOCALE=zh'         -> 剥掉标签
//	"-H 'Cookie: auth_session=abc…'"                    -> 剥掉 cURL 包装
//	'Set-Cookie: auth_session=abc…; Path=/; HttpOnly'   -> 丢掉属性
//	'[{"name":"auth_session","value":"abc…"}]'          -> jar JSON 拼成头
//
// 认不出来的一律**原样返回**，让服务端（而不是本函数）给出错误。绝不猜。
package cookie

import (
	"encoding/json"
	"regexp"
	"strings"
)

// AuthCookieName 站点会话 cookie 的名字。
const AuthCookieName = "auth_session"

var (
	bareToken   = regexp.MustCompile(`^[A-Za-z0-9._-]{16,}$`)
	shellHeader = regexp.MustCompile(`(?i)^(?:-H|--header)\s+`)
	shellCookie = regexp.MustCompile(`(?i)^(?:--cookie|-b)\s+`)
	label       = regexp.MustCompile(`(?i)^(?:set-)?cookie\s*:\s*`)
)

// setCookieAttrs 是 Set-Cookie 的**属性**名：在 Cookie 请求头里永远非法，
// 粘贴整段响应头时必须丢掉。（HttpOnly / Secure / Partitioned 没有 `=`，
// 会按"空段"被丢弃。）
var setCookieAttrs = map[string]bool{
	"path": true, "domain": true, "expires": true, "max-age": true,
	"samesite": true, "priority": true, "partitioned": true,
	"comment": true, "version": true,
}

// Normalize 把用户实际粘贴的内容规范成 Cookie 头值。
// 第二个返回值是**修过**的时候的说明（调用方负责打日志/告警）；
// 没有改写时为 nil。
func Normalize(raw string) (string, []string) {
	var notes []string
	s := stripQuotes(raw)

	// 整份导出的 cookie jar，以 JSON 形式粘贴。
	if strings.HasPrefix(s, "[") {
		var jar []map[string]any
		if err := json.Unmarshal([]byte(s), &jar); err == nil {
			pairs := make([]string, 0, len(jar))
			for _, c := range jar {
				name, _ := c["name"].(string)
				value, _ := c["value"].(string)
				if name != "" && value != "" {
					pairs = append(pairs, name+"="+value)
				}
			}
			if len(pairs) > 0 {
				s = strings.Join(pairs, "; ")
			}
		}
	}

	// 逐层剥 shell / header 噪声。用循环是因为层会嵌套：
	// `-H 'Cookie: …'` 需要两轮才剥得干净。有界，且每轮都在变短。
	for i := 0; i < 5; i++ {
		before := s
		s = stripQuotes(s)
		s = shellHeader.ReplaceAllString(s, "")
		s = shellCookie.ReplaceAllString(s, "")
		s = label.ReplaceAllString(s, "")
		s = stripQuotes(s)
		if s == before {
			break
		}
	}
	// 刻意**不**做全局空白折叠：多行粘贴（每行以 `;` 结尾）靠逐段 strip 就能正确解析；
	// 而"换行但缺 `;`"是真正的畸形输入 —— 那种必须原样下发、让 HTTP 层明确报错，
	// 而不是被折叠成"看似合法的一对"而静默发错。

	if strings.Contains(s, "=") {
		parts := make([]string, 0, 8)
		for _, seg := range strings.Split(s, ";") {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				continue
			}
			name, _, found := strings.Cut(seg, "=")
			if !found { // 光秃秃的 HttpOnly / Secure
				continue
			}
			if setCookieAttrs[strings.ToLower(strings.TrimSpace(name))] {
				continue
			}
			parts = append(parts, seg)
		}
		out := strings.Join(parts, "; ")
		if out != raw {
			notes = append(notes, "cookie header was normalized (separators / response-only attributes cleaned up)")
		}
		return out, notes
	}

	// 一个 `=` 都没有 —— 要么是裸 token，要么是我们拒绝猜的东西。
	if bareToken.MatchString(s) {
		notes = append(notes, "credential looked like a bare token, not a Cookie header; prefixed it with "+
			AuthCookieName+"= (prefer pasting the full `auth_session=...` form)")
		return AuthCookieName + "=" + s, notes
	}
	return s, notes
}

// HasAuthSession 判断规范化后的头里是否真的带上了会话 cookie。
func HasAuthSession(header string) bool {
	for _, seg := range strings.Split(header, ";") {
		name, _, _ := strings.Cut(strings.TrimSpace(seg), "=")
		if strings.EqualFold(strings.TrimSpace(name), AuthCookieName) {
			return true
		}
	}
	return false
}

// Mask 把凭据压成日志里可安全出现的形态（前 6 + 后 4）。空串恒为空。
func Mask(header string) string {
	if header == "" {
		return ""
	}
	if len(header) <= 12 {
		return "***"
	}
	return header[:6] + "…" + header[len(header)-4:]
}

func stripQuotes(value string) string {
	t := strings.TrimSpace(value)
	if len(t) >= 2 {
		q := t[0]
		if (q == '\'' || q == '"' || q == '`') && t[len(t)-1] == q {
			return strings.TrimSpace(t[1 : len(t)-1])
		}
	}
	return t
}
