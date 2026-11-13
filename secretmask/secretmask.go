// Package secretmask hides secret values in text before it goes further: to a
// log, a report or a model of another provider. It replaces each match with
// [скрыто:<kind>] and counts matches by kind; it never returns a value. A
// Tagger writes [скрыто:<kind> len=<characters> #<tag>] instead, the tag being
// a hash of the value under the caller's key.
//
// Detection has deliberate limits:
//   - Short fragments after a bare letter or digit, and generic sk values
//     without digits, can leak.
//   - A random body holds no digit by chance: over 62 characters 1.47% at length
//     24, 0.21% at 35, 0.18% at 36; AIza bodies, over 64, 0.26% at 35. Such a key
//     of the sk_live, gh?_ or AIza kinds is hidden only where it stands on its
//     own; next to a letter, a digit, - or _ it leaks. Other kinds always need a
//     digit: their bodies can hold phrases, and the shortest forms accepted here
//     can leak without one (github_pat_ 0.56%, sk-proj- and sk-ant- 3.3%, xox?-
//     17.8% at the minimum length), real keys of these kinds being much longer.
//   - After a key of the sk_live kinds the token characters are dropped from the
//     output, so text glued to it with _ or - goes with it. The key itself is
//     recognized by the body before the first separator: with _ or - before the
//     16th body character, or no digit before it next to a joiner, rk_ and pk_
//     keys stay visible (sk_ ones fall to generic sk).
//   - Base64url under generic sk can split into short fragments: at length 32,
//     0.47% after a space and 28.34% after a letter.
//   - Escapes outside escapeRE grammar can leak, including byte 0x9b.
//   - rk_, pk_ and sk_ names with test/live can be corrupted when a fragment
//     is at least 16 characters and contains a digit (see
//     network_test_HandlesUtf8Input).
//   - A name of the sk_live, gh?_ or AIza form standing on its own is corrupted
//     when its body is long enough, with or without a digit: one long word
//     (pk_test_ConnectionTimeoutCase), also before a dot (…Case.Field), and for
//     AIza a phrase joined by - or _. A leaked key costs more than these names,
//     which ordinary text rarely has.
//   - At a left boundary, digit-bearing names can be corrupted, including
//     sk-learn-contrib-imbalanced-2024-01, sk_buff_data_end_offset_2_bytes,
//     --sk-color-primary-500-hover-dark and xoxo-hugs-and-kisses-2026.
//     A hyphen or underscore neighbor can expose the same false positive, as
//     in task-sk-learn-contrib-imbalanced-2024-01.
//   - Command context detection misses some tool-specific options, non-English
//     key names, and references such as $ecret. An unquoted value can end at a
//     quote and leave a tail visible (for example PASSWORD=a'b').
//   - Tools are known by name: sshpass -p, mysql/mariadb -p, redis-cli -a,
//     mongo -p, docker login -p, htpasswd -b, gh --with-token <<<, curl -u,
//     --oauth2-bearer, -b and a Cookie header in -H, and echo piped right into
//     sudo -S (printf and other writers are not). A Cookie header outside curl
//     and a bare Bearer value are not hidden.
//   - A URL user without a password is taken for a token only when it is at
//     least 8 letters, digits, _ or - with both a letter and a digit: git@ and
//     user@ stay, deploy2024@ is hidden.
//   - JSON rules read plain JSON, not JSON written inside a JSON string: an
//     option and its value as neighbouring array elements, an array under a
//     secret key, a "value" member of an object under a secret key or after a
//     "name"/"key" member naming a secret ({"value":…,"name":…} in this order
//     stays), and -u/--user after curl in an argv.
//   - Tokens recognized only by their shape are the kinds in patterns; JWT,
//     glpat- and npm_ values are hidden only after a secret name or option.
//   - Context rules run only for texts holding their anchor words
//     (contextAnchors); a secret name counts where it is used as a name, before
//     : or =, or as an option.
//   - A secret name before a structure with a name inside ({password: x},
//     {note: x}, a quoted JSON object) or before the next secret name with its
//     own value (auth: password: x) takes no value: the names inside get their
//     own match, so auth: {note: x} leaves x visible. {a:b} and token:x are
//     values. A name exactly pwd with a path of plain words (/srv/app, ~/src)
//     is a directory and stays; MYSQL_PWD and other *pwd names are passwords.
//   - Under auth and credentials names a plain
//     word or path stays: same-origin, oauth2, basic, /etc/app/creds.json. A
//     token-like value (see isPlainWord: mixed case and digits, a lower to
//     upper turn as in base64, 12 hex characters, 16 letters) is hidden; a
//     short lower-case password such as hunter2 there is not.
//   - Context rules can hide prose that resembles an assignment or option,
//     including “--token is no longer needed” and “password: word”.
package secretmask

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	privateKeyRE         = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----|\z)`)
	awsRE                = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	escapeRE             = regexp.MustCompile(`(?:\\[A-Za-z]|\\[0-7]{1,3}|\\x[0-9A-Fa-f]{2}|\\u[0-9A-Fa-f]{4}|\\u\{[0-9A-Fa-f]+\}|\\U[0-9A-Fa-f]{8}|%(?:25)*[0-9A-Fa-f]{2}|=[0-9A-F]{2}|&#[0-9]+;?|&#x[0-9A-Fa-f]+;?|(?:\x1b|\\[eE]|\\033|\\33|\\x1[bB]|\\u001[bB]|\^\[|␛)\[[0-9;?]*[A-Za-z]|\x1b\(B|\[[0-9;]+m)$`)
	contextNameRE        = regexp.MustCompile(`(?i)([a-z0-9_.-]+)(?:\\*["'])?([ \t]*)(:=|=>|=|:)`)
	contextRussianNameRE = regexp.MustCompile(`(?:(?:\*\*|__|\x60|["'«])?)(?:[Пп][Аа][Рр][Оо][Лл][\p{L}]*|[Тт][Оо][Кк][Ее][Нн][\p{L}]*|[Кк][Лл][Юю][Чч][\p{L}]*)(?:["'»]|(?:\*\*|__|\x60))?(?:[ \t]+[\p{L}\p{N}]+){0,4}[ \t]*(?::=|=>|=|:|—|–| - )(?:(?:\*\*|__|\x60)[ \t]+)?`)

	headerRE = regexp.MustCompile(`(?im)(?:proxy-)?authorization(?:\\*["'])?[ \t]*[:=][ \t]*(?:(?:\\*["'])?(?:bearer|basic|token|digest|negotiate)(?:[ \t]+|\r?$))?`)
	flagRE   = regexp.MustCompile("(?i)(?:^|[ \\t\\r\\n'\"`])--?([a-z0-9_.-]+)")
)

// Anchor groups say which context rules can find anything in a text. A text
// without anchors skips the context rules; one with anchors runs only the rules
// of the groups it has.
type anchorGroup uint8

const (
	anchorName        anchorGroup = 1 << iota // secret names: assignments, flags, JSON keys and arrays
	anchorHeader                              // Authorization headers
	anchorURL                                 // credentials in a URL
	anchorCurl                                // curl options, also in a JSON argv
	anchorTool                                // passwords in tool options and pipelines
	anchorRuName      anchorGroup = 1 << 5    // Russian password, token and key labels
	anchorRuCandidate anchorGroup = 1 << 6
)

// anchorEdge is what must surround an anchor for it to count.
type anchorEdge uint8

const (
	// edgeNone: anywhere, also inside a longer word.
	edgeNone anchorEdge = iota
	// edgeNameUse: the anchor ends a name, digits aside, as secretName requires,
	// and the name is used the way the name rules take it: before : or = (or the
	// comma of a JSON name and value), or as an option. "tokens", "passed" and
	// "no credentials here" are no anchors; "db_password1=", "--db_password" are.
	edgeNameUse
	// edgeCommand: a command word, with the left edge of findCommandWord.
	edgeCommand
	// edgeCommandPrefix: a command word that can go on (mysqldump).
	edgeCommandPrefix
)

type contextAnchor struct {
	word  string
	group anchorGroup
	edge  anchorEdge
}

var contextAnchors = [...]contextAnchor{
	{"арол", anchorRuName, edgeNone},
	{"окен", anchorRuName, edgeNone},
	{"люч", anchorRuName, edgeNone},
	{"authorization", anchorHeader, edgeNone},
	{"password", anchorName, edgeNameUse}, {"passwd", anchorName, edgeNameUse},
	{"passphrase", anchorName, edgeNameUse}, {"pass", anchorName, edgeNameUse},
	{"pwd", anchorName, edgeNameUse}, {"token", anchorName, edgeNameUse},
	{"api_key", anchorName, edgeNameUse}, {"api-key", anchorName, edgeNameUse},
	{"apikey", anchorName, edgeNameUse}, {"access_key", anchorName, edgeNameUse},
	{"private_key", anchorName, edgeNameUse}, {"auth", anchorName, edgeNameUse},
	{"credentials", anchorName, edgeNameUse},

	// secretName also takes a name holding secret and ending in key.
	{"secret", anchorName, edgeNone},
	{"://", anchorURL, edgeNone},
	{"curl", anchorCurl, edgeCommand},
	{"sshpass", anchorTool, edgeCommand}, {"mysql", anchorTool, edgeCommandPrefix},
	{"mariadb", anchorTool, edgeCommandPrefix}, {"redis-cli", anchorTool, edgeCommand},
	{"mongo", anchorTool, edgeCommand}, {"docker", anchorTool, edgeCommand},
	{"htpasswd", anchorTool, edgeCommand}, {"gh", anchorTool, edgeCommand},
	{"sudo", anchorTool, edgeCommand},
}

// anchorAutomaton is the Aho-Corasick automaton over contextAnchors with the
// failure links folded in: next is defined for every byte after anchorFold.
// A target state where anchors end carries anchorHit in its number.
type anchorAutomaton struct {
	next    [][128]uint16
	anchors [][]uint8 // indices into contextAnchors ending in each state
}

const anchorHit = 1 << 15

// anchorFold folds ASCII and Russian label bytes for the anchor automaton.
var anchorFold = func() (fold [256]byte) {
	for c := range 128 {
		fold[c] = byte(c)
		if c >= 'A' && c <= 'Z' {
			fold[c] = byte(c + 32)
		}
	}
	fold[0xD0], fold[0xD1] = 1, 1
	for i, pair := range [...][2]byte{
		{0x90, 0xB0}, // а
		{0xA0, 0x80}, // р
		{0x9E, 0xBE}, // о
		{0x9B, 0xBB}, // л
		{0x9A, 0xBA}, // к
		{0x95, 0xB5}, // е
		{0x9D, 0xBD}, // н
		{0xAE, 0x8E}, // ю
		{0xA7, 0x87}, // ч
	} {
		fold[pair[0]], fold[pair[1]] = byte(i+2), byte(i+2)
	}
	return fold
}()

var contextAnchorAutomaton = buildAnchorAutomaton()

