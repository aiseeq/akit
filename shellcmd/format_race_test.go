package shellcmd

import (
	"slices"
	"sync"
	"testing"
)

// Calls is used from parallel scanners; decoding ANSI-C and printf escapes must not share state between calls.
func TestCallsParallelEscapes(t *testing.T) {
	cmds := []string{
		`$'\x67\x72\x61\x66\x74' test ./...`,
		`$'\x77\x66\x6f\x72' line DONE out.log`,
		`echo -e 'a\tb' | $'\x63\x61\x74'`,
		`printf '%s\n' x | $'\x6a\x71' .`,
	}
	names := func(cmd string) []string {
		calls, err := Calls(cmd)
		if err != nil {
			return []string{"error: " + err.Error()}
		}
		var out []string
		for _, c := range calls {
			out = append(out, c.Name)
			out = append(out, c.Args...)
		}
		return out
	}
	want := make([][]string, len(cmds))
	for i, c := range cmds {
		want[i] = names(c)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	bad := 0
	for range 8 {
		wg.Go(func() {
			for n := range 2000 {
				i := n % len(cmds)
				if got := names(cmds[i]); !slices.Equal(got, want[i]) {
					mu.Lock()
					bad++
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	if bad > 0 {
		t.Fatalf("%d parallel decodes differ from the sequential result", bad)
	}
}
