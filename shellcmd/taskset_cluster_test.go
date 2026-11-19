package shellcmd

import (
	"slices"
	"testing"
)

// An unknown short group before the CPU list is skipped only when it starts
// with -c: taskset rejects -0c and runs nothing, so no call follows it.
func TestTasksetClusterNeedsCPUListFirst(t *testing.T) {
	calls, err := Calls(`taskset -0c 1 kill 1`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range calls {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"taskset"}) {
		t.Fatalf("calls = %v, want [taskset]", names)
	}
}
