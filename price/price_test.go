package price

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestTable(t *testing.T) {
	if Checked != "2026-10-17" {
		t.Errorf("Checked %q", Checked)
	}
	m, ok := Of("claude-haiku-4-5-20251001")
	if !ok || m.Base.Out != 5 {
		t.Errorf("модель со снимком в имени: %+v %v", m, ok)
	}
	if _, ok := Of("glm-5"); ok {
		t.Error("модели без цены нет в таблице")
	}
	for _, bad := range []string{
		"x\tbase\t1\t1\t1\t1\t1\n",                                                     // нет даты сверки
		"# checked: 2026-01-01\nx\tlong\t1\t1\t1\t1\t1\t10\n",                          // нет base
		"# checked: 2026-01-01\nx\tbase\t1\t1\t1\tя\t1\n",                              // не число
		"# checked: 2026-01-01\nx\tslow\t1\t1\t1\t1\t1\n",                              // неизвестный tier
		"# checked: 2026-01-01\nx\tbase\t1\t1\t1\t1\t1\n" + "x\tlong\t1\t1\t1\t1\t1\n", // long без порога
	} {
		if _, _, err := parse(bad); err == nil {
			t.Errorf("принято:\n%s", bad)
		}
	}
}

func TestCost(t *testing.T) {
	u := Usage{Input: 1e6, Output: 1e6, CacheRead: 1e6, CacheWrite: 2e6, Write5m: 1e6, Write1h: 1e6}
	// opus 5.5: 4 + 20 + 0.20 + 5 + 8
	if c, ok := Cost("claude-opus-5-5", u); !ok || !near(c, 37.2) {
		t.Errorf("opus: %v %v", c, ok)
	}
	// деление записи не сходится — вся запись на 5 минут: 4 + 20 + 0.20 + 2×5
	odd := u
	odd.Write1h = 0
	if c, _ := Cost("claude-opus-5-5", odd); !near(c, 34.2) {
		t.Errorf("запись без деления: %v", c)
	}
	fast := u
	fast.Fast = true
	if c, ok := Cost("claude-opus-5-5", fast); !ok || !near(c, 74.4) {
		t.Errorf("fast: %v %v", c, ok)
	}
	if _, ok := Cost("claude-sonnet-5-5", fast); ok {
		t.Error("fast у модели без цены fast")
	}
	us := u
	us.Geo = "us"
	if c, _ := Cost("claude-opus-5-5", us); !near(c, 37.2*1.1) {
		t.Errorf("us: %v", c)
	}
	us.Geo = "mars"
	if _, ok := Cost("claude-opus-5-5", us); ok {
		t.Error("неизвестная маршрутизация")
	}
	if g, ok := Geo("us"); !ok || g != 1.1 {
		t.Errorf("Geo us: %v %v", g, ok)
	}
	if g, ok := Geo(""); !ok || g != 1 {
		t.Errorf("Geo без поля: %v %v", g, ok)
	}
	// haiku 5.5: вход 100 000 — обычная цена, на токен больше — длинная
	short := Usage{Input: 60000, CacheRead: 40000, Output: 1e6}
	if c, _ := Cost("claude-haiku-5-5", short); !near(c, (60000*0.10+40000*0.01+1e6*0.50)/1e6) {
		t.Errorf("haiku короткий: %v", c)
	}
	short.Input++
	if c, _ := Cost("claude-haiku-5-5", short); !near(c, (60001*0.50+40000*0.05+1e6*2.50)/1e6) {
		t.Errorf("haiku длинный: %v", c)
	}
}

// Terra заменяет Sol; строка Sol остаётся для старых записей журналов.
func TestTerra(t *testing.T) {
	u := Usage{Input: 1e6, Output: 1e6, CacheRead: 1e6}
	if c, ok := Cost("gpt-6.1-terra", u); !ok || !near(c, 6.05) {
		t.Errorf("terra: %v %v", c, ok)
	}
	if c, ok := Cost("gpt-6.1-sol", u); !ok || !near(c, 12.1) {
		t.Errorf("sol: %v %v", c, ok)
	}
}