func buildAnchorAutomaton() anchorAutomaton {
	children := [][128]uint16{{}}
	anchors := [][]uint8{nil}
	for index, anchor := range contextAnchors {
		state := 0
		for i := 0; i < len(anchor.word); i++ {
			c := anchorFold[anchor.word[i]]
			if children[state][c] == 0 {
				children = append(children, [128]uint16{})
				anchors = append(anchors, nil)
				children[state][c] = uint16(len(children) - 1)
			}
			state = int(children[state][c])
		}
		anchors[state] = append(anchors[state], uint8(index))
	}
	next := make([][128]uint16, len(children))
	fail := make([]int, len(children))
	queue := make([]int, 0, len(children))
	for c := range 128 {
		if child := children[0][c]; child != 0 {
			queue = append(queue, int(child))
		}
		next[0][c] = children[0][c]
	}
	for head := 0; head < len(queue); head++ {
		state := queue[head]
		anchors[state] = append(anchors[state], anchors[fail[state]]...)
		for c := range 128 {
			child := children[state][c]
			if child == 0 {
				next[state][c] = next[fail[state]][c]
				continue
			}
			next[state][c] = child
			fail[child] = int(next[fail[state]][c])
			queue = append(queue, int(child))
		}
	}
	for state := range next {
		for c := range 128 {
			if len(anchors[next[state][c]]) != 0 {
				next[state][c] |= anchorHit
			}
		}
	}
	return anchorAutomaton{next: next, anchors: anchors}
}

// contextAnchorGroups returns the groups of the anchors found in text, in one
// pass over it.
func contextAnchorGroups(text string) anchorGroup {
	var groups anchorGroup
	automaton := &contextAnchorAutomaton
	state := uint16(0)
	for i := 0; i < len(text); i++ {
		state = automaton.next[state&^anchorHit][anchorFold[text[i]]]
		if state&anchorHit == 0 {
			continue
		}
		for _, index := range automaton.anchors[state&^anchorHit] {
			anchor := contextAnchors[index]
			if groups&anchor.group == 0 && anchorEdgeAt(text, i+1-len(anchor.word), i+1, anchor.edge) {
				groups |= anchor.group
			}
		}
	}
	if groups&anchorRuName != 0 && russianHeaderCandidateInText(text) {
		groups |= anchorRuCandidate
	}
	return groups
}

func isNameByte(c byte) bool {
	return isAlphaNum(c) || c == '_' || c == '.' || c == '-'
}

func nameUsedAt(text string, start, end int) bool {
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	if end < len(text) && isNameByte(text[end]) {
		return false
	}
	return nameIsOption(text, start) || nameBeforeDelimiter(text, end)
}

// nameIsOption: the name run holding the anchor starts with a dash. A very long
// run is taken as one.
func nameIsOption(text string, start int) bool {
	run := start
	for run > 0 && start-run < 64 && isNameByte(text[run-1]) {
		run--
	}
	return text[run] == '-' || run > 0 && isNameByte(text[run-1])
}

// nameBeforeDelimiter: what contextNameRE allows between a name and its
// delimiter, then : or =, or the comma of a JSON name and value.
func nameBeforeDelimiter(text string, at int) bool {
	for at < len(text) && text[at] == '\\' {
		at++
	}
	if at < len(text) && (text[at] == '"' || text[at] == '\'') {
		at++
	}
	for at < len(text) && (text[at] == ' ' || text[at] == '\t') {
		at++
	}
	return at < len(text) && (text[at] == ':' || text[at] == '=' || text[at] == ',')
}

// anchorCommandStart: a command word can start here in some segment of
// commandSegments, which also starts after ; | and &.
func anchorCommandStart(text string, start int) bool {
	return commandLeftBoundary(text, start) || strings.ContainsRune(";|&", rune(text[start-1]))
}

func anchorEdgeAt(text string, start, end int, edge anchorEdge) bool {
	switch edge {
	case edgeNameUse:
		return nameUsedAt(text, start, end)
	case edgeCommand:
		return anchorCommandStart(text, start) && (end == len(text) || !isAlphaNum(text[end]))
	case edgeCommandPrefix:
		return anchorCommandStart(text, start)
	default:
		return true
	}
}

type contextValue struct {
	start, end int
	quoted     bool
}
type contextMatch struct {
	start, end int
	kind       string
}
type contextSpan struct {
	start, end int
	kind       string
}

type bodyClass uint8

const (
	alphaNum bodyClass = iota
	alphaUnderscore
	alphaDash
	tokenChars
)

type acceptance uint8

const (
	acceptDigitAndRun acceptance = iota
	acceptGenericSk
	// acceptAlsoStandingAlone adds a body without a digit where the token stands
	// on its own: these prefixes rarely start anything but a key.
	acceptAlsoStandingAlone
)

const (
	skAntMinimumLength       = 27
	githubPatMinimumLength   = 41
	githubTokenMinimumLength = 34
	googleTokenMinimumLength = 34
	liveKeyKind              = "sk_live"
)

type pattern struct {
	kind       string
	variants   []tokenVariant
	acceptance acceptance
	re         *regexp.Regexp
}

type tokenVariant struct {
	prefix    string
	body      bodyClass
	minLength int
}

// patterns is the single ordered source for all secret kinds. Generic sk is
// last so it sees the text after more specific tokens have been replaced.
var patterns = [...]pattern{
	{kind: "private-key", re: privateKeyRE},
	{kind: liveKeyKind, variants: []tokenVariant{{"sk_live_", alphaNum, 24}, {"sk_test_", alphaNum, 24}, {"rk_live_", alphaNum, 24}, {"rk_test_", alphaNum, 24}, {"pk_live_", alphaNum, 24}, {"pk_test_", alphaNum, 24}}, acceptance: acceptAlsoStandingAlone},
	{kind: "sk", variants: []tokenVariant{{"sk-proj-", tokenChars, 28}, {"sk-ant-", tokenChars, skAntMinimumLength}}, acceptance: acceptDigitAndRun},
	{kind: "github", variants: []tokenVariant{{"github_pat_", alphaUnderscore, githubPatMinimumLength}}, acceptance: acceptDigitAndRun},
	{kind: "github", variants: []tokenVariant{{"ghp_", alphaNum, githubTokenMinimumLength}, {"gho_", alphaNum, githubTokenMinimumLength}, {"ghu_", alphaNum, githubTokenMinimumLength}, {"ghs_", alphaNum, githubTokenMinimumLength}, {"ghr_", alphaNum, githubTokenMinimumLength}}, acceptance: acceptAlsoStandingAlone},
	{kind: "slack", variants: []tokenVariant{{"xoxa-", alphaDash, 15}, {"xoxb-", alphaDash, 15}, {"xoxe-", alphaDash, 15}, {"xoxo-", alphaDash, 15}, {"xoxp-", alphaDash, 15}, {"xoxr-", alphaDash, 15}, {"xoxs-", alphaDash, 15}}, acceptance: acceptDigitAndRun},
	{kind: "aws", re: awsRE},
	{kind: "google", variants: []tokenVariant{{"AIza", tokenChars, googleTokenMinimumLength}}, acceptance: acceptAlsoStandingAlone},
	{kind: "sk", variants: []tokenVariant{{"sk-", tokenChars, 23}, {"sk_", tokenChars, 23}}, acceptance: acceptGenericSk},
}

// Mask replaces every secret in text with [скрыто:<kind>] and returns the text
// and the number of matches by kind (empty, not nil, when there are none).
func Mask(text string) (string, map[string]int) {
	m := masking{found: map[string]int{}}
	return m.mask(text), m.found
}

// masking is one Mask call: the counts by kind and, when tagging, the hidden
// values in the order their placeholders were written.
type masking struct {
	found  map[string]int
	tagger *Tagger
	ph     *placeholders
	values []string
	kept   []string
}

func (m *masking) mask(text string) string {
	var scan *scanIndex
	for _, p := range patterns {
		var indices [][]int
		if p.re != nil {
			indices = p.re.FindAllStringIndex(text, -1)
		} else {
			var indexed *scanIndex
			indices, indexed = findTokenMatches(text, p, scan)
			if indexed != nil {
				scan = indexed
			}
		}
		if len(indices) != 0 {
			text = replaceMatches(text, indices, p.kind, m)
			scan = nil // replacements invalidate byte offsets and run metadata
		}
	}
	if groups := contextAnchorGroups(text); groups != 0 {
		text = maskJSONContext(text, m, groups)
		text = maskContext(text, m, groups)
	}
	if m.found[liveKeyKind] != 0 {
		text = m.dropLiveKeyTails(text)
	}
	if m.tagger != nil {
		text = m.render(text)
	}
	return text
}

// dropLiveKeyTails removes the token characters right after a sk_live marker: a
// key of these kinds can carry _ and - in its body, and recognition stops at the
// first of them. It runs last, so the context rules still see whatever is glued
// to the key and the output differs from theirs only by what is removed here.
// A tagged value takes its removed tail along.
func (m *masking) dropLiveKeyTails(text string) string {
	marker := maskMarker + liveKeyKind
	var out strings.Builder
	for {
		at := strings.Index(text, marker)
		if at < 0 {
			out.WriteString(text)
			return out.String()
		}
		end := at + len(marker)
		index, closed := m.placeholderEnd(text, &end)
		out.WriteString(text[:end])
		if !closed {
			text = text[end:]
			continue
		}
		tail := end
		for tail < len(text) && bodyContains(tokenChars, text[tail]) {
			tail++
		}
		if index >= 0 {
			m.extend(index, text[end:tail])
		}
		text = text[tail:]
	}
}

// placeholderEnd moves end past the close of a marker whose kind ends at end:
// "]" or a placeholder number and "]". It returns the placeholder number (-1
// for a plain marker) and false when no close follows, as after sk_live_x.
func (m *masking) placeholderEnd(text string, end *int) (int, bool) {
	rest := text[*end:]
	if strings.HasPrefix(rest, "]") {
		*end++
		return -1, true
	}
	if m.tagger == nil || !strings.HasPrefix(rest, m.ph.value) {
		return -1, false
	}
	digits, index := len(m.ph.value), 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		index = index*10 + int(rest[digits]-'0')
		if index >= len(m.values) {
			return -1, false // also keeps the number from overflowing
		}
		digits++
	}
	if digits == len(m.ph.value) || digits == len(rest) || rest[digits] != ']' {
		return -1, false
	}
	*end += digits + 1
	return index, true
}

// extend adds the dropped tail of a sk_live key to the value it belongs to.
func (m *masking) extend(index int, tail string) {
	m.values[index] = m.values[index] + tail
}

// Context rules recognize values after familiar command and configuration names.
func maskContext(text string, m *masking, groups anchorGroup) string {
	matches := make([]contextMatch, 0)
	if groups&anchorHeader != 0 {
		matches = append(matches, headerMatches(text)...)
	}
	if groups&anchorName != 0 {
		matches = append(matches, nameMatches(text)...)
		matches = append(matches, flagMatches(text)...)
	}
	if groups&anchorRuCandidate != 0 {
		matches = append(matches, russianNameMatches(text)...)
	}
	if groups&anchorURL != 0 {
		matches = append(matches, urlMatches(text)...)
	}
	if groups&(anchorCurl|anchorTool) != 0 {
		segments := commandSegments(text)
		if groups&anchorCurl != 0 {
			matches = append(matches, curlMatches(text, segments)...)
			matches = append(matches, curlExtraMatches(text, segments)...)
		}
		if groups&anchorTool != 0 {
			matches = append(matches, toolPasswordMatches(text, segments)...)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].start < matches[j].start })
	spans := make([]contextSpan, 0, len(matches))
	last := 0
	for _, match := range matches {
		if match.start < last || match.end <= match.start {
			continue
		}
		spans = append(spans, contextSpan{match.start, match.end, match.kind})
		last = match.end
	}
	return replaceSpans(text, spans, m)
}

