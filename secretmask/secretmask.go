// Package secretmask hides secret values in text before it goes further: to a
// log, a report or a model of another provider. It replaces each match with
// [скрыто:<kind>] and counts matches by kind; it never returns a value.
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
//   - Command context detection misses tool-specific options such as
//     redis-cli -a, Cookie headers, a bare Bearer value, non-English key names,
//     and references such as $ecret. An unquoted value can end at a quote and
//     leave a tail visible (for example PASSWORD=a'b').
//   - Context rules can hide prose that resembles an assignment or option,
//     including “--token is no longer needed” and “password: word”.
package secretmask

import (
	"regexp"
	"sort"
	"strings"
)

var (
	privateKeyRE   = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----|\z)`)
	awsRE          = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	escapeRE       = regexp.MustCompile(`(?:\\[A-Za-z]|\\[0-7]{1,3}|\\x[0-9A-Fa-f]{2}|\\u[0-9A-Fa-f]{4}|\\u\{[0-9A-Fa-f]+\}|\\U[0-9A-Fa-f]{8}|%(?:25)*[0-9A-Fa-f]{2}|=[0-9A-F]{2}|&#[0-9]+;?|&#x[0-9A-Fa-f]+;?|(?:\x1b|\\[eE]|\\033|\\33|\\x1[bB]|\\u001[bB]|\^\[|␛)\[[0-9;?]*[A-Za-z]|\x1b\(B|\[[0-9;]+m)$`)
	contextNameRE  = regexp.MustCompile(`(?i)([a-z0-9_.-]+)(?:\\*["'])?([ \t]*)(:=|=>|=|:)`)
	headerRE       = regexp.MustCompile(`(?im)(?:proxy-)?authorization[ \t]*[:=][ \t]*(?:(?:bearer|basic|token|digest|negotiate)(?:[ \t]+|\r?$))?`)
	urlRE          = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s/:@]*:([^\s/@]+)@`)
	flagRE         = regexp.MustCompile("(?i)(?:^|[ \\t'\"`])--?([a-z0-9_.-]+)(?:=|(?:[ \\t]|\\\\\\n)+)")
	contextAnchors = map[byte][]string{
		'a': {"authorization", "api_key", "api-key", "apikey", "access_key"},
		'c': {"curl"}, 'm': {"mysql", "mariadb"},
		'p': {"password", "passwd", "passphrase", "pass", "pwd", "private_key"},
		's': {"secret", "sshpass"}, 't': {"token"},
	}
)

type contextAnchorState struct {
	next  [128]int
	fail  int
	match bool
}

var contextAnchorAutomaton = buildContextAnchorAutomaton()

func buildContextAnchorAutomaton() []contextAnchorState {
	states := []contextAnchorState{{}}
	firstBytes := make([]int, 0, len(contextAnchors))
	for first := range contextAnchors {
		firstBytes = append(firstBytes, int(first))
	}
	sort.Ints(firstBytes)
	for _, first := range firstBytes {
		words := contextAnchors[byte(first)]
		for _, word := range words {
			state := 0
			for i := 0; i < len(word); i++ {
				c := word[i]
				if states[state].next[c] == 0 {
					states = append(states, contextAnchorState{})
					states[state].next[c] = len(states) - 1
				}
				state = states[state].next[c]
			}
			states[state].match = true
		}
	}
	queue := make([]int, 0, len(states))
	for c := range 128 {
		if child := states[0].next[c]; child != 0 {
			queue = append(queue, child)
		}
	}
	for head := 0; head < len(queue); head++ {
		state := queue[head]
		for c := range 128 {
			child := states[state].next[c]
			if child == 0 {
				continue
			}
			queue = append(queue, child)
			failure := states[state].fail
			for failure != 0 && states[failure].next[c] == 0 {
				failure = states[failure].fail
			}
			states[child].fail = states[failure].next[c]
			states[child].match = states[child].match || states[states[child].fail].match
		}
	}
	return states
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
)

