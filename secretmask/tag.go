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

// placeholderSep follows the kind in a marker written while the passes run
// (a private-use rune ordinary text does not hold), then the value's number;
// keptSep, then the number of a marker already drawn in the input.
const (
	placeholderSep = "\uE000"
	keptSep        = "\uE001"
)

var (
	placeholderRE = regexp.MustCompile(`\[скрыто:([^\]\x{E000}\x{E001}]{1,40})([\x{E000}\x{E001}])([0-9]+)\]`)
	// renderedRE is a marker Tagger.Mask has drawn: its spaces would let the
	// context rules read it as a value, so a second pass keeps it whole.
	renderedRE = regexp.MustCompile(`\[скрыто:([^\]\s\x{E000}\x{E001}]{1,40}) len=[0-9]+ #[0-9a-f]+\]`)
)

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
// the length and tag of the value it hides, counts by kind as Mask does.
func (t *Tagger) Mask(text string) (string, map[string]int) {
	m := masking{found: map[string]int{}, tagger: t}
	return m.mask(m.keepRendered(text)), m.found
}

// keepRendered puts a placeholder of the plain marker's form in place of each
// marker already drawn, so masking drawn text again leaves it as it is.
func (m *masking) keepRendered(text string) string {
	if !strings.Contains(text, " #") {
		return text
	}
	return renderedRE.ReplaceAllStringFunc(text, func(marker string) string {
		kind := renderedRE.FindStringSubmatch(marker)[1]
		m.kept = append(m.kept, marker)
		return maskMarker + kind + keptSep + strconv.Itoa(len(m.kept)-1) + "]"
	})
}

// Tag is the tag a marker shows for value.
func (t *Tagger) Tag(value string) string {
	h := hmac.New(sha256.New, t.key)
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil)[:tagBytes])
}

// original is value with the placeholders of earlier passes put back, so a
// span that swallowed a marker is measured and tagged by what it hid.
func (m *masking) original(value string) string {
	if !strings.Contains(value, placeholderSep) && !strings.Contains(value, keptSep) {
		return value
	}
	return placeholderRE.ReplaceAllStringFunc(value, func(marker string) string {
		if v, ok := m.placeholderValue(marker); ok {
			return v
		}
		return marker
	})
}

// render writes each placeholder as the final marker with length and tag, and
// a kept marker as it was.
func (m *masking) render(text string) string {
	if !strings.Contains(text, placeholderSep) && !strings.Contains(text, keptSep) {
		return text
	}
	return placeholderRE.ReplaceAllStringFunc(text, func(marker string) string {
		sub := placeholderRE.FindStringSubmatch(marker)
		index, err := strconv.Atoi(sub[3])
		switch {
		case err != nil:
			return marker
		case sub[2] == keptSep && index < len(m.kept):
			return m.kept[index]
		case sub[2] == placeholderSep && index < len(m.values):
			v := m.values[index]
			return fmt.Sprintf("%s%s len=%d #%s]", maskMarker, sub[1], utf8.RuneCountInString(v), m.tagger.Tag(v))
		}
		return marker
	})
}

// placeholderValue is what a placeholder stands for: the hidden value, or the
// drawn marker it keeps.
func (m *masking) placeholderValue(marker string) (string, bool) {
	sub := placeholderRE.FindStringSubmatch(marker)
	index, err := strconv.Atoi(sub[3])
	switch {
	case err != nil:
		return "", false
	case sub[2] == keptSep && index < len(m.kept):
		return m.kept[index], true
	case sub[2] == placeholderSep && index < len(m.values):
		return m.values[index], true
	}
	return "", false
}

// isPlaceholder reports whether s is one whole placeholder.
func (m *masking) isPlaceholder(s string) bool {
	if !strings.HasPrefix(s, maskMarker) || !strings.HasSuffix(s, "]") {
		return false
	}
	loc := placeholderRE.FindStringIndex(s)
	return loc != nil && loc[0] == 0 && loc[1] == len(s)
}