// maskJSONContext hides values that JSON keeps apart from their name: an
// option and its value as neighbouring array elements, an array or an object
// under a secret key, and a {"name": ..., "value": ...} pair.
func maskJSONContext(text string, m *masking, groups anchorGroup) string {
	if groups&(anchorName|anchorCurl|anchorRuCandidate) == 0 {
		return text
	}
	var spans []contextSpan
	for at := 0; ; {
		next := strings.IndexByte(text[at:], '[')
		if next < 0 {
			break
		}
		at += next
		elements := jsonStringElements(text, at)
		spans = append(spans, jsonArraySpans(text, at, elements, groups)...)
		// A bracket inside a string already read opens no array: skipping them
		// keeps the pass linear.
		at++
		if len(elements) != 0 {
			at = elements[len(elements)-1][1] + 1
		}
	}
	if groups&anchorName != 0 {
		spans = append(spans, jsonValueMemberSpans(text)...)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	kept := spans[:0]
	last := 0
	for _, span := range spans {
		if span.start >= last {
			kept = append(kept, span)
			last = span.end
		}
	}
	return replaceSpans(text, kept, m)
}

// jsonString returns the content of the JSON string opening at quote, or false.
func jsonString(text string, quote int) (int, int, bool) {
	if quote >= len(text) || text[quote] != '"' {
		return 0, 0, false
	}
	for i := quote + 1; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '"':
			return quote + 1, i, true
		}
	}
	return 0, 0, false
}

func skipJSONSpace(text string, at int) int {
	for at < len(text) && strings.ContainsRune(" \t\r\n", rune(text[at])) {
		at++
	}
	return at
}

func skipJSONSpaceBack(text string, at int) int {
	for at > 0 && strings.ContainsRune(" \t\r\n", rune(text[at-1])) {
		at--
	}
	return at
}

// jsonKeyBefore returns the key of the member whose value starts at at: "key": here.
func jsonKeyBefore(text string, at int) (string, bool) {
	at = skipJSONSpaceBack(text, at)
	if at == 0 || text[at-1] != ':' {
		return "", false
	}
	return jsonStringBefore(text, at-1)
}

// jsonStringBefore returns a short plain string ending right before at, spaces
// aside: a name has no escapes, and the bound keeps the look back short.
func jsonStringBefore(text string, at int) (string, bool) {
	at = skipJSONSpaceBack(text, at)
	if at == 0 || text[at-1] != '"' {
		return "", false
	}
	end := at - 1
	start := strings.LastIndexByte(text[max(end-128, 0):end], '"')
	if start < 0 {
		return "", false
	}
	start += max(end-128, 0) + 1
	name := text[start:end]
	if name == "" || strings.ContainsAny(name, "\\ \t\r\n") {
		return "", false
	}
	return name, true
}

// jsonArraySpans looks at the strings of the array opening at open: under a
// secret key all of them are values; a secret option hides the element after
// it; in a curl argv -u hides the password part of the next element.
func jsonArraySpans(text string, open int, elements [][2]int, groups anchorGroup) []contextSpan {
	if len(elements) == 0 {
		return nil
	}
	if groups&anchorName != 0 {
		if key, ok := jsonKeyBefore(text, open); ok {
			if kind, secret := secretName(key); secret {
				return jsonKeyedSpans(text, key, kind, elements)
			}
		}
	}
	return jsonArgumentSpans(text, elements, groups)
}

// jsonKeyedSpans hides the elements of an array under a secret key.
func jsonKeyedSpans(text, key, kind string, elements [][2]int) []contextSpan {
	var spans []contextSpan
	for _, element := range elements {
		if !plainValueFor(key, text[element[0]:element[1]]) {
			spans = appendJSONValue(spans, text, element, kind)
		}
	}
	return spans
}

// jsonStringElements lists the contents of the leading strings of the array
// opening at open, up to its end or its first other element.
func jsonStringElements(text string, open int) [][2]int {
	var elements [][2]int
	at := skipJSONSpace(text, open+1)
	for {
		start, end, ok := jsonString(text, at)
		if !ok {
			return elements
		}
		elements = append(elements, [2]int{start, end})
		at = skipJSONSpace(text, end+1)
		if at >= len(text) || text[at] != ',' {
			return elements
		}
		at = skipJSONSpace(text, at+1)
	}
}

func appendJSONValue(spans []contextSpan, text string, element [2]int, kind string) []contextSpan {
	if quotedIsValue(text[element[0]:element[1]]) {
		spans = append(spans, contextSpan{element[0], element[1], kind})
	}
	return spans
}

// jsonArgumentSpans reads the array as command arguments: a secret option hides
// the next element, and after curl -u or --user hides its password part.
func jsonArgumentSpans(text string, elements [][2]int, groups anchorGroup) []contextSpan {
	var spans []contextSpan
	curl := false
	for i := 0; i+1 < len(elements); i++ {
		word := text[elements[i][0]:elements[i][1]]
		next := elements[i+1]
		switch {
		case word == "curl" || strings.HasSuffix(word, "/curl"):
			curl = groups&anchorCurl != 0
		case curl && (word == "--user" || jsonCurlUserFlag(word)):
			if colon := strings.IndexByte(text[next[0]:next[1]], ':'); colon >= 0 {
				spans = appendJSONValue(spans, text, [2]int{next[0] + colon + 1, next[1]}, "password")
			}
			i++
		case groups&anchorName != 0 && strings.HasPrefix(word, "-") && !strings.Contains(word, "="):
			name := strings.TrimLeft(word, "-")
			if kind, ok := optionSecretName(name); ok && flagValueAllowed(text[next[0]:next[1]]) && !plainValueFor(name, text[next[0]:next[1]]) {
				spans = appendJSONValue(spans, text, next, kind)
				i++
			}
		}
	}
	return spans
}

// optionSecretName is secretName for an option name; options that turn a
// secret off or ask for it hold none.
func optionSecretName(name string) (string, bool) {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "no-") || strings.HasPrefix(lower, "ask-") || strings.HasPrefix(lower, "prompt-") {
		return "", false
	}
	return secretName(name)
}

// jsonCurlUserFlag: -u, alone or closing a group of short options (-su).
func jsonCurlUserFlag(word string) bool {
	return len(word) >= 2 && word[0] == '-' && word[len(word)-1] == 'u' && onlyLetters(word[1:])
}

// jsonValueMemberSpans hides the string of a "value" member when its object
// sits under a secret key, {"token":{"value":"…"}}, or names a secret,
// {"name":"password","value":"…"}.
func jsonValueMemberSpans(text string) []contextSpan {
	const member = `"value"`
	var spans []contextSpan
	for at := 0; ; {
		next := strings.Index(text[at:], member)
		if next < 0 {
			return spans
		}
		at += next
		keyAt := at
		at += len(member)
		colon := skipJSONSpace(text, at)
		if colon >= len(text) || text[colon] != ':' {
			continue
		}
		start, end, ok := jsonString(text, skipJSONSpace(text, colon+1))
		if !ok || !quotedIsValue(text[start:end]) {
			continue
		}
		before := skipJSONSpaceBack(text, keyAt)
		if before == 0 {
			continue
		}
		var name string
		switch text[before-1] {
		case '{':
			name, ok = jsonKeyBefore(text, before-1)
		case ',':
			var label string
			name, ok = jsonStringBefore(text, before-1)
			if ok {
				label, ok = jsonKeyBefore(text, before-1-len(name)-2)
				ok = ok && (label == "name" || label == "key")
			}
		default:
			ok = false
		}
		if !ok {
			continue
		}
		if kind, secret := secretName(name); secret && !plainValueFor(name, text[start:end]) {
			spans = append(spans, contextSpan{start, end, kind})
			at = end
		}
	}
}

func headerMatches(text string) []contextMatch {
	var matches []contextMatch
	for _, ix := range headerRE.FindAllStringIndex(text, -1) {
		value, ok := scanValue(text, ix[1], false)
		if ok && valueIsValue(text, value, "", "authorization", false) {
			matches = append(matches, contextMatch{value.start, value.end, "authorization"})
		}
	}
	return matches
}

func russianHeaderCandidateInText(text string) bool {
	for at := 0; at < len(text); at++ {
		if russianNameAnchorAt(text, at) && russianHeaderCandidateAt(text, at-2) {
			return true
		}
	}
	return false
}

func russianNameAnchorAt(text string, at int) bool {
	if at+8 > len(text) || text[at] != 0xD0 && text[at] != 0xD1 {
		return false
	}
	for _, base := range [...]string{"арол", "окен"} {
		if russianAnchorBytesMatch(text[at:at+len(base)], base) {
			return true
		}
	}
	return at+6 <= len(text) && russianAnchorBytesMatch(text[at:at+6], "люч")
}

func russianAnchorBytesMatch(text, base string) bool {
	for i := range len(base) {
		if anchorFold[text[i]] != anchorFold[base[i]] {
			return false
		}
	}
	return true
}

func russianHeaderCandidateAt(text string, wordStart int) bool {
	end, ok := russianCandidateLabelEnd(text, wordStart)
	return ok && russianCandidateHasDelimiter(text, end)
}

func russianCandidateLabelEnd(text string, wordStart int) (int, bool) {
	if wordStart < 0 || wordStart >= len(text) {
		return 0, false
	}
	boundaryStart := russianCandidateBoundaryStart(text, wordStart)
	if !russianNameBoundaryAt(text, boundaryStart) {
		return 0, false
	}
	end := wordEnd(text, wordStart, false)
	if _, _, ok := russianSecretLabel(text[wordStart:end]); !ok {
		return 0, false
	}
	if strings.HasPrefix(text[end:], "**") || strings.HasPrefix(text[end:], "__") {
		end += 2
	} else if end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		if strings.ContainsRune("`\"'»", r) {
			end += size
		}
	}
	return end, true
}

func russianCandidateBoundaryStart(text string, start int) int {
	if start >= 2 && (text[start-2:start] == "**" || text[start-2:start] == "__") {
		return start - 2
	}
	if start > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:start])
		if strings.ContainsRune("`\"'«", r) {
			return start - size
		}
	}
	return start
}

func wordEnd(text string, start int, numbers bool) int {
	for end := start; end < len(text); {
		r, size := utf8.DecodeRuneInString(text[end:])
		if !unicode.IsLetter(r) && (!numbers || !unicode.IsNumber(r)) {
			return end
		}
		end += size
	}
	return len(text)
}

func russianCandidateHasDelimiter(text string, end int) bool {
	for words := 0; words <= 4; words++ {
		if russianDelimiterAtStart(text[end:]) != "" {
			return true
		}
		end = skipRussianSpace(text, end)
		if russianDelimiterAtStart(text[end:]) != "" {
			return true
		}
		if words == 4 {
			return false
		}
		next := wordEnd(text, end, true)
		if next == end {
			return false
		}
		end = next
	}
	return false
}

func skipRussianSpace(text string, at int) int {
	for at < len(text) && (text[at] == ' ' || text[at] == '	') {
		at++
	}
	return at
}

func russianDelimiterAtStart(text string) string {
	for _, delimiter := range [...]string{":=", "=>", "=", ":", "—", "–", " - "} {
		if strings.HasPrefix(text, delimiter) {
			return delimiter
		}
	}
	return ""
}

func russianNameMatches(text string) []contextMatch {
	var matches []contextMatch
	for _, ix := range contextRussianNameRE.FindAllStringIndex(text, -1) {
		start, end := ix[0], ix[1]
		if !russianNameBoundaryAt(text, start) || withinMaskMarker(text, start) || withinMaskMarker(text, end) {
			continue
		}
		name, kind, delimiter := russianNameFromHeader(text[start:end])
		if name == "" {
			continue
		}
		value, ok := russianContextValue(text, end, name, kind, delimiter)
		if !ok {
			continue
		}
		matches = append(matches, contextMatch{value.start, value.end, kind})
	}
	return matches
}

