package shellcmd

import (
	"errors"
	"slices"
	"testing"
)

// An unknown short group before the CPU list is skipped only when it starts
// with -c: taskset rejects -0c and runs nothing, so no kill call follows it.
// An unknown wrapper option may instead be reported as hiding a signal.
func TestTasksetClusterNeedsCPUListFirst(t *testing.T) {
	calls, err := Calls(`taskset -0c 1 kill 1`)
	if err != nil && !errors.Is(err, errUnknownWrapperSignal) {
		t.Fatal(err)
	}
	var names []string
	for _, c := range calls {
		names = append(names, c.Name)
	}
	if slices.Contains(names, "kill") {
		t.Fatalf("calls = %v, want no kill call", names)
	}
	if err == nil && !slices.Equal(names, []string{"taskset"}) {
		t.Fatalf("calls = %v, want [taskset]", names)
	}
}
