package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/aiseeq/akit/transcript"
)

type openQuestion struct {
	at         time.Time
	text       string
	ref        string
	callID     string
	answeredAt time.Time
}

// reportOpenQuestions prints each main-session question and its first later user reply in file order.
func reportOpenQuestions(sources []source, stdout, stderr io.Writer) int {
	var questions []openQuestion
	for _, src := range sources {
		inSource, err := openQuestionsInSource(src, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "tl: read %s: %v\n", src.Path, err)
			return exitUsage
		}
		questions = append(questions, inSource...)
	}
	sort.SliceStable(questions, func(i, j int) bool { return questions[i].at.Before(questions[j].at) })
	for i := range questions {
		q := &questions[i]
		status := "открыт"
		answerAt := ""
		if !q.answeredAt.IsZero() {
			status = "закрыт"
			answerAt = " " + q.answeredAt.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(stdout, "%s %s%s %s %s\n", q.at.Local().Format("2006-01-02 15:04:05"), status, answerAt,
			transcript.Short(q.text, 180), q.ref)
	}
	return 0
}

func openQuestionsInSource(src source, warnings io.Writer) ([]openQuestion, error) {
	if src.ParentID != "" {
		return nil, nil
	}
	var questions []openQuestion
	blockedCalls := map[string]bool{}
	_, err := transcript.ReadWithTools(src.Path, nil, func(ev transcript.Event) {
		collectOpenQuestionEvent(src, ev, &questions, blockedCalls, warnings)
	})
	if err != nil {
		return nil, err
	}
	return filterBlockedQuestions(questions, blockedCalls), nil
}

func collectOpenQuestionEvent(src source, ev transcript.Event, questions *[]openQuestion, blockedCalls map[string]bool, warnings io.Writer) {
	if !ev.Sidechain && ev.Role == transcript.Out && ev.Who == "AskUserQuestion" && ev.Error && ev.CallID != "" {
		blockedCalls[ev.CallID] = true
	}
	texts, err := questionTexts(ev)
	if err != nil {
		fmt.Fprintf(warnings, "tl: %s:%d: ignoring AskUserQuestion input: %v\n", src.Path, ev.Line, err)
		return
	}
	for _, text := range texts {
		callID := ""
		if ev.Role == transcript.Cmd && ev.Who == "AskUserQuestion" {
			callID = ev.CallID
		}
		*questions = append(*questions, openQuestion{at: ev.Time, text: text, ref: questionRef(src, ev.Line), callID: callID})
	}
	isReply := !ev.Sidechain && (ev.Role == transcript.User || ev.Role == transcript.Ask && strings.TrimSpace(ev.Answer) != "")
	if isReply {
		for i := range *questions {
			if (*questions)[i].answeredAt.IsZero() {
				(*questions)[i].answeredAt = ev.Time
			}
		}
	}
}

func filterBlockedQuestions(questions []openQuestion, blockedCalls map[string]bool) []openQuestion {
	filtered := questions[:0]
	for _, q := range questions {
		if !blockedCalls[q.callID] {
			filtered = append(filtered, q)
		}
	}
	return filtered
}

func questionRef(src source, line int) string {
	id := sourceFullID(src)
	if src.ID == src.Path {
		id = src.Path
	}
	return fmt.Sprintf("%s:%d", id, line)
}

func questionTexts(ev transcript.Event) ([]string, error) {
	if ev.Sidechain {
		return nil, nil
	}
	if ev.Role == transcript.Cmd && ev.Who == "AskUserQuestion" {
		var input struct {
			Questions json.RawMessage `json:"questions"`
		}
		if err := json.Unmarshal(ev.Raw, &input); err != nil {
			return nil, fmt.Errorf("decode AskUserQuestion input: %w", err)
		}
		if len(input.Questions) == 0 {
			return nil, nil
		}
		if !strings.HasPrefix(strings.TrimSpace(string(input.Questions)), "[") {
			return nil, fmt.Errorf("AskUserQuestion questions is not an array")
		}
		var questions []struct {
			Question string `json:"question"`
		}
		if err := json.Unmarshal(input.Questions, &questions); err != nil {
			return nil, fmt.Errorf("decode AskUserQuestion questions: %w", err)
		}
		var out []string
		for _, q := range questions {
			if q.Question != "" {
				out = append(out, q.Question)
			}
		}
		return out, nil
	}
	if ev.Role != transcript.Agent {
		return nil, nil
	}
	last := lastPlainParagraph(ev.Text)
	return questionSentences(last), nil
}

// lastPlainParagraph skips fenced code, including blank lines inside a fence. A trailing fence leaves
// the prose paragraph before it as the last paragraph.
func lastPlainParagraph(text string) string {
	var paragraphs []string
	var current []string
	inFence := false
	finish := func() {
		if value := strings.TrimSpace(strings.Join(current, "\n")); value != "" {
			paragraphs = append(paragraphs, value)
		}
		current = nil
	}
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if trimmed == "" {
			finish()
			continue
		}
		current = append(current, line)
	}
	finish()
	if len(paragraphs) == 0 {
		return ""
	}
	return paragraphs[len(paragraphs)-1]
}

