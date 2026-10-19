package provider

import (
	"testing"
	"time"
)

// timeNow 返回 UTC+8 墙钟当前时刻（与 expireLoc 口径一致，测试用）。
func timeNow() time.Time { return time.Now().In(expireLoc) }

func TestExpiringWithin(t *testing.T) {
	today := timeNow().Format("2006-01-02")
	tomorrow := timeNow().AddDate(0, 0, 1).Format("2006-01-02")
	after := timeNow().AddDate(0, 0, 3).Format("2006-01-02")

	items := []ResourceItem{
		{Name: "今日到期", Remain: 100, Usable: true, ExpireAt: today},
		{Name: "明天到期", Remain: 50, Usable: true, ExpireAt: tomorrow},
		{Name: "三天后到期", Remain: 999, Usable: true, ExpireAt: after},
		{Name: "无到期时间", Remain: 70, Usable: true},
		{Name: "不可用池临期", Remain: 300, Usable: false, ExpireAt: today},
	}
	if got := ExpiringWithin(items, 24*time.Hour); got != 150 {
		t.Fatalf("expiring=%d want 150 (今天100+明天50，三天后/无到期/不可用不计)", got)
	}
}

func TestSummarizeExpiring(t *testing.T) {
	items := []ResourceItem{
		{Remain: 100, Usable: true, ExpireAt: timeNow().Format("2006-01-02")},
		{Remain: 200, Usable: false},
	}
	u, un := Summarize(items)
	e := ExpiringWithin(items, 24*time.Hour)
	if u != 100 || un != 200 || e != 100 {
		t.Fatalf("usable=%d unusable=%d expiring=%d want 100/200/100", u, un, e)
	}
}

// TestExpireInstant 失效时刻口径：日期粒度是「次日 00:00」，RFC3339 是精确时刻。
// 这是 ledger「是否已过期」与面板「临期」共用的唯一解析入口，两种写法都要覆盖。
func TestExpireInstant(t *testing.T) {
	// 日期粒度：当天仍可用 ⇒ 失效时刻落在次日零点
	got, ok := ExpireInstant("2026-10-18")
	if !ok {
		t.Fatal("日期粒度应可解析")
	}
	want := time.Date(2026, 10, 19, 0, 0, 0, 0, expireLoc)
	if !got.Equal(want) {
		t.Fatalf("日期粒度失效时刻 = %v，want %v（次日 00:00）", got, want)
	}

	// RFC3339：精确时刻，不做「次日」平移
	got, ok = ExpireInstant("2026-10-18T15:00:00+08:00")
	if !ok {
		t.Fatal("RFC3339 应可解析")
	}
	want = time.Date(2026, 10, 18, 15, 0, 0, 0, expireLoc)
	if !got.Equal(want) {
		t.Fatalf("RFC3339 失效时刻 = %v，want %v（原样）", got, want)
	}

	// 空串 / 不可解析：ok=false，调用方按「无到期信息」处理
	for _, s := range []string{"", "  ", "not-a-date", "2026-13-45"} {
		if _, ok := ExpireInstant(s); ok {
			t.Errorf("ExpireInstant(%q) 不应可解析", s)
		}
	}
}

// TestExpiringWithinRFC3339 临期统计要能吃 RFC3339（gemini 的窗口重置时间）。
// 日期粒度的既有口径（今天/明天都算临期）由 TestExpiringWithin 锁住，这里只补新格式。
func TestExpiringWithinRFC3339(t *testing.T) {
	now := timeNow()
	items := []ResourceItem{
		{Name: "两小时后重置", Remain: 40, Usable: true, ExpireAt: now.Add(2 * time.Hour).Format(time.RFC3339)},
		{Name: "两天后重置", Remain: 900, Usable: true, ExpireAt: now.Add(48 * time.Hour).Format(time.RFC3339)},
		{Name: "不可解析", Remain: 500, Usable: true, ExpireAt: "bogus"},
	}
	if got := ExpiringWithin(items, 24*time.Hour); got != 40 {
		t.Fatalf("expiring=%d want 40（只算 24h 内的 RFC3339 条目）", got)
	}
}

