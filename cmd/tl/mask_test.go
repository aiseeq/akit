package main

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/secretmask"
)

// A key pasted into a transcript never comes out of tl: not in a conversation, a whole event, a grep or
// raw jsonl lines. The fake key is built at run time and failures never print the output that held it.
func TestOutputMasksSecrets(t *testing.T) {
	e := fakeHome(t)
	key := "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	line := `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-25T10:00:00.000Z","message":{"role":"user","content":"ключ ` + key + ` для шлюза"}}` + "\n"
	path := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", "9f8e7d6c-0000-4000-8000-000000000002.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"9f8e7d6c"},
		{"9f8e7d6c:1"},
		{"--all", "--role", "user", "--grep", "ключ"},
		{"9f8e7d6c", "--raw", "шлюза"},
	} {
		code, out, errOut := runTL(t, e, args...)
		if strings.Contains(out, key) || strings.Contains(errOut, key) {
			t.Errorf("tl %v: the key is in the output", args)
			continue
		}
		if code != 0 || !strings.Contains(out, "[скрыто:sk]") {
			t.Errorf("tl %v: code %d, no mask marker in %d bytes of output", args, code, len(out))
		}
	}
}

func TestShowRefMasksWholeMultilineEvent(t *testing.T) {
	e := fakeHome(t)
	value := "Zq314XvSyntheticValue"
	event := strings.Join([]string{
		"curl -s \\",
		"  -u admin:" + value + " \\",
		"  https://example.test/",
		"mysql -h db \\",
		"  -p" + value + " app",
		"{",
		"  \"name\": \"DB_PASSWORD\",",
		"  \"value\": \"" + value + "\"",
		"}",
		"**Password**",
		": " + value,
		"password",
		": " + value,
		"[\"curl\",",
		"\"-u\",",
		"\"admin:" + value + "\"]",
		"password=\"a",
		value + "\"",
	}, "\n")
	line, err := json.Marshal(map[string]any{
		"type": "user", "timestamp": "2026-10-25T10:00:00.000Z",
		"message": map[string]any{"role": "user", "content": event},
	})
	if err != nil {
		t.Fatal("marshal synthetic event")
	}
	path := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", sessionID+".jsonl")
	if err := os.WriteFile(path, append(line, '\n'), 0o644); err != nil {
		t.Fatal("write synthetic transcript")
	}
	code, out, errOut := runTL(t, e, sessionID+":1")
	if code != 0 || errOut != "" {
		t.Fatalf("showRef: code %d, stderr %q", code, errOut)
	}
	if strings.Contains(out, value) {
		t.Error("showRef exposed a synthetic value from the multiline event")
	}
	if got, want := strings.Count(out, "\n"), strings.Count(event, "\n")+2; got != want {
		t.Errorf("showRef changed newline count: got %d, want %d", got, want)
	}
	for _, width := range []string{"40", "200"} {
		code, out, errOut = runTL(t, e, "--width", width, sessionID+":1")
		if code != 0 || errOut != "" {
			t.Fatalf("showRef --width %s: code %d, stderr %q", width, code, errOut)
		}
		if strings.Contains(out, value) {
			t.Errorf("showRef --width %s exposed a synthetic value from the multiline event", width)
		}
		if !strings.Contains(out, "…(+") || strings.Count(out, "\n") != 2 {
			t.Errorf("showRef --width %s did not cut the multiline event to one line:\n%s", width, out)
		}
	}
}

func TestMaskEventRandomShapeAndLeaks(t *testing.T) {
	random := rand.New(rand.NewPCG(41, 73))
	fragments := []string{
		"password", "**Password**", "token", "DB_PASSWORD", ":", "=", " ", " ",
		"curl", "-u", "admin:", "mysql", "-p", "value", "name", "\"value\"",
		"\n", "\r\n", "\\\n", "{", "}", "[", "]", "\"", "'", "`",
		"Zq314XvSyntheticValue", "Zq315XvAnotherSyntheticValue",
	}
	secrets := []string{"Zq314XvSyntheticValue", "Zq315XvAnotherSyntheticValue"}
	lineShape := func(text string) string {
		var shape strings.Builder
		for i := 0; i < len(text); i++ {
			if text[i] == '\r' {
				shape.WriteByte('R')
			} else if text[i] == '\n' {
				shape.WriteByte('N')
			}
		}
		return shape.String()
	}
	for sample := range 300 {
		var event strings.Builder
		for range 40 {
			event.WriteString(fragments[random.IntN(len(fragments))])
		}
		input := event.String()
		base, _ := secretmask.Mask(input)
		maskedEvent := maskEventText(input)
		var output strings.Builder
		if err := writeMasked(&output, maskedEvent); err != nil {
			t.Fatalf("sample %d: write masked event: %v", sample, err)
		}
		branch := output.String()
		for _, value := range secrets {
			if !strings.Contains(base, value) && strings.Contains(branch, value) {
				t.Errorf("sample %d exposed a base-hidden synthetic value", sample)
			}
		}
		if lineShape(branch) != lineShape(input) {
			t.Errorf("sample %d changed line shape", sample)
		}
	}
}