type pattern struct {
	kind       string
	variants   []tokenVariant
	acceptance acceptance
	firstBytes string
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
	{kind: "sk_live", variants: []tokenVariant{{"sk_live_", alphaNum, 24}, {"sk_test_", alphaNum, 24}, {"rk_live_", alphaNum, 24}, {"rk_test_", alphaNum, 24}, {"pk_live_", alphaNum, 24}, {"pk_test_", alphaNum, 24}}, acceptance: acceptAlsoStandingAlone, firstBytes: "srp"},
	{kind: "sk", variants: []tokenVariant{{"sk-proj-", tokenChars, 28}, {"sk-ant-", tokenChars, skAntMinimumLength}}, acceptance: acceptDigitAndRun, firstBytes: "s"},
	{kind: "github", variants: []tokenVariant{{"github_pat_", alphaUnderscore, githubPatMinimumLength}}, acceptance: acceptDigitAndRun, firstBytes: "g"},
	{kind: "github", variants: []tokenVariant{{"ghp_", alphaNum, githubTokenMinimumLength}, {"gho_", alphaNum, githubTokenMinimumLength}, {"ghu_", alphaNum, githubTokenMinimumLength}, {"ghs_", alphaNum, githubTokenMinimumLength}, {"ghr_", alphaNum, githubTokenMinimumLength}}, acceptance: acceptAlsoStandingAlone, firstBytes: "g"},
	{kind: "slack", variants: []tokenVariant{{"xoxa-", alphaDash, 15}, {"xoxb-", alphaDash, 15}, {"xoxe-", alphaDash, 15}, {"xoxo-", alphaDash, 15}, {"xoxp-", alphaDash, 15}, {"xoxr-", alphaDash, 15}, {"xoxs-", alphaDash, 15}}, acceptance: acceptDigitAndRun, firstBytes: "x"},
	{kind: "aws", re: awsRE},
	{kind: "google", variants: []tokenVariant{{"AIza", tokenChars, googleTokenMinimumLength}}, acceptance: acceptAlsoStandingAlone, firstBytes: "A"},
	{kind: "sk", variants: []tokenVariant{{"sk-", tokenChars, 23}, {"sk_", tokenChars, 23}}, acceptance: acceptGenericSk, firstBytes: "s"},
}

// Mask replaces every secret in text with [скрыто:<kind>] and returns the text
// and the number of matches by kind (empty, not nil, when there are none).
func Mask(text string) (string, map[string]int) {
	found := map[string]int{}
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
			text = replaceMatches(text, indices, p.kind, found)
			scan = nil // replacements invalidate byte offsets and run metadata
		}
	}
	if hasContextAnchor(text) || strings.Contains(text, "://") {
		text = maskContext(text, found)
	}
	return text, found
}

func hasContextAnchor(text string) bool {
	state := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		if c >= 128 {
			state = 0
			continue
		}
		for state != 0 && contextAnchorAutomaton[state].next[c] == 0 {
			state = contextAnchorAutomaton[state].fail
		}
		state = contextAnchorAutomaton[state].next[c]
		if contextAnchorAutomaton[state].match {
			return true
		}
	}
	return false
}