// TestExpireBucketOf 守门自然日分桶（R103 核心口径）。
//
// 用户定义（2026-10-19）：按精确时刻判定，边界是自然日，
// 且**次日 00:00:00 失效算今日**——上游每日类额度就是次日零点失效，
// 归「明日」会让人以为还有一整天（实测 415 分被误报，实际只剩 5.7h）。
func TestExpireBucketOf(t *testing.T) {
	// 固定基准：2026-10-19 14:00（UTC+8）
	now := time.Date(2026, 10, 19, 14, 0, 0, 0, expireLoc)
	day := func(d, h, mi, s int) string {
		return time.Date(2026, 10, d, h, mi, s, 0, expireLoc).Format(time.RFC3339)
	}
	cases := []struct {
		name, at string
		want     int
	}{
		// —— 今日桶：失效自然日 == 今天（含次日 00:00 的零点宽限）——
		{"今天下午 16:00", day(19, 16, 0, 0), BucketToday},
		{"今晚 23:59:59", day(19, 23, 59, 59), BucketToday},
		{"次日 00:00:00（上游每日额度典型形态）", day(20, 0, 0, 0), BucketToday},
		{"次日 00:00:03（上游时钟抖动）", day(20, 0, 0, 3), BucketToday},
		{"次日 00:00:07（实测最大抖动）", day(20, 0, 0, 7), BucketToday},
		{"次日 00:04:59（宽限内）", day(20, 0, 4, 59), BucketToday},
		// —— 明日桶 ——
		{"次日 00:05:00（宽限外，真正的次日包）", day(20, 0, 5, 0), BucketTomorrow},
		{"明日中午", day(20, 12, 0, 0), BucketTomorrow},
		{"明日 23:59:59", day(20, 23, 59, 59), BucketTomorrow},
		{"后天 00:00:00（零点宽限归明日）", day(21, 0, 0, 0), BucketTomorrow},
		// —— 7 日内桶 ——
		{"后天中午", day(21, 12, 0, 0), Bucket7d},
		{"第 7 天 23:59（今天算第 1 天）", day(25, 23, 59, 59), Bucket7d},
		// 26 日 00:00 经零点宽限归 25 日（第 7 天）⇒ 仍在 7 日内；
		// 真正的越界点是 27 日 00:00（归 26 日 = 第 8 天）。
		{"第 8 天 00:00（归第 7 天，仍算 7 日内）", day(26, 0, 0, 0), Bucket7d},
		{"第 9 天 00:00（超出 7 日窗口）", day(27, 0, 0, 0), BucketNone},
		{"远期", day(30, 12, 0, 0), BucketNone},
		// —— 已失效 / 无信息 ——
		{"今天上午（已过）", day(19, 10, 0, 0), BucketNone},
		{"空串", "", BucketNone},
		{"不可解析", "bogus", BucketNone},
		// —— 纯日期粒度：该日全天可用 ⇒ **次日零点**失效 ——
		// 今天(19) 的纯日期 ⇒ 20 日 00:00 失效 ⇒ 今日桶
		{"纯日期=今天 ⇒ 今晚零点失效 ⇒ 今日", "2026-10-19", BucketToday},
		{"纯日期=明天 ⇒ 明晚零点失效 ⇒ 明日", "2026-10-20", BucketTomorrow},
	}
	for _, c := range cases {
		if got := ExpireBucketOf(c.at, now); got != c.want {
			t.Errorf("%s: ExpireBucketOf(%q) = %d，want %d", c.name, c.at, got, c.want)
		}
	}
}