func TestWriteMaskedKeepsFollowingEventLines(t *testing.T) {
	// Synthetic event text: the secret is intentionally much longer than a line
	// of ordinary output, and the following events must remain intact.
	text := "event 1 password=" + strings.Repeat("x9", 700) + "\nevent 2 ordinary\nevent 3 ordinary\n"
	originalHide := hide
	var calls int
	hide = func(line string) (string, map[string]int) {
		calls++
		if strings.Contains(line, "\n") || strings.Contains(line, "\r") {
			t.Error("masker received more than one output line")
		}
		before, _, ok := strings.Cut(line, "password=")
		if !ok {
			return line, nil
		}
		// Model a detector whose long value can span a newline if given the
		// whole output buffer. writeMasked must enforce the boundary itself.
		return before + "password=[скрыто:password]", map[string]int{"password": 1}
	}
	t.Cleanup(func() { hide = originalHide })
	var out strings.Builder
	if err := writeMasked(&out, text); err != nil {
		t.Fatal("write masked output")
	}
	got := out.String()
	if lines := strings.Count(got, "event "); lines != 3 {
		t.Errorf("got %d event lines, want 3", lines)
	}
	if calls != 3 {
		t.Errorf("masker called %d times, want one call per line (3)", calls)
	}
	for _, line := range []string{"event 1 password=[скрыто:password]", "event 2 ordinary", "event 3 ordinary"} {
		if !strings.Contains(got, line) {
			t.Errorf("missing expected output line %q", line)
		}
	}
}

func TestMaskEventPrivateKeyBlocksPreserveLines(t *testing.T) {
	originalHide := hide
	hide = secretmask.Mask
	t.Cleanup(func() { hide = originalHide })

	body := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=\r\n", 40)
	rsaBegin, rsaEnd := "-----BEGIN RSA "+"PRIVATE KEY-----", "-----END RSA "+"PRIVATE KEY-----"
	pgpBegin, pgpEnd := "-----BEGIN PGP "+"PRIVATE KEY BLOCK-----", "-----END PGP "+"PRIVATE KEY BLOCK-----"
	blocks := []struct {
		name, begin, end string
	}{
		{"rsa", rsaBegin, rsaEnd},
		{"pgp", pgpBegin, pgpEnd},
	}
	for _, block := range blocks {
		t.Run(block.name, func(t *testing.T) {
			input := "event 1 " + block.begin + "\r\n" + body + block.end + "\r\nevent 2 ordinary\r\n"
			got := maskEventText(input)
			if strings.Contains(got, "QUJDREVGR0hJSktM") {
				t.Error("synthetic key body remains visible")
			}
			if strings.Count(got, "\n") != strings.Count(input, "\n") || strings.Count(got, "\r\n") != strings.Count(input, "\r\n") {
				t.Errorf("line break count changed: input %d/%d, output %d/%d", strings.Count(input, "\n"), strings.Count(input, "\r\n"), strings.Count(got, "\n"), strings.Count(got, "\r\n"))
			}
			if !strings.Contains(got, "[скрыто:private-key]") || !strings.Contains(got, "event 2 ordinary") {
				t.Error("closed key block or following event was not preserved")
			}
		})
	}
}

