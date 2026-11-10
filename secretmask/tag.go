package secretmask

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MinTagKeyLen is the shortest key NewTagger accepts.
const MinTagKeyLen = 16

// tagBytes is how much of the keyed hash a marker shows: enough to tell two
// values apart in one reading, too little to stand for the value.
const tagBytes = 3

// renderedRE is a marker Tagger.Mask has drawn: its spaces would let the
// context rules read it as a value, so a second pass keeps it whole. Kinds are
// lowercase words, so a key standing where the kind goes is no drawn marker.
var renderedRE = regexp.MustCompile(`\[скрыто:[a-z_-]{1,20} len=[0-9]+ #[0-9a-f]{1,16}\]`)

// placeholders are the markers written while the passes run: [скрыто:<kind>
// value-separator <number>] for a value hidden in this call and [скрыто:
// kept-separator <number>] for a drawn marker kept from the input. The
// separators are private-use runes the input does not hold, so text in the
// input never reads as a placeholder.
type placeholders struct {
	value, kept string
	re          *regexp.Regexp
}

var defaultPlaceholders = newPlaceholders('', '')

func newPlaceholders(value, kept rune) *placeholders {
	v, k := regexp.QuoteMeta(string(value)), regexp.QuoteMeta(string(kept))
	return &placeholders{
		value: string(value), kept: string(kept),
		re: regexp.MustCompile(`\[скрыто:([a-z_-]{0,20})(` + v + `|` + k + `)([0-9]+)\]`),
	}
}

// placeholdersFor picks separators text does not hold; nil when it holds every
// private-use rune of the basic plane but one.
func placeholdersFor(text string) *placeholders {
	if !strings.ContainsRune(text, '') && !strings.ContainsRune(text, '') {
		return defaultPlaceholders
	}
	var free []rune
	for r := ''; r <= '' && len(free) < 2; r++ {
		if !strings.ContainsRune(text, r) {
			free = append(free, r)
		}
	}
	if len(free) < 2 {
		return nil
	}
	return newPlaceholders(free[0], free[1])
}

// Tagger masks like Mask and tells what each hidden value was like without
// showing it: [скрыто:<kind> len=<characters> #<tag>]. The tag is a keyed hash
// of the value, so equal values get equal tags under one key, while without the
// key a tag cannot be checked against a guess, even of a short password.
type Tagger struct {
	key []byte
}

// NewTagger returns a Tagger for key, which must hold at least MinTagKeyLen
// bytes; the caller keeps it private and stable to compare across readings.
func NewTagger(key []byte) (*Tagger, error) {
	if len(key) < MinTagKeyLen {
		return nil, fmt.Errorf("secretmask: tag key of %d bytes, need at least %d", len(key), MinTagKeyLen)
	}
	return &Tagger{key: append([]byte(nil), key...)}, nil
}

// Mask hides secrets in text like the package Mask and writes each marker with
// the length and tag of the value it hides, counts by kind as Mask does. Text
// that holds every private-use rune leaves no separator free: it is masked as
// Mask does, without length and tag.
func (t *Tagger) Mask(text string) (string, map[string]int) {
	p := placeholdersFor(text)
	if p == nil {
		return Mask(text)
	}
	m := masking{found: map[string]int{}, tagger: t, ph: p}
	return m.mask(m.keepRendered(text)), m.found
}

// keepRendered puts a placeholder in place of each marker already drawn, so
// masking drawn text again leaves it as it is.
func (m *masking) keepRendered(text string) string {
	if !strings.Contains(text, " #") {
		return text
	}
	return renderedRE.ReplaceAllStringFunc(text, func(marker string) string {
		m.kept = append(m.kept, marker)
		// The placeholder takes the marker's length, so the rules that look
		// for a nearby marker see text laid out as Mask sees it.
		digits := len(marker) - len(maskMarker) - len(m.ph.kept) - len("]")
		return fmt.Sprintf("%s%s%0*d]", maskMarker, m.ph.kept, digits, len(m.kept)-1)
	})
}

// Tag is the tag a marker shows for value.
func (t *Tagger) Tag(value string) string {
	h := hmac.New(sha256.New, t.key)
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil)[:tagBytes])
}

// holdsPlaceholder reports whether s may hold a placeholder of this call.
func (m *masking) holdsPlaceholder(s string) bool {
	return strings.Contains(s, m.ph.value) || strings.Contains(s, m.ph.kept)
}

// original is value with the placeholders of earlier passes put back, so a
// span that swallowed a marker is measured and tagged by what it hid.
func (m *masking) original(value string) string {
	if !m.holdsPlaceholder(value) {
		return value
	}
	return m.ph.re.ReplaceAllStringFunc(value, func(marker string) string {
		if v, _, ok := m.placeholderValue(marker); ok {
			return v
		}
		return marker
	})
}

// render writes each placeholder as the final marker with length and tag, and
// a kept marker as it was.
func (m *masking) render(text string) string {
	if !m.holdsPlaceholder(text) {
		return text
	}
	return m.ph.re.ReplaceAllStringFunc(text, func(marker string) string {
		v, kind, ok := m.placeholderValue(marker)
		switch {
		case !ok:
			return marker
		case kind == "":
			return v
		}
		return fmt.Sprintf("%s%s len=%d #%s]", maskMarker, kind, utf8.RuneCountInString(v), m.tagger.Tag(v))
	})
}

// placeholderValue is what a placeholder stands for: the hidden value and its
// kind, or the drawn marker it keeps and an empty kind.
func (m *masking) placeholderValue(marker string) (string, string, bool) {
	sub := m.ph.re.FindStringSubmatch(marker)
	index, err := strconv.Atoi(sub[3])
	switch {
	case err != nil:
		return "", "", false
	case sub[2] == m.ph.kept && sub[1] == "" && index < len(m.kept):
		return m.kept[index], "", true
	case sub[2] == m.ph.value && sub[1] != "" && index < len(m.values):
		return m.values[index], sub[1], true
	}
	return "", "", false
}

// isPlaceholder reports whether s is one whole placeholder.
func (m *masking) isPlaceholder(s string) bool {
	if !strings.HasPrefix(s, maskMarker) || !strings.HasSuffix(s, "]") {
		return false
	}
	loc := m.ph.re.FindStringIndex(s)
	return loc != nil && loc[0] == 0 && loc[1] == len(s)
}