func russianNameFromHeader(header string) (name, kind, delimiter string) {
	delimiterAt, delimiter := russianDelimiterAt(header)
	if delimiter == "" {
		return "", "", ""
	}
	phrase := strings.Trim(header[:delimiterAt], "*_`\"'«»")
	for word := range strings.FieldsSeq(phrase) {
		wordKind, key, ok := russianSecretLabel(word)
		if ok && (name == "" || name == "key" && !key) {
			name, kind = word, wordKind
		}
	}
	return name, kind, delimiter
}

func russianContextValue(text string, end int, name, kind, delimiter string) (contextValue, bool) {
	value, ok := scanValue(text, end, delimiter == ":" || delimiter == ":=")
	if !ok || valueIsStructure(text, value) ||
		!valueIsValueFor(text, value, name, kind, delimiter == ":" || delimiter == ":=", true) {
		return contextValue{}, false
	}
	if !value.quoted && (delimiter == ":=" || delimiter == "=>") && !isRussianValueLabel(name) {
		return contextValue{}, false
	}
	return value, true
}

func russianDelimiterAt(header string) (int, string) {
	for i := 0; i < len(header); i++ {
		switch {
		case strings.HasPrefix(header[i:], ":="):
			return i, ":="
		case strings.HasPrefix(header[i:], "=>"):
			return i, "=>"
		case header[i] == '=':
			return i, "="
		case header[i] == ':':
			return i, ":"
		case strings.HasPrefix(header[i:], "—"):
			return i, "—"
		case strings.HasPrefix(header[i:], "–"):
			return i, "–"
		case strings.HasPrefix(header[i:], " - "):
			return i, " - "
		}
	}
	return 0, ""
}

func russianNameBoundaryAt(text string, at int) bool {
	if at == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:at])
	return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != 95
}

func isRussianValueLabel(name string) bool {
	_, _, ok := russianSecretLabel(name)
	return ok
}

func shortRussianValue(value string) bool {
	if utf8.RuneCountInString(value) > 8 {
		return false
	}
	for _, r := range value {
		if unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func russianSecretLabel(word string) (kind string, key bool, ok bool) {
	switch strings.ToLower(word) {
	case "пароль", "пароля", "паролю", "паролем", "пароле", "пароли", "паролей", "паролям", "паролями", "паролях":
		return "password", false, true
	case "токен", "токена", "токену", "токеном", "токене", "токены", "токенов", "токенам", "токенами", "токенах":
		return "token", false, true
	case "ключ", "ключа", "ключу", "ключом", "ключе", "ключи", "ключей", "ключам", "ключами", "ключах":
		return "token", true, true
	default:
		return "", false, false
	}
}

func nameMatches(text string) []contextMatch {
	var matches []contextMatch
	for _, ix := range contextNameRE.FindAllStringSubmatchIndex(text, -1) {
		if withinMaskMarker(text, ix[0]) || withinMaskMarker(text, ix[6]) {
			continue
		}
		// A name inside the value before it still counts: its own value can lie
		// past that value's end (auth=::X-API-KEY: value). Overlaps are dropped
		// in maskContext.
		name, kind, ok := contextSecretName(text, ix[2], ix[3])
		if !ok {
			continue
		}
		leftSpace := text[ix[4]:ix[5]]
		delimiter := text[ix[6]:ix[7]]
		at := ix[1]
		if delimiter == "=" && leftSpace == "" && hasHorizontalSpaceAt(text, at) {
			continue
		}
		value, ok := scanValue(text, at, delimiter == ":" || delimiter == ":=")
		if !ok {
			continue
		}
		// A structure or the next name is no value: the names inside it get
		// their own match.
		if valueIsStructure(text, value) {
			continue
		}
		if plainValueFor(name, text[value.start:value.end]) || nonSecretValueForm(text, value, name) {
			continue
		}
		if !value.quoted && notSecretHere(text, ix[2], name, delimiter, text[value.start:value.end]) {
			continue
		}
		// After := and => a bare value is an expression in code; a literal has quotes.
		if !value.quoted && (delimiter == ":=" || delimiter == "=>") {
			continue
		}
		if valueIsValue(text, value, name, kind, delimiter == ":" || delimiter == ":=") {
			matches = append(matches, contextMatch{value.start, value.end, kind})
		}
	}
	return matches
}

// nonSecretValueForm recognizes code and diagnostic syntax that cannot be a credential.
func nonSecretValueForm(text string, value contextValue, name string) bool {
	if value.quoted {
		return false
	}
	n := strings.TrimRight(strings.ToLower(name), "0123456789")
	if authName(n) {
		if typeExpressionHead(text, value) || goCompositeLiteralHead(text[value.start:value.end]) || shortNumberInNumericList(text, value) {
			return true
		}
	}
	return strings.EqualFold(name, "pwd") && pwdErrorDiagnostic(text, value)
}

func pwdErrorDiagnostic(text string, value contextValue) bool {
	rest := strings.ToLower(text[value.start:])
	if end := strings.IndexAny(rest, "\r\n"); end >= 0 {
		rest = rest[:end]
	}
	const getcwd = "error retrieving current directory: getcwd: cannot access parent directories"
	const shell = "cannot access parent directories"
	if after, ok := strings.CutPrefix(rest, getcwd); ok {
		rest = after
	} else if after, ok := strings.CutPrefix(rest, shell); ok {
		rest = after
	} else {
		return false
	}
	return rest == "" || rest == ": no such file or directory"
}

func typeExpressionHead(text string, value contextValue) bool {
	v := text[value.start:value.end]
	angle := strings.IndexByte(v, '<')
	if angle < 1 || !plainTypeName(v[:angle]) {
		return false
	}
	i := angle + 1
	if i >= len(v) || !isIdentifierStart(v[i]) {
		return false
	}
	argStart := i
	for i < len(v) && isIdentifierByte(v[i]) {
		i++
	}
	if !plainTypeName(v[argStart:i]) {
		return false
	}
	if i < len(v) && v[i] == '>' {
		if i+1 != len(v) {
			return false
		}
		return value.end == len(text) || value.end < len(text) && typeTailBoundary(text[value.end])
	}
	if i < len(v) && v[i] == ',' {
		return i+1 == len(v)
	}
	return i == len(v) && value.end < len(text) && text[value.end] == ','
}

// plainTypeName accepts a plain identifier or a sequence of title-case words,
// such as HashMap. Requiring letter-only title-case parts avoids token-like
// names that mix digits with case changes.
func plainTypeName(value string) bool {
	if value == "" || !isLetterOnly(value) {
		return false
	}
	if isPlainWord(value) && plainTypeCase(value) {
		return true
	}
	return titleCaseTypeParts(value)
}

func isLetterOnly(value string) bool {
	for i := 0; i < len(value); i++ {
		if !(value[i] >= 'A' && value[i] <= 'Z' || value[i] >= 'a' && value[i] <= 'z') {
			return false
		}
	}
	return true
}

func plainTypeCase(value string) bool {
	allLower, allUpper := true, true
	for i := 0; i < len(value); i++ {
		allLower = allLower && value[i] >= 'a' && value[i] <= 'z'
		allUpper = allUpper && value[i] >= 'A' && value[i] <= 'Z'
	}
	if allLower || allUpper {
		return true
	}
	if value[0] < 'A' || value[0] > 'Z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		if value[i] >= 'A' && value[i] <= 'Z' {
			return false
		}
	}
	return true
}

func titleCaseTypeParts(value string) bool {
	parts := 0
	for i := 0; i < len(value); {
		if value[i] < 'A' || value[i] > 'Z' {
			return false
		}
		i++
		start := i
		for i < len(value) && value[i] >= 'a' && value[i] <= 'z' {
			i++
		}
		if i-start < 2 {
			return false
		}
		parts++
	}
	return parts >= 2
}