// Context rules recognize values after familiar command and configuration names.
func maskContext(text string, found map[string]int) string {
	matches := make([]contextMatch, 0)
	matches = append(matches, headerMatches(text)...)
	matches = append(matches, nameMatches(text)...)
	matches = append(matches, flagMatches(text)...)
	matches = append(matches, urlMatches(text)...)
	matches = append(matches, curlMatches(text)...)
	matches = append(matches, toolPasswordMatches(text)...)
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
	return replaceSpans(text, spans, found)
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

func nameMatches(text string) []contextMatch {
	var matches []contextMatch
	coveredUntil := 0
	for _, ix := range contextNameRE.FindAllStringSubmatchIndex(text, -1) {
		if ix[0] < coveredUntil || withinMaskMarker(text, ix[0]) || withinMaskMarker(text, ix[6]) {
			continue
		}
		name := text[ix[2]:ix[3]]
		kind, ok := secretName(name)
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
		coveredUntil = value.end
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

func flagMatches(text string) []contextMatch {
	var matches []contextMatch
	coveredUntil := 0
	for _, ix := range flagRE.FindAllStringSubmatchIndex(text, -1) {
		if ix[0] < coveredUntil {
			continue
		}
		name := text[ix[2]:ix[3]]
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "no-") || strings.HasPrefix(lower, "ask-") || strings.HasPrefix(lower, "prompt-") {
			continue
		}
		kind, ok := secretName(name)
		if !ok {
			continue
		}
		at := ix[1]
		value, ok := scanValue(text, at, false)
		if !ok {
			continue
		}
		coveredUntil = value.end
		separator := text[ix[1]-1]
		if separator == '=' && ix[1] < len(text) && hasHorizontalSpaceAt(text, ix[1]) {
			continue
		}
		if separator != '=' && !flagValueAllowed(text[value.start:value.end]) {
			continue
		}
		if valueIsValue(text, value, name, kind, false) {
			matches = append(matches, contextMatch{value.start, value.end, kind})
		}
	}
	return matches
}

func urlMatches(text string) []contextMatch {
	var matches []contextMatch
	for _, ix := range urlRE.FindAllStringSubmatchIndex(text, -1) {
		if withinMaskMarker(text, ix[2]) {
			continue
		}
		value, ok := scanValue(text, ix[2], false)
		if !ok {
			continue
		}
		if value.end > ix[3] {
			value.end = ix[3]
		}
		if valueIsValue(text, value, "", "password", false) {
			matches = append(matches, contextMatch{value.start, value.end, "password"})
		}
	}
	return matches
}

func curlMatches(text string) []contextMatch {
	var matches []contextMatch
	for _, segment := range commandSegments(text) {
		part := text[segment[0]:segment[1]]
		quotes := pairedQuoteRanges(part)
		if command, ok := findCommandWord(part, "curl", false); ok {
			quoteIndex := 0
			for at := command.end; ; {
				start, end, ok := nextShellWord(part, at, quotes, &quoteIndex)
				if !ok {
					break
				}
				at = end
				valueAt, found := curlUserValue(part, start, end, quotes, quoteIndex)
				if !found {
					continue
				}
				value, ok := scanValue(part, valueAt, false)
				if !ok {
					continue
				}
				colon := strings.IndexByte(part[value.start:value.end], ':')
				if colon < 0 {
					continue
				}
				if strings.Contains(part[value.start:value.end], "[скрыто:") {
					continue
				}
				password := contextValue{value.start + colon + 1, value.end, value.quoted}
				if valueIsValue(part, password, "", "password", false) {
					matches = append(matches, contextMatch{segment[0] + password.start, segment[0] + password.end, "password"})
				}
			}
		}
	}
	return matches
}

func curlUserValue(text string, start, end int, quotes [][2]int, quoteIndex int) (int, bool) {
	token := text[start:end]
	if strings.HasPrefix(token, "--user=") {
		return start + len("--user="), true
	}
	if token == "--user" {
		value, _, ok := nextShellWord(text, end, quotes, &quoteIndex)
		return value, ok
	}
	if !strings.HasPrefix(token, "-") || strings.HasPrefix(token, "--") {
		return 0, false
	}
	body := token[1:]
	u := strings.IndexByte(body, 'u')
	if u < 0 {
		return 0, false
	}
	after := start + 1 + u + 1
	if after < end && text[after] == '=' {
		return after + 1, true
	}
	if after < end {
		return after, true
	}
	value, _, ok := nextShellWord(text, end, quotes, &quoteIndex)
	return value, ok
}

func toolPasswordMatches(text string) []contextMatch {
	var matches []contextMatch
	for _, segment := range commandSegments(text) {
		part := text[segment[0]:segment[1]]
		quotes := pairedQuoteRanges(part)
		quoteIndex := 0
		if command, ok := findCommandWord(part, "sshpass", false); ok {
			for at := command.end; ; {
				start, end, ok := nextShellWord(part, at, quotes, &quoteIndex)
				if !ok {
					break
				}
				token := part[start:end]
				at = end
				if !strings.HasPrefix(token, "-") {
					break
				}
				if token == "-p" {
					valueAt, _, ok := nextShellWord(part, end, quotes, &quoteIndex)
					if ok {
						addToolPassword(part, segment[0], valueAt, &matches)
					}
					break
				}
				if strings.HasPrefix(token, "-p") && len(token) > 2 {
					addToolPassword(part, segment[0], start+2, &matches)
					break
				}
				if token == "-f" || token == "-d" {
					_, at, _ = nextShellWord(part, at, quotes, &quoteIndex)
				}
			}
		}
		for _, tool := range [...]string{"mysql", "mariadb"} {
			command, ok := findCommandWord(part, tool, true)
			if !ok {
				continue
			}
			for at := command.end; ; {
				start, end, ok := nextShellWord(part, at, quotes, &quoteIndex)
				if !ok {
					break
				}
				at = end
				if strings.HasPrefix(part[start:end], "-p") && end-start > 2 {
					addToolPassword(part, segment[0], start+2, &matches)
				}
			}
		}
	}
	return matches
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
		if prefix || commandRightBoundary(text, end) {
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
	if at >= 2 && text[at-2:at] == "\\\n" {
		return true
	}
	return strings.ContainsRune(" \t/('\"`", rune(text[at-1]))
}

func commandRightBoundary(text string, at int) bool {
	return at == len(text) || strings.ContainsRune(" \t'\"`)", rune(text[at])) || strings.HasPrefix(text[at:], "\\\n")
}

func commandSegments(text string) [][2]int {
	quoted := pairedQuoteRanges(text)
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
	return append(segments, [2]int{start, len(text)})
}

func pairedQuoteRanges(text string) [][2]int {
	type quoteForm struct {
		quote   byte
		slashes int
	}
	pending := make(map[quoteForm]int)
	ranges := make([][2]int, 0)
	for i := 0; i < len(text); i++ {
		if text[i] != '\'' && text[i] != '"' && text[i] != '`' {
			continue
		}
		start := i
		for start > 0 && text[start-1] == '\\' {
			start--
		}
		form := quoteForm{text[i], i - start}
		if open, ok := pending[form]; ok {
			ranges = append(ranges, [2]int{open, i + 1})
			delete(pending, form)
		} else {
			pending[form] = start
		}
	}
	if len(ranges) < 2 {
		return ranges
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i][0] < ranges[j][0] })
	return ranges
}

func nextShellWord(text string, at int, quoted [][2]int, quoteIndex *int) (int, int, bool) {
	for at < len(text) {
		if text[at] == ' ' || text[at] == '\t' || text[at] == '\n' || text[at] == '\r' {
			at++
			continue
		}
		if strings.HasPrefix(text[at:], "\\\n") {
			at += 2
			continue
		}
		break
	}
	if at >= len(text) {
		return 0, 0, false
	}
	start := at
	quoteRange := *quoteIndex
	for quoteRange < len(quoted) && at >= quoted[quoteRange][1] {
		quoteRange++
	}
	*quoteIndex = quoteRange
	for at < len(text) {
		for quoteRange < len(quoted) && at >= quoted[quoteRange][1] {
			quoteRange++
		}
		if quoteRange < len(quoted) && at >= quoted[quoteRange][0] {
			at = quoted[quoteRange][1]
			quoteRange++
			*quoteIndex = quoteRange
			continue
		}
		c := text[at]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || strings.HasPrefix(text[at:], "\\\n") {
			break
		}
		at++
	}
	return start, at, true
}

func scanValue(text string, at int, colon bool) (contextValue, bool) {
	for at < len(text) {
		if text[at] == ' ' || text[at] == '\t' {
			at++
			continue
		}
		if strings.HasPrefix(text[at:], "\\\n") {
			at += 2
			continue
		}
		break
	}
	if at >= len(text) || strings.HasPrefix(text[at:], "[скрыто:") || text[at] == '\n' || text[at] == '\r' || text[at] == '=' || text[at] == '>' {
		return contextValue{}, false
	}
	quoteAt, quote := at, byte(0)
	if text[at] == '`' {
		return contextValue{}, false
	}
	if text[at] == '\'' || text[at] == '"' {
		quote = text[at]
	} else if text[at] == '\\' {
		i := at
		for i < len(text) && text[i] == '\\' {
			i++
		}
		if i < len(text) && text[i] == '"' {
			quoteAt, quote = i, '"'
		}
	}
	if quote != 0 {
		openStart := quoteAt
		for openStart > at && text[openStart-1] == '\\' {
			openStart--
		}
		slashes := quoteAt - openStart
		for i := quoteAt + 1; i < len(text); i++ {
			if text[i] != quote {
				continue
			}
			closeStart := i
			for closeStart > 0 && text[closeStart-1] == '\\' {
				closeStart--
			}
			if i-closeStart == slashes {
				return contextValue{quoteAt + 1, closeStart, true}, closeStart > quoteAt+1
			}
		}
		return contextValue{}, false
	}
	start := at
	for at < len(text) {
		c := text[at]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || strings.ContainsRune("\"'&;|)}`", rune(c)) || c == ',' && colon || strings.HasPrefix(text[at:], "\\\n") {
			break
		}
		at++
	}
	return contextValue{start, at, false}, at > start
}

func valueIsValue(text string, value contextValue, name, kind string, colon bool) bool {
	v := text[value.start:value.end]
	if value.quoted {
		return quotedIsValue(v)
	}
	return bareIsValue(v, name, kind, colon)
}

func quotedIsValue(value string) bool {
	return value != "" && !isReference(value) && !isPlaceholder(value) && !strings.Contains(value, "[скрыто:")
}

func bareIsValue(value, name, kind string, colon bool) bool {
	if value == "" || isReference(value) || isPlaceholder(value) || strings.Contains(value, "[скрыто:") || strings.ContainsAny(value, "('") || equalSecretName(value, name) {
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
		digits := true
		for i := 0; i < len(value); i++ {
			if value[i] < '0' || value[i] > '9' {
				digits = false
				break
			}
		}
		if digits && len(value) < 8 || colon && !digits && len(value) < 8 {
			return false
		}
	}
	return true
}

func withinMaskMarker(text string, at int) bool {
	if at < 0 || at > len(text) {
		return false
	}
	start := max(at-len("[скрыто:"), 0)
	open := strings.LastIndex(text[start:at], "[скрыто:")
	if open < 0 {
		return false
	}
	open += start
	close := strings.IndexByte(text[open:], ']')
	return close >= 0 && open+close >= at
}

func isReference(value string) bool {
	return strings.HasPrefix(value, "${") || strings.HasPrefix(value, "$(") || strings.HasPrefix(value, "`") || len(value) > 1 && value[0] == '$' && ((value[1] >= 'A' && value[1] <= 'Z') || (value[1] >= 'a' && value[1] <= 'z') || value[1] == '_') || strings.HasPrefix(value, `\"$`)
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

func flagValueAllowed(value string) bool { return value != "" && value[0] != '-' && value[0] < 0x80 }

func hasHorizontalSpaceAt(text string, at int) bool {
	return at < len(text) && (text[at] == ' ' || text[at] == '\t')
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
	if strings.HasSuffix(n, "pwd") && strings.ToUpper(name) != "PWD" && strings.ToUpper(name) != "OLDPWD" {
		return "password", true
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

func replaceMatches(text string, indices [][]int, kind string, found map[string]int) string {
	if len(indices) == 0 {
		return text
	}
	spans := make([]contextSpan, 0, len(indices))
	for _, index := range indices {
		spans = append(spans, contextSpan{index[0], index[1], kind})
	}
	return replaceSpans(text, spans, found)
}

func replaceSpans(text string, spans []contextSpan, found map[string]int) string {
	if len(spans) == 0 {
		return text
	}
	var out strings.Builder
	last := 0
	for _, span := range spans {
		out.WriteString(text[last:span.start])
		out.WriteString("[скрыто:")
		out.WriteString(span.kind)
		out.WriteByte(']')
		last = span.end
		found[span.kind]++
	}
	out.WriteString(text[last:])
	return out.String()
}

func findTokenMatches(text string, p pattern, scan *scanIndex) ([][]int, *scanIndex) {
	var accepted [][]int
	var bodyEnds [4]bodyEndCache
	for offset := 0; offset < len(text); {
		relative := strings.IndexAny(text[offset:], p.firstBytes)
		if relative < 0 {
			break
		}
		offset += relative
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