// TestExpireWithinDays 守门临期阈值的自然日语义。
//
// 与旧 ExpiringWithin（时长口径）的关键差异：旧实现用「到期锚点 < now+days×24h」，
// 会把「明天 23:59 失效」在 days=1 时算成临期（不足 48h）；自然日口径下它属于明日、
// 不算今日。调度器要回答的是「今天该优先消耗谁」，故取自然日语义。
func TestExpireWithinDays(t *testing.T) {
	now := time.Date(2026, 10, 19, 14, 0, 0, 0, expireLoc)
	at := func(d, h int) string {
		return time.Date(2026, 10, d, h, 0, 0, 0, expireLoc).Format(time.RFC3339)
	}
	// days=1：只含今天（含次日零点宽限内失效的）
	for _, s := range []string{at(19, 20), at(20, 0)} {
		if !ExpireWithinDays(s, now, 1) {
			t.Errorf("%s 应在 1 个自然日内到期", s)
		}
	}
	// 明日 23:00 属明日自然日 ⇒ days=2 才含
	if !ExpireWithinDays(at(20, 23), now, 2) {
		t.Errorf("明日 23:00 应在「2 个自然日内」")
	}
	// days=1 = 只含今天；明天中午属明日自然日，不在窗口内
	// （旧时长口径 24h 会把它算进来，因为不足 48h——这正是要修的漂移）
	if ExpireWithinDays(at(20, 12), now, 1) {
		t.Errorf("明日中午不应算「1 个自然日内」（旧时长口径才会误判为临期）")
	}
	if !ExpireWithinDays(at(20, 12), now, 2) {
		t.Errorf("明日中午应在「2 个自然日内」")
	}
	if ExpireWithinDays(at(22, 12), now, 2) {
		t.Errorf("后天中午不应算「2 个自然日内」")
	}
	if ExpireWithinDays(at(19, 10), now, 3) {
		t.Errorf("已失效不应算临期")
	}
	if ExpireWithinDays("", now, 3) {
		t.Errorf("无到期信息不应算临期")
	}
}

// TestExpiringWithinDaysSum 逐条目累加（不按「每账号最早日」聚合）。
// 旧实现只取最早到期日那一天的额度，「7 日内到期」严重低估（实测 1788 vs 4938）。
func TestExpiringWithinDaysSum(t *testing.T) {
	now := time.Date(2026, 10, 19, 14, 0, 0, 0, expireLoc)
	items := []ResourceItem{
		{Name: "今晚", Remain: 100, Usable: true, ExpireAt: "2026-10-20T00:00:00+08:00"},
		{Name: "明天", Remain: 200, Usable: true, ExpireAt: "2026-10-20T23:59:59+08:00"},
		{Name: "第三天", Remain: 300, Usable: true, ExpireAt: "2026-10-21T12:00:00+08:00"},
		{Name: "远期", Remain: 900, Usable: true, ExpireAt: "2026-10-30T12:00:00+08:00"},
		{Name: "不可用", Remain: 500, Usable: false, ExpireAt: "2026-10-20T00:00:00+08:00"},
		{Name: "仅展示", Remain: 400, Usable: true, InfoOnly: true, ExpireAt: "2026-10-20T00:00:00+08:00"},
		{Name: "无到期", Remain: 800, Usable: true},
	}
	// days=1：今晚（次日零点）+ 明天 23:59 都属「明日自然日」？不——见下方断言
	// 今天算第 1 天：days=1 只含「今晚 00:00 失效」那 100 分
	if got := ExpiringWithinDays(items, 1, now); got != 100 {
		t.Errorf("days=1 应只含今日（今晚 00:00 失效）100 分，实际 %d", got)
	}
	// days=2 = 今日 + 明日（明日 23:59 失效的 200 分也在内）
	if got := ExpiringWithinDays(items, 2, now); got != 300 {
		t.Errorf("days=2 应含今日+明日 = 300 分，实际 %d", got)
	}
	// days=3 = + 第三天中午那 300 分
	if got := ExpiringWithinDays(items, 3, now); got != 600 {
		t.Errorf("days=3 应含今日+明日+第三天 = 600 分，实际 %d", got)
	}
}
