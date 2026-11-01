package edit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Write записывает изменённые файлы: сначала все во временные рядом с целью, затем переименованием поверх. Если файл
// изменился на диске после чтения, или временный не записался — не меняется ни один файл. Возвращает имена
// записанных файлов.
func (p *Plan) Write() ([]string, error) {
	if !p.OK() {
		return nil, errors.New("plan has mismatched match counts, nothing written")
	}
	var changed []*file
	var names []string
	for _, name := range p.order {
		f := p.files[name]
		if f.cur != f.orig {
			changed = append(changed, f)
			names = append(names, name)
		}
	}
	for i, f := range changed {
		st, err := os.Stat(f.path)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", names[i], err)
		}
		if st.Size() != f.size || !st.ModTime().Equal(f.modTime) {
			return nil, fmt.Errorf("%s changed on disk while editing, nothing written", names[i])
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, fmt.Errorf("stat %s: unexpected file metadata", names[i])
		}
		if sys.Nlink > 1 {
			return nil, fmt.Errorf("%s has %d hard links; sub replaces the file and would break them, nothing written", names[i], sys.Nlink)
		}
	}
	temps := make([]string, 0, len(changed))
	// cleanup убирает временные файлы; не удалившийся называется в ошибке, чтобы агент убрал его сам.
	cleanup := func(err error) error {
		for _, t := range temps {
			if rerr := os.Remove(t); rerr != nil {
				err = errors.Join(err, fmt.Errorf("temp file left: %w", rerr))
			}
		}
		return err
	}
	for i, f := range changed {
		tmp, err := writeTemp(f)
		if err != nil {
			return nil, cleanup(fmt.Errorf("write %s: %w, nothing written", names[i], err))
		}
		temps = append(temps, tmp)
	}
	for i, f := range changed {
		if err := os.Rename(temps[i], f.path); err != nil {
			temps = temps[i:]
			return names[:i], cleanup(fmt.Errorf("replace %s: %w; already written: %s", names[i], err, strings.Join(names[:i], ", ")))
		}
	}
	return names, nil
}

func writeTemp(f *file) (string, error) {
	t, err := os.CreateTemp(filepath.Dir(f.path), "."+filepath.Base(f.path)+".sub-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	name := t.Name()
	_, werr := t.WriteString(f.cur)
	if werr == nil {
		st, err := t.Stat()
		if err != nil {
			werr = err
		} else if sys, ok := st.Sys().(*syscall.Stat_t); !ok {
			werr = errors.New("unexpected temp file metadata")
		} else if int(sys.Uid) != f.uid || int(sys.Gid) != f.gid {
			werr = unix.Fchown(int(t.Fd()), f.uid, f.gid)
		}
	}
	if werr == nil {
		werr = t.Chmod(f.mode)
	}
	if werr == nil {
		werr = copyXattrs(f.path, name)
	}
	if werr == nil {
		werr = t.Sync()
	}
	if cerr := t.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		werr = fmt.Errorf("temp file %s: %w", name, werr)
		if rerr := os.Remove(name); rerr != nil {
			werr = errors.Join(werr, fmt.Errorf("temp file left: %w", rerr))
		}
		return "", werr
	}
	return name, nil
}

func copyXattrs(source, target string) error {
	size, err := unix.Listxattr(source, nil)
	if err != nil {
		return fmt.Errorf("list extended attributes: %w", err)
	}
	if size == 0 {
		return nil
	}
	list := make([]byte, size)
	n, err := unix.Listxattr(source, list)
	if err != nil {
		return fmt.Errorf("list extended attributes: %w", err)
	}
	for attr := range strings.SplitSeq(string(list[:n]), "\x00") {
		if attr == "" {
			continue
		}
		size, err := unix.Getxattr(source, attr, nil)
		if err != nil {
			return fmt.Errorf("get attribute %s: %w", attr, err)
		}
		value := make([]byte, size)
		got, err := unix.Getxattr(source, attr, value)
		if err != nil {
			return fmt.Errorf("get attribute %s: %w", attr, err)
		}
		if err := unix.Setxattr(target, attr, value[:got], 0); err != nil {
			return fmt.Errorf("set attribute %s: %w", attr, err)
		}
	}
	return nil
}
