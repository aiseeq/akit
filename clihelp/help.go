// Package clihelp defines the machine-readable command help shared by akit tools.
package clihelp

import (
	"encoding/json"
	"io"

	"github.com/aiseeq/akit/buildversion"
)

// Command describes one command's accepted flags and positional arguments.
type Command struct {
	Path       []string `json:"path"`
	Flags      []string `json:"flags"`
	ValueFlags []string `json:"value_flags"`
	Rest       string   `json:"rest"`
	Desc       string   `json:"desc"`
	NoEquals   bool     `json:"no_equals,omitempty"`
	NoDashdash bool     `json:"no_dashdash,omitempty"`
	DashArgs   bool     `json:"dash_args,omitempty"`
}

// Help is the machine-readable help document used by command-line linters.
type Help struct {
	Tool             string    `json:"tool"`
	Version          string    `json:"version"`
	GlobalFlags      []string  `json:"global_flags"`
	GlobalValueFlags []string  `json:"global_value_flags"`
	Commands         []Command `json:"commands"`
}

// New builds a help document containing one root command.
func New(tool string, command Command) Help {
	return Help{Tool: tool, Version: buildversion.Version(), GlobalFlags: []string{}, GlobalValueFlags: []string{}, Commands: []Command{command}}
}

// NewRoot builds a help document from a root command's flag table.
func NewRoot(tool string, flags, valueFlags []string, rest, desc string) Help {
	return New(tool, Command{Path: []string{}, Flags: flags, ValueFlags: valueFlags, Rest: rest, Desc: desc})
}

// Print writes stable, indented JSON followed by a newline.
func Print(w io.Writer, help Help) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(help)
}