func typeTailBoundary(c byte) bool {
	return c == ';' || c == '=' || c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func isIdentifierStart(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func isIdentifierByte(c byte) bool {
	return isIdentifierStart(c) || c >= '0' && c <= '9'
}

// goCompositeLiteralHead accepts map, slice, and short named composite literals.
func goCompositeLiteralHead(value string) bool {
	if !strings.HasSuffix(value, "{") {
		return false
	}
	typeName := value[:len(value)-1]
	if strings.HasPrefix(typeName, "map[") {
		close := strings.IndexByte(typeName[4:], ']')
		if close < 1 {
			return false
		}
		close += 4
		return simpleGoType(typeName[4:close]) && simpleGoType(typeName[close+1:])
	}
	if strings.HasPrefix(typeName, "[]") {
		return simpleGoType(typeName[2:])
	}
	return false
}

func simpleGoType(value string) bool {
	if value == "" || len(value) > 8 || !isIdentifierStart(value[0]) {
		return false
	}
	for i := 1; i < len(value); i++ {
		if !isIdentifierByte(value[i]) || value[i] >= '0' && value[i] <= '9' {
			return false
		}
	}
	return true
}

func shortNumberInNumericList(text string, value contextValue) bool {
	if !shortNumericValue(text[value.start:value.end]) {
		return false
	}
	lineStart := strings.LastIndexByte(text[:value.start], '\n') + 1
	lineEnd := len(text)
	if end := strings.IndexAny(text[value.end:], "\r\n"); end >= 0 {
		lineEnd = value.end + end
	}
	line := text[lineStart:lineEnd]
	count := 0
	for _, ix := range contextNameRE.FindAllStringSubmatchIndex(line, -1) {
		if line[ix[6]:ix[7]] != "=" {
			continue
		}
		candidate, ok := scanValue(line, ix[1], false)
		if ok && shortNumericValue(line[candidate.start:candidate.end]) {
			count++
		}
	}
	return count >= 3
}

func shortNumericValue(value string) bool {
	number := strings.TrimSuffix(value, ",")
	if len(number) == 0 || len(number) > 6 || len(number) != len(value) && !strings.HasSuffix(value, ",") {
		return false
	}
	for i := range len(number) {
		if number[i] < '0' || number[i] > '9' {
			return false
		}
	}
	return true
}

// contextSecretName is secretName for the name at text[start:end]. After a
// written line break the letter of \n can be no part of the name.
func contextSecretName(text string, start, end int) (string, string, bool) {
	name := text[start:end]
	kind, ok := secretName(name)
	if !ok && start > 0 && text[start-1] == '\\' && len(name) > 1 && strings.ContainsRune("nrt", rune(name[0])) {
		name = name[1:]
		kind, ok = secretName(name)
	}
	return name, kind, ok
}

func flagMatches(text string) []contextMatch {
	var matches []contextMatch
	coveredUntil := 0
	for _, ix := range flagRE.FindAllStringSubmatchIndex(text, -1) {
		if ix[0] < coveredUntil {
			continue
		}
		name := text[ix[2]:ix[3]]
		kind, ok := optionSecretName(name)
		if !ok {
			continue
		}
		at := ix[1]
		assigned := at < len(text) && text[at] == '='
		if assigned {
			at++
		} else if at == len(text) || !hasFlagSpaceAt(text, at) {
			continue
		}
		if assigned && hasHorizontalSpaceAt(text, at) {
			continue
		}
		value, ok := scanValue(text, at, false)
		if !ok {
			continue
		}
		if valueIsStructure(text, value) {
			continue
		}
		coveredUntil = value.end
		if !assigned && !flagValueAllowed(text[value.start:value.end]) || plainValueFor(name, text[value.start:value.end]) {
			continue
		}
		if valueIsValue(text, value, name, kind, false) {
			matches = append(matches, contextMatch{value.start, value.end, kind})
		}
	}
	return matches
}

// urlMatches finds credentials in scheme://user:password@ and a token standing
// for the user, scheme://token@ (see urlUserIsToken).
func urlMatches(text string) []contextMatch {
	var matches []contextMatch
	for at := 0; ; {
		next := strings.Index(text[at:], "://")
		if next < 0 {
			return matches
		}
		at += next + len("://")
		if !urlSchemeBefore(text, at-len("://")) {
			continue
		}
		if match, ok := urlCredential(text, at); ok {
			matches = append(matches, match)
		}
	}
}

// urlCredential reads the authority starting at at: a token user before @, or
// the password after user: and before @.
func urlCredential(text string, at int) (contextMatch, bool) {
	userEnd := at
	for userEnd < len(text) && !urlStop(text[userEnd]) && text[userEnd] != ':' {
		userEnd++
	}
	if userEnd == len(text) || text[userEnd] == '/' || withinMaskMarker(text, at) {
		return contextMatch{}, false
	}
	if text[userEnd] == '@' {
		return contextMatch{at, userEnd, "token"}, userEnd > at && urlUserIsToken(text[at:userEnd])
	}
	if text[userEnd] != ':' {
		return contextMatch{}, false
	}
	passwordAt := userEnd + 1
	passwordEnd := passwordAt
	for passwordEnd < len(text) && !urlStop(text[passwordEnd]) {
		passwordEnd++
	}
	if passwordEnd == passwordAt || passwordEnd == len(text) || text[passwordEnd] != '@' || withinMaskMarker(text, passwordAt) {
		return contextMatch{}, false
	}
	value, ok := scanValue(text, passwordAt, false)
	if !ok {
		return contextMatch{}, false
	}
	value.end = min(value.end, passwordEnd)
	return contextMatch{value.start, value.end, "password"}, valueIsValue(text, value, "", "password", false)
}

// urlStop ends the user and the password of a URL.
func urlStop(c byte) bool {
	return c == '@' || c == '/' || c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// urlSchemeBefore: a scheme with a letter ends at this place.
func urlSchemeBefore(text string, end int) bool {
	for i := end - 1; i >= 0; i-- {
		c := text[i]
		if isLetter(c) {
			return true
		}
		if !(c >= '0' && c <= '9' || c == '+' || c == '.' || c == '-') {
			return false
		}
	}
	return false
}

// urlUserIsToken: a user name of a URL is taken for a token when it is at
// least 8 characters of letters, digits, _ and -, with both a letter and a
// digit. Plain names (git, user, deploy) stay; a name with digits of that
// length (deploy2024) is hidden too.
func urlUserIsToken(user string) bool {
	if len(user) < 8 {
		return false
	}
	letter, digit := false, false
	for i := 0; i < len(user); i++ {
		c := user[i]
		switch {
		case isLetter(c):
			letter = true
		case c >= '0' && c <= '9':
			digit = true
		case c != '_' && c != '-':
			return false
		}
	}
	return letter && digit
}

func addToolPassword(text string, offset, at int, matches *[]contextMatch) {
	value, ok := scanValue(text, at, false)
	if ok && (value.quoted || flagValueAllowed(text[value.start:value.end])) && valueIsValue(text, value, "", "password", false) {
		*matches = append(*matches, contextMatch{offset + value.start, offset + value.end, "password"})
	}
}

func findCommandWord(text, name string, prefix bool) (contextValue, bool) {
	for start := 0; start+len(name) <= len(text); start++ {
		if !commandLeftBoundary(text, start) || !asciiEqualFoldAt(text, start, name) {
			continue
		}
		end := start + len(name)
		if prefix {
			for end < len(text) && !strings.ContainsRune(" \t\r\n'\"`)\\", rune(text[end])) {
				end++
			}
			return contextValue{start, end, false}, true
		}
		if commandRightBoundary(text, end) {
			return contextValue{start, end, false}, true
		}
	}
	return contextValue{}, false
}

func asciiEqualFoldAt(text string, start int, want string) bool {
	for i := 0; i < len(want); i++ {
		c := text[start+i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		if c != want[i] {
			return false
		}
	}
	return true
}

func commandLeftBoundary(text string, at int) bool {
	if at == 0 {
		return true
	}
	if at >= 2 && text[at-2] == '\\' && strings.ContainsRune("\nnrt", rune(text[at-1])) {
		return true
	}
	return strings.ContainsRune(" \t\r\n/('\"`", rune(text[at-1]))
}

// literalSpaceAt returns the length of a written \n, \r or \t at this place, with
// the backslashes of any escaping depth: raw JSON and %q output carry them as text.
func literalSpaceAt(text string, at int) int {
	// Only the head of a series can start the literal: this keeps a long series linear.
	if at > 0 && text[at-1] == '\\' {
		return 0
	}
	end := at
	for end < len(text) && text[end] == '\\' {
		end++
	}
	if end == at || end == len(text) || !strings.ContainsRune("nrt", rune(text[end])) {
		return 0
	}
	return end + 1 - at
}

func commandRightBoundary(text string, at int) bool {
	return at == len(text) || strings.ContainsRune(" \t'\"`)", rune(text[at])) || strings.HasPrefix(text[at:], "\\\n") || literalSpaceAt(text, at) > 0
}

func commandSegments(text string) [][2]int {
	quoted := quoteSpans(text)
	var segments [][2]int
	start := 0
	quoteRange := 0
	for i := 0; i < len(text); i++ {
		for quoteRange < len(quoted) && i >= quoted[quoteRange][1] {
			quoteRange++
		}
		if quoteRange < len(quoted) && i >= quoted[quoteRange][0] {
			continue
		}
		if text[i] == '\n' && i > 0 && text[i-1] == '\\' {
			continue
		}
		if strings.ContainsRune(";|&\n", rune(text[i])) {
			segments = append(segments, [2]int{start, i})
			start = i + 1
		}
	}
	segments = append(segments, [2]int{start, len(text)})
	// Inside a quote, a command can start a new line or follow a separator:
	// each piece is searched once more, the pieces not overlapping.
	for _, span := range quoted {
		piece := span[0] + 1
		for i := piece; i < span[1]-1; i++ {
			n := 0
			switch {
			case strings.ContainsRune("\n\r;|&", rune(text[i])):
				n = 1
			case text[i] == '\\':
				if n = literalSpaceAt(text, i); n > 0 && text[i+n-1] == 't' {
					n = 0
				}
			}
			if n == 0 {
				continue
			}
			if piece < i {
				segments = append(segments, [2]int{piece, i})
			}
			piece = i + n
			i += n - 1
		}
		if piece < span[1]-1 {
			segments = append(segments, [2]int{piece, span[1] - 1})
		}
	}
	return segments
}

func skipValueSpace(text string, at int) int {
	for at < len(text) {
		if text[at] == ' ' || text[at] == '\t' {
			at++
			continue
		}
		if strings.HasPrefix(text[at:], "\\\n") {
			at += 2
			continue
		}
		if n := literalSpaceAt(text, at); n > 0 {
			at += n
			continue
		}
		break
	}
	return at
}

func scanValue(text string, at int, colon bool) (contextValue, bool) {
	at = skipValueSpace(text, at)
	if at >= len(text) || strings.HasPrefix(text[at:], maskMarker) || text[at] == '\n' || text[at] == '\r' || text[at] == '=' || text[at] == '>' || text[at] == '`' {
		return contextValue{}, false
	}
	quoteAt := at
	for quoteAt < len(text) && text[quoteAt] == '\\' {
		quoteAt++
	}
	if slashes := quoteAt - at; quoteAt < len(text) && (text[quoteAt] == '\'' || text[quoteAt] == '"') && opensQuote(text[quoteAt], slashes) {
		end := closingQuote(text, quoteAt, slashes)
		if end < 0 {
			return contextValue{}, false
		}
		// The closing mark of a nested string takes its slashes with it.
		end -= slashes
		return contextValue{quoteAt + 1, end, true}, end > quoteAt+1
	}
	if text[at] == '$' && at+1 < len(text) && (text[at+1] == '\'' || text[at+1] == '"') {
		quoteAt := at + 1
		end := closingQuote(text, quoteAt, 0)
		if end < 0 {
			return contextValue{}, false
		}
		return contextValue{quoteAt + 1, end, true}, end > quoteAt+1
	}
	return scanBareValue(text, at, colon)
}

func scanBareValue(text string, at int, colon bool) (contextValue, bool) {
	start := at
	for at < len(text) {
		c := text[at]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || strings.ContainsRune("\"'&;|)}`", rune(c)) || c == ',' && colon || strings.HasPrefix(text[at:], "\\\n") || literalSpaceAt(text, at) > 0 {
			break
		}
		at++
	}
	return contextValue{start, at, false}, at > start
}

func valueIsValue(text string, value contextValue, name, kind string, colon bool) bool {
	return valueIsValueFor(text, value, name, kind, colon, false)
}

func valueIsValueFor(text string, value contextValue, name, kind string, colon, russian bool) bool {
	v := text[value.start:value.end]
	if russian {
		if len(v) < 8 && (onlyDigits(v) || isPlainWord(v)) || shortRussianValue(v) {
			return false
		}
		if strings.HasPrefix(strings.ToLower(name), "ключ") {
			upper, lower, digit := false, false, false
			for _, c := range v {
				switch {
				case c >= 'A' && c <= 'Z':
					upper = true
				case c >= 'a' && c <= 'z':
					lower = true
				case c >= '0' && c <= '9':
					digit = true
				default:
					return false
				}
			}
			return len(v) >= 8 && upper && lower && digit
		}
	}
	if value.quoted {
		return quotedIsValue(v)
	}
	return bareIsValue(v, name, kind, colon)
}

func quotedIsValue(value string) bool {
	// A value starting with a space is the gap between two neighbouring quotes: "NAME=" .env "...".
	return value != "" && !strings.ContainsRune(" \t\r\n", rune(value[0])) && !isReference(value) && !isPlaceholder(value) && !strings.Contains(value, maskMarker)
}

func bareIsValue(value, name, kind string, colon bool) bool {
	// A lone bracket opens an array or an object; the JSON rules look inside.
	if value == "" || value == "[" || value == "{" || isReference(value) || isPlaceholder(value) || strings.Contains(value, maskMarker) || strings.ContainsAny(value, "('") || equalSecretName(value, name) {
		return false
	}
	if strings.Contains(value, ".") {
		identifier := true
		for i := 0; i < len(value); i++ {
			if !isLetter(value[i]) && value[i] != '.' {
				identifier = false
				break
			}
		}
		if identifier {
			return false
		}
	}
	if kind == "token" {
		if len(value) < 8 && (colon || onlyDigits(value)) {
			return false
		}
	}
	return true
}

const (
	maskMarker     = "[скрыто:"
	maskMarkerSpan = 64 // the longest marker fits: a kind is a short word

	maxQuoteSlashes = 31
)

func withinMaskMarker(text string, at int) bool {
	if at < 0 || at > len(text) {
		return false
	}
	start := max(at-len(maskMarker), 0)
	open := strings.LastIndex(text[start:at], maskMarker)
	if open < 0 {
		return false
	}
	open += start
	close := strings.IndexByte(text[open:min(open+maskMarkerSpan, len(text))], ']')
	return close >= 0 && open+close >= at
}

func isReference(value string) bool {
	return len(value) > 1 && value[0] == '$' && (onlyDigits(value[1:]) || value == "$@" || value == "$*") || strings.HasPrefix(value, "${") || strings.HasPrefix(value, "$(") || strings.HasPrefix(value, "`") || len(value) > 1 && value[0] == '$' && ((value[1] >= 'A' && value[1] <= 'Z') || (value[1] >= 'a' && value[1] <= 'z') || value[1] == '_') || strings.HasPrefix(value, `\"$`)
}

func equalSecretName(value, name string) bool {
	clean := func(s string) string {
		s = strings.TrimLeft(s, "-")
		return strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(s))
	}
	return name != "" && clean(value) == clean(name)
}

var placeholderWords = map[string]bool{"%s": true, "%v": true, "string": true, "str": true, "bool": true, "int": true, "true": true, "false": true, "null": true, "nil": true, "none": true, "undefined": true, "required": true, "optional": true, "yes": true, "no": true}

func isPlaceholder(value string) bool {
	if placeholderWords[strings.ToLower(value)] {
		return true
	}
	return len(value) > 1 && (value[0] == '<' && value[len(value)-1] == '>' || value[0] == '[' && value[len(value)-1] == ']' || value[0] == '{' && value[len(value)-1] == '}')
}

func flagValueAllowed(value string) bool {
	return value != "" && value[0] != '-' && value[0] != '<' && value[0] != '>' && value[0] < 0x80
}

func hasHorizontalSpaceAt(text string, at int) bool {
	return at < len(text) && (text[at] == ' ' || text[at] == '\t')
}

// valueIsStructure: what follows a name is an object, an array or a group with
// a name inside ({password: x}, {note: x}, '{"token":"x"}'), or the next secret
// name with its own value after a space (auth: password: x). The names inside
// get their own match. {a:b}, {a=b} and token:x hold no name: they are values.
func valueIsStructure(text string, value contextValue) bool {
	v := text[value.start:value.end]
	if v == "" {
		return false
	}
	if strings.ContainsRune("{[(", rune(v[0])) {
		return holdsName(text, value.start+1, value.end)
	}
	if value.quoted || v[len(v)-1] != ':' {
		return false
	}
	name := v[:len(v)-1]
	// A trailing digit makes a word (Secret1), not the next name.
	if _, secret := secretName(name); !secret || !onlyNameBytes(name) || name[len(name)-1] >= '0' && name[len(name)-1] <= '9' {
		return false
	}
	at := value.end
	for at < len(text) && (text[at] == ' ' || text[at] == '\t') {
		at++
	}
	return at > value.end && at < len(text) && text[at] != '\n' && text[at] != '\r'
}

// holdsName: a word in text[start:end] is a name with a delimiter: a secret
// name before : or =, or any name before ": " as in YAML and JavaScript objects.
func holdsName(text string, start, end int) bool {
	for i := start; i < end; i++ {
		if !isLetter(text[i]) || i > 0 && isNameByte(text[i-1]) {
			continue
		}
		e := i
		for e < end && isNameByte(text[e]) {
			e++
		}
		name := text[i:e]
		for e < len(text) && text[e] == '\\' {
			e++
		}
		if e < len(text) && (text[e] == '"' || text[e] == '\'') {
			e++
		}
		if e >= len(text) {
			return false
		}
		if _, secret := secretName(name); secret && (text[e] == ':' || text[e] == '=') {
			return true
		}
		if text[e] == ':' && e+1 < len(text) && (text[e+1] == ' ' || text[e+1] == '\t') {
			return true
		}
		i = e - 1
	}
	return false
}

func onlyNameBytes(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isNameByte(s[i]) {
			return false
		}
	}
	return s != ""
}

// plainValueFor: the value of this name is no secret. A name exactly pwd with
// a path (/a/b, ~/a) is the working directory. The auth and credentials names
// often hold a mode, a profile or a file: a plain word (same-origin, oauth2,
// basic) or a path stays; a value looking like a token is hidden.
func plainValueFor(name, value string) bool {
	if strings.EqualFold(name, "pwd") {
		return isPlainPath(value)
	}
	n := strings.TrimRight(strings.ToLower(name), "0123456789")
	return authName(n) && (isPlainPath(value) || isPlainWord(value))
}

// isPlainPath: /a/b, ~/a or ./a, every part a plain word: /Bx4Tq9Wz is no path.
func isPlainPath(value string) bool {
	var rest string
	switch {
	case strings.HasPrefix(value, "~/"), strings.HasPrefix(value, "./"):
		rest = value[2:]
	case strings.HasPrefix(value, "/"):
		rest = value[1:]
	default:
		return false
	}
	for part := range strings.SplitSeq(rest, "/") {
		if part != "" && !isPlainWord(part) && !isTempDirectoryComponent(part) {
			return false
		}
	}
	return true
}

// isTempDirectoryComponent recognizes the letters-plus-number suffix used by
// temporary test directories without treating arbitrary tokens as paths.
func isTempDirectoryComponent(part string) bool {
	digits := len(part)
	for digits > 0 && part[digits-1] >= '0' && part[digits-1] <= '9' {
		digits--
	}
	if digits < 24 || len(part)-digits < 4 {
		return false
	}
	for i := 0; i < digits; i++ {
		if !(part[i] >= 'A' && part[i] <= 'Z' || part[i] >= 'a' && part[i] <= 'z') {
			return false
		}
	}
	return true
}

// isPlainWord: letters, digits, -, _ and . that do not look like a token. A
// token mixes upper, lower case and digits, turns from lower to upper case
// inside a word (base64 such as dXNl; OAuth and NTLM are words), or holds a
// run of 12 hex characters, 12 letters and digits mixed, or 16 letters.
func isPlainWord(value string) bool {
	upper, lower, digit, ok := wordClasses(value)
	if !ok || upper && lower && digit {
		return false
	}
	for _, run := range strings.FieldsFunc(value, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		if len(run) >= 16 || len(run) >= 12 && (isHex(run) || strings.ContainsAny(run, "0123456789") && !onlyDigits(run)) {
			return false
		}
	}
	return true
}

// wordClasses reports the character classes of a word; ok is false for an empty
// word, another character, or a turn from lower to upper case.
func wordClasses(value string) (upper, lower, digit, ok bool) {
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'A' && c <= 'Z':
			upper = true
			if i > 0 && value[i-1] >= 'a' && value[i-1] <= 'z' {
				return upper, lower, digit, false
			}
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= '0' && c <= '9':
			digit = true
		case c != '-' && c != '_' && c != '.':
			return upper, lower, digit, false
		}
	}
	return upper, lower, digit, value != ""
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f' || s[i] >= 'A' && s[i] <= 'F') {
			return false
		}
	}
	return true
}

