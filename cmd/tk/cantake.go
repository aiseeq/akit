package main

import (
	"fmt"
	"io"
	"os"
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
// reason goes to errout, and the list of other projects stays whole.
func (v *takeView) project(project string, errout io.Writer) takeProject {
	if state, ok := v.projects[project]; ok {
		return state
	}
	state, err := v.resolve(project, errout)
	if err != nil {
		fmt.Fprintf(errout, "tk: %s: can_take is false: %v\n", project, err)
		state = takeProject{refused: true}
	}
	v.projects[project] = state
	return state
}

func (v *takeView) resolve(project string, errout io.Writer) (takeProject, error) {
	if v.broken != nil {
		return takeProject{}, v.broken
	}
	want, err := normalizedRoot(project)
	if err != nil {
		return takeProject{}, fmt.Errorf("resolve project %s: %w", project, err)
	}
	master, err := v.files.masterName(want, errout)
	if err != nil {
		return takeProject{}, err
	}
	refusal, err := v.files.refusal(project, master, errout)
	if err != nil {
		return takeProject{}, err
	}
	scope, err := v.gate.For(project)
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
