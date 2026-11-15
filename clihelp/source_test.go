package clihelp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceKeysFindParserForms(t *testing.T) {
	dir := t.TempDir()
	main := `package sample
import ("slices"; "strings")
const backupFlag = "--backup-constant"
var extraFlags = map[string]bool{"--backup-map": true}
func parseArgs(name, arg string) bool {
	switch name { case "--backup-case": return true }
	if name == "--backup-equal" {}
	if name != "--backup-not-equal" {}
	if extraFlags[name] {}
	if slices.Contains([]string{"--backup-slice"}, name) {}
	if strings.HasPrefix(arg, "--backup-prefix") {}
	if strings.TrimPrefix(arg, "--backup-trim=") != arg {}
	if _, ok := strings.CutPrefix(arg, "--backup-cut-prefix="); ok {}
	if key, _, ok := strings.Cut(arg, "="); ok && key == "--backup-cut" {}
	if strings.EqualFold(name, "--backup-equal-fold") {}
	if isBackup(name) {}
	if name == backupFlag {}
	return false
}`
	helper := `package sample
func isBackup(name string) bool { return name == "--backup-helper" }`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(main), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "helper.go"), []byte(helper), 0600); err != nil {
		t.Fatal(err)
	}
	got := sourceKeys(t, dir, []string{"parseArgs"})
	want := []string{
		"--backup-case", "--backup-equal", "--backup-not-equal", "--backup-map", "--backup-slice",
		"--backup-prefix", "--backup-trim", "--backup-cut-prefix", "--backup-cut", "--backup-equal-fold",
		"--backup-constant", "--backup-helper",
	}
	for _, key := range want {
		if !got[key] {
			t.Errorf("source key %q was not found", key)
		}
	}
}

func TestOptionalCommandFieldsOmittedUnlessSet(t *testing.T) {
	help := NewRoot("sample", []string{}, []string{}, "none", "sample")
	raw, err := json.Marshal(help.Commands[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"no_equals", "no_dashdash", "dash_args"} {
		if _, ok := fields[name]; ok {
			t.Errorf("unset field %q was serialized", name)
		}
	}
	help.Commands[0].NoEquals = true
	help.Commands[0].NoDashdash = true
	help.Commands[0].DashArgs = true
	raw, err = json.Marshal(help.Commands[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"no_equals", "no_dashdash", "dash_args"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("set field %q was omitted", name)
		}
	}
}