func secretName(name string) (string, bool) {
	n := strings.ToLower(name)
	for len(n) > 0 && n[len(n)-1] >= '0' && n[len(n)-1] <= '9' {
		n = n[:len(n)-1]
	}
	for _, suffix := range [...]string{"password", "passwd", "passphrase"} {
		if strings.HasSuffix(n, suffix) {
			return "password", true
		}
	}
	if strings.HasSuffix(n, "pwd") && name != "PWD" && name != "OLDPWD" {
		return "password", true
	}
	if authName(n) {
		return "token", true
	}
	if n == "pass" || strings.HasSuffix(n, "_pass") || strings.HasSuffix(n, "-pass") {
		return "password", true
	}
	for _, suffix := range [...]string{"token", "secret", "api_key", "api-key", "apikey", "access_key", "private_key"} {
		if strings.HasSuffix(n, suffix) {
			return "token", true
		}
	}
	if strings.Contains(n, "secret") && strings.HasSuffix(n, "key") {
		return "token", true
	}
	return "", false
}

func authName(name string) bool {
	return name == "auth" || name == "credentials" || strings.HasSuffix(name, "_auth") || strings.HasSuffix(name, "-auth") || strings.HasSuffix(name, "_credentials") || strings.HasSuffix(name, "-credentials")
}

func replaceMatches(text string, indices [][]int, kind string, m *masking) string {
	if len(indices) == 0 {
		return text
	}
	spans := make([]contextSpan, 0, len(indices))
	for _, index := range indices {
		spans = append(spans, contextSpan{index[0], index[1], kind})
	}
	return replaceSpans(text, spans, m)
}

func replaceSpans(text string, spans []contextSpan, m *masking) string {
	if len(spans) == 0 {
		return text
	}
	var out strings.Builder
	last := 0
	for _, span := range spans {
		out.WriteString(text[last:span.start])
		last = span.end
		m.found[span.kind]++
		if m.tagger != nil && m.isPlaceholder(text[span.start:span.end]) {
			// A rule that reads a marker as a value hides what is hidden already.
			out.WriteString(text[span.start:span.end])
			continue
		}
		out.WriteString(maskMarker)
		out.WriteString(span.kind)
		if m.tagger != nil {
			out.WriteString(m.ph.value)
			out.WriteString(strconv.Itoa(len(m.values)))
			m.values = append(m.values, m.original(text[span.start:span.end]))
		}
		out.WriteByte(']')
	}
	out.WriteString(text[last:])
	return out.String()
}

func findTokenMatches(text string, p pattern, scan *scanIndex) ([][]int, *scanIndex) {
	var accepted [][]int
	var bodyEnds [4]bodyEndCache
	heads := prefixHeads(p.variants)
	for offset := 0; offset < len(text); {
		offset = nextPrefixHead(text, offset, heads)
		if offset < 0 {
			break
		}
		variant, matched := matchingPrefix(text, offset, p.variants)
		if !matched {
			offset++
			continue
		}
		body := offset + len(variant.prefix)
		end := bodyEnd(text, body, variant.body, &bodyEnds)
		if end-body < variant.minLength-len(variant.prefix) {
			offset++
			continue
		}
		if scan == nil {
			scan = &scanIndex{}
		}
		if scan.runs == nil {
			scan.runs = newRunIndex(text)
		}
		if acceptsToken(text, offset, body, end, p, scan.runs) {
			accepted = append(accepted, []int{offset, end})
			offset = end
		} else {
			offset++
		}
	}
	return accepted, scan
}

// prefixHead is the start of a variant prefix and its next place in the text
// at or after the offset last asked; the search for each runs over the text once.
type prefixHead struct {
	head string
	at   int // -1: not searched yet; len(text): none left
}

func prefixHeads(variants []tokenVariant) []prefixHead {
	var heads []prefixHead
	for _, variant := range variants {
		head := variant.prefix[:min(3, len(variant.prefix))]
		known := false
		for _, h := range heads {
			known = known || h.head == head
		}
		if !known {
			heads = append(heads, prefixHead{head, -1})
		}
	}
	return heads
}

// nextPrefixHead returns the first place at or after offset where a variant
// prefix can start, or -1.
func nextPrefixHead(text string, offset int, heads []prefixHead) int {
	next := -1
	for i := range heads {
		h := &heads[i]
		if h.at < offset {
			h.at = len(text)
			if at := strings.Index(text[offset:], h.head); at >= 0 {
				h.at = offset + at
			}
		}
		if h.at < len(text) && (next < 0 || h.at < next) {
			next = h.at
		}
	}
	return next
}