// questionSentences masks URLs, inline code and quoted text before extracting sentences with a question mark.
func questionSentences(paragraph string) []string {
	runes := []rune(paragraph)
	visible := maskQuestionContexts(runes)
	return extractQuestionSentences(runes, visible)
}

func maskQuestionContexts(runes []rune) []rune {
	visible := append([]rune(nil), runes...)
	var quote rune
	quoteDepth := 0
	inCode := false
	for i := 0; i < len(runes); {
		if quote == 0 && !inCode && startsURL(runes, i) {
			i = maskURL(runes, visible, i)
			continue
		}
		if inCode {
			inCode = maskCodeRune(runes, visible, i)
			i++
			continue
		}
		if quote != 0 {
			quote = maskQuotedRune(runes, visible, i, quote, &quoteDepth)
			i++
			continue
		}
		quote, inCode = openQuestionContext(runes, visible, i)
		if isNestedQuote(quote) {
			quoteDepth = 1
		}
		i++
	}
	return visible
}

func maskURL(runes, visible []rune, at int) int {
	for at < len(runes) && !isSpace(runes[at]) && !strings.ContainsRune(")]>}", runes[at]) && !trailingURLQuestion(runes, at) {
		visible[at] = ' '
		at++
	}
	return at
}

func maskCodeRune(runes, visible []rune, at int) bool {
	visible[at] = ' '
	return runes[at] != '`'
}

func maskQuotedRune(runes, visible []rune, at int, quote rune, depth *int) rune {
	visible[at] = ' '
	if isNestedQuote(quote) && runes[at] == quote {
		*depth++
		return quote
	}
	if runes[at] == quoteEnd(quote) {
		if isNestedQuote(quote) && *depth > 1 {
			*depth--
			return quote
		}
		*depth = 0
		return 0
	}
	return quote
}

func openQuestionContext(runes, visible []rune, at int) (rune, bool) {
	r := runes[at]
	if r == '`' && hasClosingInSentence(runes, at+1, '`', '`') {
		visible[at] = ' '
		return 0, true
	}
	if r == '"' && at > 0 && unicode.IsDigit(runes[at-1]) {
		return 0, false
	}
	if strings.ContainsRune("«“\"", r) && hasClosingInSentence(runes, at+1, quoteEnd(r), r) {
		visible[at] = ' '
		return r, false
	}
	return 0, false
}

func quoteEnd(open rune) rune {
	switch open {
	case '«':
		return '»'
	case '“':
		return '”'
	default:
		return open
	}
}

func hasClosingInSentence(text []rune, from int, closing, opener rune) bool {
	depth := 1
	for i := from; i < len(text); i++ {
		if isNestedQuote(opener) {
			if text[i] == opener {
				depth++
			}
			if text[i] == closing {
				depth--
				if depth == 0 {
					return true
				}
			}
			continue
		}
		if text[i] == closing {
			return true
		}
		if text[i] == '\n' {
			return false
		}
		if (text[i] == '.' || text[i] == '!') && isSpaceAfter(text, i) {
			if opener == '"' && hasClosingAfterQuotedQuestion(text, i+1, closing) {
				return true
			}
			return false
		}
	}
	return false
}

func hasClosingAfterQuotedQuestion(text []rune, from int, closing rune) bool {
	for i := from; i < len(text); i++ {
		if text[i] == '\n' {
			return false
		}
		if text[i] == closing {
			return i > 0 && text[i-1] == '?'
		}
	}
	return false
}

func isNestedQuote(quote rune) bool {
	return quote == '«' || quote == '“'
}

func isSpaceAfter(text []rune, at int) bool {
	return at+1 == len(text) || isSpace(text[at+1])
}

func extractQuestionSentences(runes, visible []rune) []string {
	var out []string
	seen := map[string]bool{}
	for i, r := range visible {
		if r != '?' {
			continue
		}
		if i > 0 && visible[i-1] == '?' {
			continue
		}
		start := i
		for start > 0 && !sentenceBoundary(visible, start-1) {
			start--
		}
		end := i + 1
		for end < len(runes) && visible[end] == '?' {
			end++
		}
		text := strings.TrimSpace(string(runes[start:end]))
		text = strings.TrimLeft(text, "-*• ")
		if text != "" && !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	return out
}

func startsURL(text []rune, at int) bool {
	for _, prefix := range []string{"https://", "http://", "www."} {
		p := []rune(prefix)
		if at+len(p) > len(text) {
			continue
		}
		match := true
		for i := range p {
			ch := text[at+i]
			if ch >= 'A' && ch <= 'Z' {
				ch += 'a' - 'A'
			}
			if ch != p[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func trailingURLQuestion(text []rune, at int) bool {
	if text[at] != '?' {
		return false
	}
	return at+1 == len(text) || isSpace(text[at+1]) || strings.ContainsRune(")]>}", text[at+1])
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

func sentenceBoundary(text []rune, at int) bool {
	r := text[at]
	if r == '.' {
		next := at + 1
		for next < len(text) && text[next] == '*' {
			next++
		}
		return next < len(text) && isSpace(text[next])
	}
	return strings.ContainsRune("!?\n", r)
}