func TestMaskEventUnterminatedPrivateKeyBlock(t *testing.T) {
	originalHide := hide
	hide = secretmask.Mask
	t.Cleanup(func() { hide = originalHide })

	input := "event 1 -----BEGIN RSA " + "PRIVATE KEY-----\n" + strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=\n", 40)
	got := maskEventText(input)
	if strings.Contains(got, "QUJDREVGR0hJSktM") {
		t.Error("synthetic unterminated key body remains visible")
	}
	if strings.Count(got, "\n") != strings.Count(input, "\n") || !strings.Contains(got, "[скрыто:private-key]") {
		t.Error("unterminated key masking changed line breaks or omitted marker")
	}
}

// An argument error quotes the value; a key passed by mistake as a value stays masked there too.
func TestArgumentErrorMasksSecrets(t *testing.T) {
	key := "sk" + "-proj-" + strings.Repeat("x9Y8", 6)
	for _, args := range [][]string{{"--since", key}, {"--q", key}, {"--role", key}} {
		var out, errOut strings.Builder
		code := start(args, time.Now(), &out, &errOut)
		if strings.Contains(out.String()+errOut.String(), key) {
			t.Errorf("tl %s VALUE: the key is in the output", args[0])
			continue
		}
		if code != exitUsage || !strings.Contains(errOut.String(), "[скрыто:sk]") {
			t.Errorf("tl %s VALUE: code %d, no mask marker in %d bytes of stderr", args[0], code, errOut.Len())
		}
	}
}

// A narrow --width cuts a key inside its body. The cut must not leave a piece
// of the value that the mask no longer knows by its prefix: the text is masked
// before it is cut, in the event list and in a whole event.
func TestTruncatedLineMasksKeyFirst(t *testing.T) {
	e := fakeHome(t)
	body := "Q7wK2mXp9LzR4tVb8NcY"
	key := "sk" + "-proj-" + body
	line := `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-25T10:00:00.000Z","message":{"role":"user","content":"ключ ` + key + ` для шлюза"}}` + "\n"
	path := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", "9f8e7d6c-0000-4000-8000-000000000003.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	for width := 6; width <= 40; width++ {
		for _, args := range [][]string{
			{"9f8e7d6c", "--width", strconv.Itoa(width)},
			{"9f8e7d6c:1", "--width", strconv.Itoa(width)},
		} {
			code, out, errOut := runTL(t, e, args...)
			if code != 0 {
				t.Errorf("tl %v: code %d", args, code)
				continue
			}
			for n := 6; n <= len(body); n++ {
				for i := 0; i+n <= len(body); i++ {
					if strings.Contains(out+errOut, body[i:i+n]) {
						t.Errorf("tl %v: %d characters of the key body are in the output", args, n)
						goto next
					}
				}
			}
		next:
		}
	}
}

// Keys one per line, longer than the window: the mask shrinks each key to a short
// label, so the visible part reaches past the first window. A key cut by the
// window's end must not show; the hidden count follows the masked text.
func TestTruncatedLineMasksKeysPastShrunkWindow(t *testing.T) {
	e := fakeHome(t)
	const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"
	rng := rand.New(rand.NewPCG(7, 7))
	keys := []string{"cat keys.txt"}
	var bodies []string
	for i := range 40 {
		b := make([]byte, 48)
		for j := range b {
			b[j] = alphabet[rng.IntN(len(alphabet))]
		}
		body := string(b) + strconv.Itoa(i%10)
		bodies = append(bodies, body)
		keys = append(keys, "sk"+"-proj-"+body)
	}
	text := strings.Join(keys, "\n")
	line := `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-25T10:00:00.000Z","message":{"role":"user","content":` + strconv.Quote(text) + `}}` + "\n"
	path := filepath.Join(e.home, ".claude", "projects", "-home-x-proj", "9f8e7d6c-0000-4000-8000-000000000004.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	for width := 20; width <= 400; width += 7 {
		for _, args := range [][]string{
			{"9f8e7d6c", "--width", strconv.Itoa(width)},
			{"9f8e7d6c:1", "--width", strconv.Itoa(width)},
		} {
			code, out, errOut := runTL(t, e, args...)
			if code != 0 {
				t.Errorf("tl %v: code %d", args, code)
				continue
			}
			if strings.Contains(out+errOut, "sk"+"-proj-") {
				t.Errorf("tl %v: a key prefix is in the output", args)
			}
			for _, body := range bodies {
				if strings.Contains(out+errOut, body[:4]) {
					t.Errorf("tl %v: a piece of key %d is in the output", args, slices.Index(bodies, body))
					break
				}
			}
		}
	}
}