func matchingPrefix(text string, start int, variants []tokenVariant) (tokenVariant, bool) {
	for _, variant := range variants {
		if strings.HasPrefix(text[start:], variant.prefix) {
			return variant, true
		}
	}
	return tokenVariant{}, false
}

func acceptsToken(text string, start, body, end int, p pattern, runs *runIndex) bool {
	if p.acceptance == acceptGenericSk {
		if runs.has(body, end, genericAnyRun) {
			return true
		}
		return (boundaryBefore(text, start) || start > 0 && text[start-1] == '_') && runs.hasDigit(start, end)
	}
	if p.acceptance == acceptAlsoStandingAlone && standsAlone(text, start, end) {
		return true
	}
	return runs.hasDigit(start, end) && (boundaryBefore(text, start) || runs.has(start, end, 16))
}

// standsAlone: nothing joins the token to a longer one. Unlike boundaryBefore,
// - and _ do not count as an edge: they make a longer name, or a real key may
// start inside this match and its body must not be left behind the cut.
func standsAlone(text string, start, end int) bool {
	return boundaryBefore(text, start) && (start == 0 || !isJoiner(text[start-1])) && (end == len(text) || !isJoiner(text[end]))
}

func isJoiner(c byte) bool { return c == '-' || c == '_' }

type scanIndex struct {
	runs *runIndex
}

type bodyEndCache struct {
	start, end int
	valid      bool
}

func bodyEnd(text string, start int, class bodyClass, caches *[4]bodyEndCache) int {
	cached := &caches[class]
	if cached.valid && start >= cached.start && start < cached.end {
		return cached.end
	}
	end := start
	for end < len(text) && bodyContains(class, text[end]) {
		end++
	}
	if !cached.valid || start >= cached.start {
		*cached = bodyEndCache{start: start, end: end, valid: true}
	}
	return end
}

func bodyContains(class bodyClass, c byte) bool {
	if isAlphaNum(c) {
		return true
	}
	switch class {
	case alphaUnderscore:
		return c == '_'
	case alphaDash:
		return c == '-'
	case tokenChars:
		return c == '_' || c == '-'
	default:
		return false
	}
}

type runIndex struct {
	starts, ends    []int
	letters, digits []int
	next16, next20  []int
}

func newRunIndex(text string) *runIndex {
	r := &runIndex{letters: make([]int, len(text)+1), digits: make([]int, len(text)+1)}
	for i := 0; i < len(text); i++ {
		c := text[i]
		r.letters[i+1], r.digits[i+1] = r.letters[i], r.digits[i]
		if isLetter(c) {
			r.letters[i+1]++
		}
		if c >= '0' && c <= '9' {
			r.digits[i+1]++
		}
	}
	for start := 0; start < len(text); {
		if !isAlphaNum(text[start]) {
			start++
			continue
		}
		end := start
		for end < len(text) && isAlphaNum(text[end]) {
			end++
		}
		r.starts = append(r.starts, start)
		r.ends = append(r.ends, end)
		start = end
	}
	r.next16, r.next20 = make([]int, len(r.ends)+1), make([]int, len(r.ends)+1)
	r.next16[len(r.ends)], r.next20[len(r.ends)] = len(r.ends), len(r.ends)
	for i := len(r.ends) - 1; i >= 0; i-- {
		good := func(n int) bool {
			return r.ends[i]-r.starts[i] >= n && r.letters[r.ends[i]] > r.letters[r.starts[i]] && r.digits[r.ends[i]] > r.digits[r.starts[i]]
		}
		r.next16[i], r.next20[i] = r.next16[i+1], r.next20[i+1]
		if good(16) {
			r.next16[i] = i
		}
		if good(20) {
			r.next20[i] = i
		}
	}
	return r
}

func (r *runIndex) hasDigit(start, end int) bool {
	return r.digits[end] > r.digits[start]
}

func (r *runIndex) has(start, end, minimum int) bool {
	i := sort.Search(len(r.ends), func(i int) bool { return r.ends[i] >= start+minimum })
	if i == len(r.ends) {
		return false
	}
	if r.hasRunPart(i, start, end, minimum) {
		return true
	}
	if minimum >= 20 {
		i = r.next20[i+1]
	} else {
		i = r.next16[i+1]
	}
	if i < len(r.ends) && r.starts[i] >= start && r.ends[i] <= end {
		return true
	}
	last := sort.Search(len(r.starts), func(i int) bool { return r.starts[i] > end }) - 1
	return last >= 0 && last < len(r.ends) && r.hasRunPart(last, start, end, minimum)
}

func (r *runIndex) hasRunPart(i, start, end, minimum int) bool {
	lo, hi := max(start, r.starts[i]), min(end, r.ends[i])
	return hi-lo >= minimum && r.letters[hi] > r.letters[lo] && r.digits[hi] > r.digits[lo]
}

// genericAnyRun is the random run that lets a generic key be masked after any
// neighbour: the shortest generic body (20) is a single run of this length.
const genericAnyRun = 20

