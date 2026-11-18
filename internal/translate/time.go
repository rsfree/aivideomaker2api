package translate

import "time"

// isoLayouts 是站点时间字段实际出现过的形态（createdAt / completedAt 都是 ISO8601，
// 有时带毫秒、有时带 Z、有时是纯日期）。按顺序尝试，全不匹配就返回错误 ——
// 调用方据此如实给 nil，绝不按"现在"补一个看起来合理的值。
var isoLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseISO(s string) (time.Time, error) {
	var lastErr error
	for _, layout := range isoLayouts {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	// 站点也会回 "…Z" 但不带时区偏移的变体，RFC3339 需要偏移 ⇒ 兜一次。
	if len(s) > 1 && s[len(s)-1] == 'Z' {
		if t, err := time.Parse("2006-01-02T15:04:05.999999999Z", s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, lastErr
}
