package main

// watchEvent — something happened to an entry of a watched folder.
type watchEvent struct {
	dir, name string
	isDir     bool
}

// watcher speeds up finding changes; the passes do not depend on it: without one they find the same by polling.
type watcher interface {
	add(dir string)
	// drain returns the events since the last call; overflow — some were lost, a full listing is due
	drain() (events []watchEvent, overflow bool)
	active() bool
	close()
}

// noWatch — no inotify: everything is found by polling.
type noWatch struct{}

func (noWatch) add(string)                  {}
func (noWatch) drain() ([]watchEvent, bool) { return nil, false }
func (noWatch) active() bool                { return false }
func (noWatch) close()                      {}
