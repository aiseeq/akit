// Package price — цены моделей по API из одной таблицы (prices.tsv, вшита в пакет) с датой сверки и стоимость
// ответа по его токенам. Ёмкость подписок сюда не входит: это замеры по своим учёткам, данные потребителя, а не
// akit.
package price

import (
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

//go:embed prices.tsv
var table string

// Rate — $ за миллион токенов: вход, запись кэша на 5 минут и на час, чтение кэша, выход.
type Rate struct{ In, Write5m, Write1h, Read, Out float64 }

// Model — цены модели: обычная, для длинного запроса (вход больше LongAbove токенов) и режима Fast; nil — нет такой.
type Model struct {
	Base      Rate
	Long      *Rate
	LongAbove int64
	Fast      *Rate
}

// Usage — токены одного ответа. Write5m + Write1h, равные CacheWrite, делят запись кэша; иначе вся запись — на
// 5 минут, как считает API. Geo — маршрутизация (inference_geo): пусто, global, not_available, us.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int64
	Write5m, Write1h                     int64
	Fast                                 bool
	Geo                                  string
}

// geo — множитель маршрутизации: только США — ×1.1, глобальная и запросы без поля — обычная цена.
var geo = map[string]float64{"": 1, "global": 1, "not_available": 1, "us": 1.1}

// Geo — множитель цены за маршрутизацию (inference_geo); false — маршрутизация незнакомая.
func Geo(name string) (float64, bool) {
	g, ok := geo[name]
	return g, ok
}

var (
	// Checked — дата, когда таблицу сверяли с ценами поставщиков (ГГГГ-ММ-ДД).
	Checked string
	models  map[string]Model
)

func init() {
	var err error
	if Checked, models, err = parse(table); err != nil {
		panic(fmt.Sprintf("price: prices.tsv: %v", err))
	}
}

// modelDate — дата снимка в имени модели (claude-haiku-4-5-20251001): цена у снимка та же.
var modelDate = regexp.MustCompile(`-\d{8}$`)

// Of — цены модели; false — модели нет в таблице.
func Of(model string) (Model, bool) {
	m, ok := models[modelDate.ReplaceAllString(model, "")]
	return m, ok
}

// RateFor — цена, по которой считается ответ: Fast, длинный запрос или обычная. False — у модели нет цены Fast.
func (m Model) RateFor(u Usage) (Rate, bool) {
	switch {
	case u.Fast:
		if m.Fast == nil {
			return Rate{}, false
		}
		return *m.Fast, true
	case m.Long != nil && u.Input+u.CacheWrite+u.CacheRead > m.LongAbove:
		return *m.Long, true
	}
	return m.Base, true
}

// Of — $ за токены ответа по цене r, без множителя маршрутизации.
func (r Rate) Of(u Usage) float64 {
	w5, w1 := u.CacheWrite, int64(0)
	if u.Write5m+u.Write1h == u.CacheWrite {
		w5, w1 = u.Write5m, u.Write1h
	}
	return (float64(u.Input)*r.In + float64(w5)*r.Write5m + float64(w1)*r.Write1h + float64(u.CacheRead)*r.Read +
		float64(u.Output)*r.Out) / 1e6
}

// Cost — $ ответа по API; false — у модели, режима или маршрутизации нет цены.
func Cost(model string, u Usage) (float64, bool) {
	m, ok := Of(model)
	if !ok {
		return 0, false
	}
	g, ok := Geo(u.Geo)
	if !ok {
		return 0, false
	}
	r, ok := m.RateFor(u)
	if !ok {
		return 0, false
	}
	return r.Of(u) * g, true
}

// columns — model, tier, пять цен и long_above (только у long).
const (
	minColumns = 7
	maxColumns = 8
)

func parse(text string) (string, map[string]Model, error) {
	checked := ""
	out := map[string]Model{}
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if v, ok := strings.CutPrefix(line, "# checked: "); ok {
			checked = v
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < minColumns || len(f) > maxColumns {
			return "", nil, fmt.Errorf("line %d: %d columns", i+1, len(f))
		}
		r, err := parseRate(f[2:7])
		if err != nil {
			return "", nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		m := out[f[0]]
		switch f[1] {
		case "base":
			m.Base = r
		case "fast":
			m.Fast = &r
		case "long":
			if len(f) != maxColumns {
				return "", nil, fmt.Errorf("line %d: long needs long_above", i+1)
			}
			if m.LongAbove, err = strconv.ParseInt(f[7], 10, 64); err != nil {
				return "", nil, fmt.Errorf("line %d: long_above: %w", i+1, err)
			}
			m.Long = &r
		default:
			return "", nil, fmt.Errorf("line %d: tier %q", i+1, f[1])
		}
		out[f[0]] = m
	}
	for name, m := range out {
		if m.Base == (Rate{}) {
			return "", nil, fmt.Errorf("%s: no base price", name)
		}
	}
	if checked == "" {
		return "", nil, fmt.Errorf("no «# checked: YYYY-MM-DD» line")
	}
	return checked, out, nil
}

func parseRate(f []string) (Rate, error) {
	var v [5]float64
	for i, s := range f {
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return Rate{}, fmt.Errorf("price %q: %w", s, err)
		}
		v[i] = n
	}
	return Rate{In: v[0], Write5m: v[1], Write1h: v[2], Read: v[3], Out: v[4]}, nil
}