func isAlphaNum(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

func isLetter(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func boundaryBefore(text string, index int) bool {
	if index == 0 {
		return true
	}
	c := text[index-1]
	return !(isLetter(c) || c >= '0' && c <= '9') || escapeBefore(text, index)
}

func escapeBefore(text string, index int) bool {
	start := max(index-32, 0)
	return escapeRE.MatchString(text[start:index])
}

// notSecretHere keeps forms of ordinary output that look like a name and a value:
// test results (PASS: go, pass=0), a message of the passwd tool, a file path before
// a colon. A value mixing letters and digits after PASS: stays hidden: test words
// and status codes do not look like that.
func notSecretHere(text string, at int, name, delimiter, value string) bool {
	if delimiter == ":" && startsFilePath(text, at) {
		return true
	}
	switch {
	case name == "PASS" && delimiter == ":":
		return !strings.ContainsAny(value, "0123456789") || onlyDigits(value) && len(value) < 8
	case strings.EqualFold(name, "pass"):
		return delimiter == "=" && onlyDigits(value) && len(value) < 8
	case strings.EqualFold(name, "passwd"):
		return delimiter == ":" && value == "password"
	}
	return false
}

// startsFilePath: the word ending at `at` is an absolute or relative path. The look
// back is bounded, a path is short and many names must stay linear.
func startsFilePath(text string, at int) bool {
	if at == 0 || text[at-1] != '/' {
		return false
	}
	start := at - 1
	for start > 0 && at-start < 256 && !strings.ContainsRune(" \t\r\n'\"", rune(text[start-1])) {
		start--
	}
	word := text[start:at]
	return word[0] == '/' || strings.HasPrefix(word, "./") || strings.HasPrefix(word, "../") || strings.HasPrefix(word, "~/")
}

// hasFlagSpaceAt: a flag is followed by a space or a line continuation.
func hasFlagSpaceAt(text string, at int) bool {
	return hasHorizontalSpaceAt(text, at) || strings.HasPrefix(text[at:], "\\\n") || literalSpaceAt(text, at) > 0
}

func curlMatches(text string, segments [][2]int) []contextMatch {
	var matches []contextMatch
	for _, segment := range segments {
		part := text[segment[0]:segment[1]]
		command, ok := findCommandWord(part, "curl", false)
		if !ok {
			continue
		}
		words := wordsAfter(part, command.end)
		for {
			start, end, ok := words.next()
			if !ok {
				break
			}
			valueAt, found := curlUserValue(part, start, end, words)
			if !found {
				continue
			}
			if password, ok := curlPassword(part, valueAt); ok {
				matches = append(matches, contextMatch{segment[0] + password.start, segment[0] + password.end, "password"})
			}
		}
	}
	return matches
}

func curlUserValue(text string, start, end int, words *shellWords) (int, bool) {
	token := text[start:end]
	if strings.HasPrefix(token, "--user=") {
		return start + len("--user="), true
	}
	if token == "--user" {
		value, _, ok := words.next()
		return value, ok
	}
	if !strings.HasPrefix(token, "-") || strings.HasPrefix(token, "--") {
		return 0, false
	}
	body := token[1:]
	u := strings.IndexByte(body, 'u')
	if u < 0 || !onlyLetters(body[:u]) {
		return 0, false
	}
	after := start + 1 + u + 1
	if after < end && text[after] == '=' {
		return after + 1, true
	}
	if after < end {
		return after, true
	}
	value, _, ok := words.next()
	return value, ok
}

// curlPassword finds the part after the first colon of user:password, in one
// value or split by quotes: admin:'x' and 'admin':'x'.
func curlPassword(text string, at int) (contextValue, bool) {
	value, ok := scanValue(text, at, false)
	if !ok || strings.Contains(text[value.start:value.end], maskMarker) {
		return contextValue{}, false
	}
	password := contextValue{}
	colon := strings.IndexByte(text[value.start:value.end], ':')
	switch {
	case colon >= 0 && value.start+colon+1 < value.end:
		passwordAt := value.start + colon + 1
		if text[passwordAt] == '"' || text[passwordAt] == '\\' {
			password, ok = scanValue(text, passwordAt, false)
		} else {
			password = contextValue{passwordAt, value.end, value.quoted}
		}
	case colon >= 0 && !value.quoted && value.end < len(text) && (text[value.end] == '\'' || text[value.end] == '"'):
		password, ok = scanValue(text, value.end, false)
	case colon < 0 && value.quoted:
		after := value.end
		for after < len(text) && text[after] == '\\' {
			after++
		}
		if after+1 >= len(text) || text[after+1] != ':' {
			return contextValue{}, false
		}
		password, ok = scanValue(text, after+2, false)
	default:
		return contextValue{}, false
	}
	return password, ok && valueIsValue(text, password, "", "password", false)
}

func toolPasswordMatches(text string, segments [][2]int) []contextMatch {
	var matches []contextMatch
	for _, segment := range segments {
		part := text[segment[0]:segment[1]]
		sshpassMatches(part, segment[0], &matches)
		mysqlMatches(part, "mysql", segment[0], &matches)
		mysqlMatches(part, "mariadb", segment[0], &matches)
		ghTokenMatches(part, segment[0], &matches)
		optionPasswordMatches(part, segment[0], "redis-cli", "-a", &matches)
		optionPasswordMatches(part, segment[0], "mongo", "-p", &matches)
		dockerPasswordMatches(part, segment[0], &matches)
		htpasswdMatches(part, segment[0], &matches)
		sudoStdinMatches(text, segment, &matches)
	}
	return matches
}

// sudoStdinMatches hides the word echo hands to sudo -S, which reads the
// password from its input: echo VALUE | sudo -S cmd. Echo is the command right
// before the pipe, with no command separator between them.
func sudoStdinMatches(text string, segment [2]int, matches *[]contextMatch) {
	part := text[segment[0]:segment[1]]
	command, ok := findCommandWord(part, "sudo", false)
	if !ok || !sudoReadsStdin(wordsAfter(part, command.end), part) {
		return
	}
	pipe := skipJSONSpaceBack(text, segment[0]+command.start) - 1
	if pipe < 0 || text[pipe] != '|' || pipe > 0 && text[pipe-1] == '|' {
		return
	}
	from := max(pipe-1024, 0)
	for i := pipe - 1; i >= from; i-- {
		if strings.ContainsRune(";&|\n\r", rune(text[i])) || i > 0 && text[i-1] == '\\' && (text[i] == 'n' || text[i] == 'r') {
			from = i + 1
			break
		}
	}
	command, ok = findCommandWord(text[from:pipe], "echo", false)
	if !ok {
		return
	}
	echo := text[from:pipe]
	words := wordsAfter(echo, command.end)
	for {
		start, end, ok := words.next()
		if !ok {
			return
		}
		if !strings.HasPrefix(echo[start:end], "-") {
			value, ok := scanValue(echo, start, false)
			if ok && valueIsValue(echo, value, "", "password", false) {
				*matches = append(*matches, contextMatch{from + value.start, from + value.end, "password"})
			}
			return
		}
	}
}

// sudoReadsStdin: the options of sudo, before its command, hold -S or --stdin.
// The options taking a value skip it.
func sudoReadsStdin(words *shellWords, part string) bool {
	for {
		start, end, ok := words.next()
		if !ok {
			return false
		}
		word := part[start:end]
		switch {
		case word == "--stdin":
			return true
		case word == "--":
			return false
		case strings.HasPrefix(word, "--"):
			continue
		case len(word) > 1 && word[0] == '-' && onlyLetters(word[1:]):
			if strings.ContainsRune(word, 'S') {
				return true
			}
			if strings.ContainsRune("ugpCDhrtTU", rune(word[len(word)-1])) {
				words.next()
			}
		default:
			return false
		}
	}
}

func ghTokenMatches(part string, offset int, matches *[]contextMatch) {
	command, ok := findCommandWord(part, "gh", false)
	if !ok {
		return
	}
	words := wordsAfter(part, command.end)
	for {
		start, end, ok := words.next()
		if !ok {
			return
		}
		if part[start:end] != "--with-token" {
			continue
		}
		marker, _, ok := words.next()
		if !ok || marker+3 > len(part) || part[marker:marker+3] != "<<<" {
			return
		}
		valueAt, _, ok := words.next()
		if ok {
			addToolPassword(part, offset, valueAt, matches)
		}
		return
	}
}

func dockerPasswordMatches(part string, offset int, matches *[]contextMatch) {
	command, ok := findCommandWord(part, "docker", false)
	if !ok {
		return
	}
	words := wordsAfter(part, command.end)
	start, _, ok := words.next()
	if !ok || part[start:words.at] != "login" {
		return
	}
	optionPasswordMatches(part[words.at:], offset+words.at, "", "-p", matches)
}

func htpasswdMatches(part string, offset int, matches *[]contextMatch) {
	command, ok := findCommandWord(part, "htpasswd", false)
	if !ok {
		return
	}
	words := wordsAfter(part, command.end)
	for {
		start, end, ok := words.next()
		if !ok {
			return
		}
		token := part[start:end]
		if token != "-b" {
			continue
		}
		_, _, ok = words.next() // file
		if !ok {
			return
		}
		_, _, ok = words.next() // user
		if !ok {
			return
		}
		valueAt, _, ok := words.next()
		if ok {
			addToolPassword(part, offset, valueAt, matches)
		}
		return
	}
}

func optionPasswordMatches(part string, offset int, tool, option string, matches *[]contextMatch) {
	commandEnd := 0
	if tool != "" {
		command, ok := findCommandWord(part, tool, false)
		if !ok {
			return
		}
		commandEnd = command.end
	}
	words := wordsAfter(part, commandEnd)
	for {
		start, end, ok := words.next()
		if !ok {
			return
		}
		token := part[start:end]
		if token == option {
			if valueAt, _, ok := words.next(); ok {
				addToolPassword(part, offset, valueAt, matches)
			}
			return
		}
		if strings.HasPrefix(token, option) && len(token) > len(option) {
			addToolPassword(part, offset, start+len(option), matches)
			return
		}
	}
}

func curlExtraMatches(text string, segments [][2]int) []contextMatch {
	var matches []contextMatch
	for _, segment := range segments {
		part := text[segment[0]:segment[1]]
		command, ok := findCommandWord(part, "curl", false)
		if !ok {
			continue
		}
		words := wordsAfter(part, command.end)
		for {
			start, end, ok := words.next()
			if !ok {
				break
			}
			token := part[start:end]
			at, valueEnd, mode, ok := curlExtraValue(token, start, &words)
			if !ok {
				continue
			}
			if mode == 'h' {
				matches = append(matches, curlHeaderOptionMatches(part, segment[0], at, valueEnd)...)
				continue
			}
			value, valid := scanValue(part, at, false)
			if !valid {
				continue
			}
			if mode == 'b' {
				cookie := part[value.start:value.end]
				if !strings.Contains(cookie, "=") || strings.Contains(cookie, "/") {
					continue
				}
				if eq := strings.IndexByte(cookie, '='); eq >= 0 && eq+1 < len(cookie) {
					value.start += eq + 1
				}
			}
			if valueIsValue(part, value, "token", "token", false) {
				matches = append(matches, contextMatch{segment[0] + value.start, segment[0] + value.end, "token"})
			}
		}
	}
	return matches
}

func curlHeaderOptionMatches(part string, offset, at, end int) []contextMatch {
	if end == 0 {
		header, ok := scanValue(part, at, false)
		if !ok {
			return nil
		}
		end = header.end
	}
	return cookieHeaderMatches(part, offset, at, end)
}

func curlExtraValue(token string, start int, words **shellWords) (int, int, byte, bool) {
	switch {
	case token == "--oauth2-bearer" || token == "-b" || token == "--cookie":
		at, end, ok := (*words).next()
		if token == "-b" || token == "--cookie" {
			return at, end, 'b', ok
		}
		return at, end, 't', ok
	case strings.HasPrefix(token, "--oauth2-bearer="):
		return start + len("--oauth2-bearer="), 0, 't', true
	case strings.HasPrefix(token, "-b") && len(token) > 2:
		return start + 2, 0, 'b', true
	case strings.HasPrefix(token, "--cookie="):
		return start + len("--cookie="), 0, 'b', true
	case token == "-H" || token == "--header":
		at, end, ok := (*words).next()
		return at, end, 'h', ok
	case strings.HasPrefix(token, "-H") && len(token) > 2:
		return start + 2, 0, 'h', true
	case strings.HasPrefix(token, "--header="):
		return start + len("--header="), 0, 'h', true
	default:
		return 0, 0, 0, false
	}
}

func cookieHeaderMatches(part string, offset, start, end int) []contextMatch {
	value := part[start:end]
	colon := strings.IndexByte(value, ':')
	if colon < 0 || !strings.EqualFold(strings.Trim(value[:colon], " \t\r\n'\"`"), "Cookie") {
		return nil
	}
	eq := strings.IndexByte(value[colon+1:], '=')
	if eq < 0 {
		return nil
	}
	at := start + colon + 1 + eq + 1
	endAt := end
	if endAt > at && (part[endAt-1] == '\'' || part[endAt-1] == '"') {
		endAt--
	}
	if at >= endAt {
		return nil
	}
	return []contextMatch{{offset + at, offset + endAt, "token"}}
}

func mysqlMatches(part, tool string, offset int, matches *[]contextMatch) {
	command, ok := findCommandWord(part, tool, true)
	if !ok {
		return
	}
	words := wordsAfter(part, command.end)
	for {
		start, end, ok := words.next()
		if !ok {
			return
		}
		if strings.HasPrefix(part[start:end], "-p") && end-start > 2 {
			addToolPassword(part, offset, start+2, matches)
		}
	}
}

func sshpassMatches(part string, offset int, matches *[]contextMatch) {
	command, ok := findCommandWord(part, "sshpass", false)
	if !ok {
		return
	}
	words := wordsAfter(part, command.end)
	for {
		start, end, ok := words.next()
		if !ok || !strings.HasPrefix(part[start:end], "-") {
			return
		}
		token := part[start:end]
		switch {
		case token == "-p":
			if valueAt, _, ok := words.next(); ok {
				addToolPassword(part, offset, valueAt, matches)
			}
			return
		case strings.HasPrefix(token, "-p"):
			addToolPassword(part, offset, start+2, matches)
			return
		case token == "-f" || token == "-d" || token == "-P":
			words.next()
		}
	}
}

// quoteSpans lists closed quotes from left to right, so a quote inside another is
// data. A quote without a pair is an ordinary character. Each form is searched to
// the end of the text at most once.
func quoteSpans(text string) [][2]int {
	type quoteForm struct {
		quote   byte
		slashes int
	}
	var spans [][2]int
	unclosed := map[quoteForm]bool{}
	for i := 0; i < len(text); i++ {
		if text[i] != '\'' && text[i] != '"' && text[i] != '`' {
			continue
		}
		start := i
		for start > 0 && text[start-1] == '\\' {
			start--
		}
		form := quoteForm{text[i], i - start}
		if !opensQuote(text[i], form.slashes) || unclosed[form] {
			continue
		}
		end := closingQuote(text, i, form.slashes)
		if end < 0 {
			unclosed[form] = true
			continue
		}
		spans = append(spans, [2]int{start, end + 1})
		i = end
	}
	return spans
}

// opensQuote: a backslash before an apostrophe only escapes it; before a double
// quote backslashes are the quote of a string nested in a string. The limit keeps
// the number of quote forms, each searched once, small.
func opensQuote(quote byte, slashes int) bool {
	if quote != '"' {
		return slashes == 0
	}
	return slashes <= maxQuoteSlashes
}

// closingQuote returns the index of the quote closing the one at quoteAt, or -1.
// Apostrophes have no escapes. A double quote at its nesting level closes with an
// even number of escaped backslashes before it: each such backslash takes
// slashes+1 characters, and an odd one would escape the quote itself.
func closingQuote(text string, quoteAt, slashes int) int {
	quote := text[quoteAt]
	for i := quoteAt + 1; i < len(text); i++ {
		if text[i] != quote {
			continue
		}
		if quote != '"' {
			return i
		}
		count := 0
		for i-count-1 > quoteAt && text[i-count-1] == '\\' {
			count++
		}
		extra := count - slashes
		if extra >= 0 && extra%(slashes+1) == 0 && extra/(slashes+1)%2 == 0 {
			return i
		}
	}
	return -1
}

// shellWords walks the words after a command. Quotes are counted from the end of
// the command word: a command inside a quote ends at that quote's closing mark,
// which then has no pair and is an ordinary character.
type shellWords struct {
	text   string
	at     int
	base   int
	quotes [][2]int
}

func wordsAfter(text string, at int) *shellWords {
	return &shellWords{text: text, at: at, base: at, quotes: quoteSpans(text[at:])}
}

func (w *shellWords) next() (int, int, bool) {
	text, at := w.text, w.at
	for at < len(text) {
		if text[at] == ' ' || text[at] == '\t' || text[at] == '\n' || text[at] == '\r' {
			at++
			continue
		}
		if strings.HasPrefix(text[at:], "\\\n") {
			at += 2
			continue
		}
		if n := literalSpaceAt(text, at); n > 0 {
			at += n
			continue
		}
		break
	}
	w.at = at
	if at >= len(text) {
		return 0, 0, false
	}
	start := at
	for at < len(text) {
		for len(w.quotes) > 0 && at >= w.base+w.quotes[0][1] {
			w.quotes = w.quotes[1:]
		}
		if len(w.quotes) > 0 && at >= w.base+w.quotes[0][0] {
			at = w.base + w.quotes[0][1]
			continue
		}
		if c := text[at]; c == ' ' || c == '\t' || c == '\n' || c == '\r' || strings.HasPrefix(text[at:], "\\\n") || literalSpaceAt(text, at) > 0 {
			break
		}
		at++
	}
	w.at = at
	return start, at, true
}

func onlyLetters(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isLetter(s[i]) {
			return false
		}
	}
	return true
}

func onlyDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
