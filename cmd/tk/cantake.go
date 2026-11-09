package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aiseeq/akit/queuegate"
)

// takeView answers what tk next would give the master of a project. It reads the
// gate and the frozen list once per command, however many projects a list covers,
// and decides with the same functions as tk next.
type takeView struct {
	gate     queuegate.Gate
	files    frozenFiles
	broken   error // why tk next could not run for any project
	projects map[string]takeProject
}

type takeProject struct {
	master  string
	scope   queuegate.Scope
	refused bool
}

func newTakeView() *takeView {
	view := &takeView{projects: map[string]takeProject{}}
	home, err := os.UserHomeDir()
	if err != nil {
		view.broken = fmt.Errorf("determine gate directory: %w", err)
		return view
	}
	view.gate = queuegate.Read(queuegate.Path(home), time.Now())
	view.files, view.broken = readFrozenFiles()
	return view
}

// project is the state of one project, worked out once. Where it cannot be
// worked out tk next fails too and so gives nothing: the answer is false and the
// reason goes to errout, and the list of other projects stays whole. A problem
// of a single line of the files is reported by the commands that act on it; a
// list would only repeat it for each project.
func (v *takeView) project(project string, errout io.Writer) takeProject {
	if state, ok := v.projects[project]; ok {
		return state
	}
	state, err := v.resolve(project)
	if err != nil {
		fmt.Fprintf(errout, "tk: %s: can_take is false: %v\n", project, err)
		state = takeProject{refused: true}
	}
	v.projects[project] = state
	return state
}

func (v *takeView) resolve(project string) (takeProject, error) {
	if v.broken != nil {
		return takeProject{}, v.broken
	}
	want, err := normalizedRoot(project)
	if err != nil {
		return takeProject{}, fmt.Errorf("resolve project %s: %w", project, err)
	}
	// tk next asks the table only about a frozen project, and there refusal below
	// fails the same way. Elsewhere the name only tells whose work is in progress.
	master, nameErr := v.files.masterName(want, io.Discard)
	if nameErr != nil {
		master = filepath.Base(want) + "-master"
	}
	refusal, err := v.files.refusal(project, master, io.Discard)
	if err != nil {
		return takeProject{}, err
	}
	scope, err := v.gate.For(want)
	if err != nil {
		return takeProject{}, err
	}
	return takeProject{master: master, scope: scope, refused: refusal != ""}, nil
}

func (v *takeView) canTake(t task, project string, errout io.Writer) bool {
	state := v.project(project, errout)
	return !state.refused && len(nextCandidates([]task{t}, state.master, state.scope)) == 1
}

func (v *takeView) canTakeExternal(item externalTask, project string, errout io.Writer) bool {
	state := v.project(project, errout)
	return !state.refused && externalVisible(item, state.scope)
}

// externalVisible: a source record the gates show; what was already begun stays.
func externalVisible(item externalTask, gate queuegate.Scope) bool {
	return item.Started || gate.Passes(item.Prio, "")
}
