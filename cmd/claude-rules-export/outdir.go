package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Выгрузка удаляет в OUT_DIR только записи своего прошлого прогона, которые с тех пор не менялись: путь, тип,
// права с битами suid, sgid и sticky, размер и sha256 файла, текст ссылки, xattr (в них и ACL), устройство, inode
// и время рождения — как в списке. Список лежит вне OUT_DIR, в каталоге состояния, под ключом физического пути
// OUT_DIR: под другим путём (ссылка, bind mount, overlay, /proc) его нет, и непустая папка без списка — отказ. Чужой
// файл с тем же содержимым — другие устройство, inode или время рождения, тоже отказ. Папки удаляются только
// пустыми, все операции в OUT_DIR — через os.Root: ни путь из списка, ни ссылка, подменённая на ходу, не выводят
// за OUT_DIR.
//
// Новое попадает под своё имя только после строки журнала (рядом со списком) с его inode: файл пишется без имени
// (O_TMPFILE) и получает имя linkat; папка и ссылка создаются под случайным именем, записанным в журнал до
// создания, и переименовываются без замены. Оборванный прогон не оставляет под именами выгрузки ничего, чего нет
// в журнале.

// entry — запись выгрузки: f — файл, l — ссылка, d — папка; td и tl — папка или ссылка под случайным именем Path,
// которое прогон записал в журнал до создания. At — где запись лежит до переименования в Path.
type entry struct {
	Kind  string      `json:"kind"`
	Path  string      `json:"path"`
	At    string      `json:"at,omitempty"`
	Mode  fs.FileMode `json:"mode,omitempty"`
	Size  int64       `json:"size,omitempty"`
	Sum   string      `json:"sha256,omitempty"`
	Link  string      `json:"link,omitempty"`
	Xattr string      `json:"xattr,omitempty"`
	Uid   uint32      `json:"uid,omitempty"`
	Gid   uint32      `json:"gid,omitempty"`
	Dev   uint64      `json:"dev,omitempty"`
	Ino   uint64      `json:"ino,omitempty"`
	Birth int64       `json:"birth,omitempty"`
}

// tempPrefix — начало случайного имени, под которым создаются папка и ссылка до переименования.
const tempPrefix = ".claude-rules-export-"

// openUnnamed открывает файл без имени в папке dirfd; тест подменяет его файловой системой без O_TMPFILE.
var openUnnamed = func(dirfd int) (int, error) {
	return unix.Openat(dirfd, ".", unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
}

// modeBits — права, которые выгрузка сверяет и ставит.
const modeBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

type exportList struct {
	Out     string  `json:"out"`
	Entries []entry `json:"entries"`
}

// transferStep — точка после шага переноса; nil в работе. Тест останавливает на ней прогон, как обрыв процесса.
var transferStep func(step string) error

// listFile — файл списка для OUT_DIR out: $XDG_STATE_HOME/claude-rules-export/<sha256 пути>.json.
func listFile(out string) (string, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("state folder: %w", err)
		}
		state = filepath.Join(home, ".local", "state")
	}
	sum := sha256.Sum256([]byte(out))
	return filepath.Join(state, "claude-rules-export", hex.EncodeToString(sum[:16])+".json"), nil
}

// stageDir — пустая папка сборки для OUT_DIR out рядом с его списком; сборка оборванного прогона убирается.
func stageDir(out string) (string, error) {
	file, err := listFile(out)
	if err != nil {
		return "", err
	}
	stage := strings.TrimSuffix(file, ".json") + ".stage"
	if err := os.RemoveAll(stage); err != nil {
		return "", fmt.Errorf("stage: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(stage), 0o755); err != nil {
		return "", fmt.Errorf("stage: %w", err)
	}
	if err := os.Mkdir(stage, 0o700); err != nil {
		return "", fmt.Errorf("stage: %w", err)
	}
	return stage, nil
}

// journalFile — журнал записей, созданных прогоном, рядом со списком: по строке JSON на запись.
func journalFile(list string) string {
	return strings.TrimSuffix(list, ".json") + ".journal"
}

// loadJournal — записи из журнала оборванного прогона. Последняя строка без перевода строки оборвана на записи:
// её файл ещё не получил имени.
func loadJournal(file string) ([]entry, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("export journal: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	var entries []entry
	for _, line := range lines[:len(lines)-1] {
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("export journal %s: %w", file, err)
		}
		if err := checkEntryPath(e.Path); err != nil {
			return nil, fmt.Errorf("export journal %s: %w", file, err)
		}
		if e.At != "" {
			if err := checkEntryPath(e.At); err != nil {
				return nil, fmt.Errorf("export journal %s: %w", file, err)
			}
		}
		temp := e.Kind == "td" || e.Kind == "tl"
		if temp != (e.Ino == 0) || temp && !strings.HasPrefix(path.Base(e.Path), tempPrefix) {
			return nil, fmt.Errorf("export journal %s: bad line for %s", file, e.Path)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func logEntry(journal *os.File, e entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("export journal: %w", err)
	}
	if _, err := journal.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("export journal: %w", err)
	}
	return nil
}

func loadList(file, out string) ([]entry, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("export list: %w", err)
	}
	var l exportList
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("export list %s: %w", file, err)
	}
	if l.Out != out {
		return nil, fmt.Errorf("export list %s is for %s, not %s", file, l.Out, out)
	}
	for i := range l.Entries {
		if err := checkEntryPath(l.Entries[i].Path); err != nil {
			return nil, fmt.Errorf("export list %s: %w", file, err)
		}
	}
	return l.Entries, nil
}

// saveList пишет список целиком через временный файл и rename: обрыв оставляет прежний или новый, не половину.
func saveList(file, out string, entries []entry) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return fmt.Errorf("export list: %w", err)
	}
	data, err := json.MarshalIndent(exportList{Out: out, Entries: entries}, "", " ")
	if err != nil {
		return fmt.Errorf("export list: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".list-*")
	if err != nil {
		return fmt.Errorf("export list: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("export list: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("export list: %w", err)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return fmt.Errorf("export list: %w", err)
	}
	return nil
}

// checkEntryPath — путь записи внутри OUT_DIR и не в .git верхнего уровня (в любом регистре: папка может быть без
// учёта регистра).
func checkEntryPath(p string) error {
	if !filepath.IsLocal(p) || p != path.Clean(p) || p == "." || strings.ContainsRune(p, 0) {
		return fmt.Errorf("bad export path %q", p)
	}
	if first, _, _ := strings.Cut(p, "/"); strings.EqualFold(first, ".git") {
		return fmt.Errorf("export path inside .git: %q", p)
	}
	return nil
}

// scanStage — записи собранной выгрузки в папке dir; два пути, равные без учёта регистра, — ошибка до касания
// OUT_DIR: на папке без учёта регистра они сошлись бы в один.
func scanStage(dir string) ([]entry, error) {
	var entries []entry
	folded := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if other, ok := folded[strings.ToLower(rel)]; ok {
			return fmt.Errorf("export paths differ only in case: %s and %s", other, rel)
		}
		folded[strings.ToLower(rel)] = rel
		e, err := describe(func() (fs.FileInfo, error) { return os.Lstat(p) },
			func() (*os.File, error) { return os.Open(p) },
			func() (string, error) { return os.Readlink(p) }, rel)
		if err != nil {
			return err
		}
		// права нужны при переносе; владелец, xattr, устройство и inode — у созданной в OUT_DIR записи, не у сборки
		if e.Kind == "d" && e.Mode.Perm()&0o700 != 0o700 {
			return fmt.Errorf("folder %s: mode %v, the owner cannot write into it", rel, e.Mode)
		}
		e.Uid, e.Gid, e.Xattr, e.Dev, e.Ino = 0, 0, "", 0, 0
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	return entries, nil
}

// describe — запись по пути rel: тип, права, владелец и группа, размер, sha256 файла, текст ссылки, xattr,
// устройство, inode.
func describe(lstat func() (fs.FileInfo, error), open func() (*os.File, error), readlink func() (string, error), rel string) (entry, error) {
	if err := checkEntryPath(rel); err != nil {
		return entry{}, err
	}
	st, err := lstat()
	if err != nil {
		return entry{}, err
	}
	e := entry{Path: rel, Mode: st.Mode() & modeBits}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		e.Uid, e.Gid, e.Dev, e.Ino = sys.Uid, sys.Gid, sys.Dev, sys.Ino
	}
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		e.Kind, e.Mode = "l", 0
		if e.Link, err = readlink(); err != nil {
			return entry{}, err
		}
		return e, nil
	case st.Mode().IsRegular():
		e.Kind, e.Size = "f", st.Size()
	case st.IsDir():
		e.Kind = "d"
	default:
		return entry{}, fmt.Errorf("%s: not a file, folder or link", rel)
	}
	f, err := open()
	if err != nil {
		return entry{}, err
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd())
	if e.Xattr, err = xattrSum(func(b []byte) (int, error) { return unix.Flistxattr(fd, b) },
		func(name string, b []byte) (int, error) { return unix.Fgetxattr(fd, name, b) }); err != nil {
		return entry{}, fmt.Errorf("%s: %w", rel, err)
	}
	if e.Kind == "f" {
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return entry{}, fmt.Errorf("%s: %w", rel, err)
		}
		e.Sum = hex.EncodeToString(h.Sum(nil))
	}
	return e, nil
}

// xattrSum — sha256 имён и значений всех xattr открытого файла (ACL — тоже xattr); пусто, если их нет или файловая
// система их не держит.
func xattrSum(list func([]byte) (int, error), get func(string, []byte) (int, error)) (string, error) {
	size, err := list(nil)
	if errors.Is(err, unix.ENOTSUP) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("xattr: %w", err)
	}
	if size == 0 {
		return "", nil
	}
	buf := make([]byte, size)
	if size, err = list(buf); err != nil {
		return "", fmt.Errorf("xattr: %w", err)
	}
	names := strings.Split(strings.TrimSuffix(string(buf[:size]), "\x00"), "\x00")
	slices.Sort(names)
	h := sha256.New()
	for _, name := range names {
		n, err := get(name, nil)
		if err != nil {
			return "", fmt.Errorf("xattr %s: %w", name, err)
		}
		v := make([]byte, n)
		if n, err = get(name, v); err != nil {
			return "", fmt.Errorf("xattr %s: %w", name, err)
		}
		h.Write([]byte(name + "\x00" + strconv.Itoa(n) + "\x00"))
		h.Write(v[:n])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// same — запись на диске совпадает с записью списка или журнала. Под случайным именем из журнала — папка или
// ссылка с тем же текстом: прогон оборвался между созданием и строкой с inode; папка удаляется только пустой.
func same(have, want *entry) bool {
	switch want.Kind {
	case "td":
		return have.Kind == "d"
	case "tl":
		return have.Kind == "l" && have.Link == want.Link
	}
	return *have == *want
}

// statx — тест подменяет его файловой системой без времени рождения.
var statx = unix.Statx

// birthAt — время рождения записи name в папке dirfd, нс. Без него новая запись с повторённым inode не отличима от
// записи прошлого прогона — ошибка.
func birthAt(dirfd int, name string, flags int) (int64, error) {
	var st unix.Statx_t
	if err := statx(dirfd, name, flags|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &st); err != nil {
		return 0, fmt.Errorf("statx %s: %w", name, err)
	}
	if st.Mask&unix.STATX_BTIME == 0 {
		return 0, fmt.Errorf("statx %s: the file system gives no birth time", name)
	}
	return st.Btime.Sec*1e9 + int64(st.Btime.Nsec), nil
}

// linkXattr — xattr ссылки name в папке dirfd (метку SELinux ссылки обычный пользователь меняет chcon -h): listxattrat
// и getxattrat (Linux 6.13) без перехода по ссылке. f*xattr не берут дескриптор O_PATH, а путь /proc/self/fd/N
// зависит от того, что смонтировано на /proc.
func linkXattr(dirfd int, name string) (string, error) {
	p, err := unix.BytePtrFromString(name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	list := func(b []byte) (int, error) {
		n, _, errno := unix.Syscall6(unix.SYS_LISTXATTRAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)),
			unix.AT_SYMLINK_NOFOLLOW, uintptr(bufPtr(b)), uintptr(len(b)), 0)
		if errno != 0 {
			return 0, errno
		}
		return int(n), nil
	}
	get := func(attr string, b []byte) (int, error) {
		a, err := unix.BytePtrFromString(attr)
		if err != nil {
			return 0, err
		}
		args := xattrArgs{Value: uint64(uintptr(bufPtr(b))), Size: uint32(len(b))}
		n, _, errno := unix.Syscall6(unix.SYS_GETXATTRAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)),
			unix.AT_SYMLINK_NOFOLLOW, uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(&args)), unsafe.Sizeof(args))
		runtime.KeepAlive(b)
		if errno != 0 {
			return 0, errno
		}
		return int(n), nil
	}
	sum, err := xattrSum(list, get)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return sum, nil
}

// xattrArgs — struct xattr_args ядра для getxattrat.
type xattrArgs struct {
	Value uint64
	Size  uint32
	Flags uint32
}

func bufPtr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(&b[0])
}

// verifyOut — каждая запись OUT_DIR, кроме .git верхнего уровня, есть в списке и не менялась. Записи из списка,
// которых нет, допустимы: прошлый прогон оборвался посреди очистки.
func verifyOut(root *os.Root, known []entry) error {
	byPath := map[string][]int{}
	for i := range known {
		byPath[known[i].Path] = append(byPath[known[i].Path], i)
	}
	var walk func(dir string) error
	walk = func(dir string) error {
		f, err := root.Open(dir)
		if err != nil {
			return fmt.Errorf("OUT_DIR: %w", err)
		}
		defer func() { _ = f.Close() }()
		names, err := f.Readdirnames(-1)
		if err != nil {
			return fmt.Errorf("OUT_DIR: %w", err)
		}
		slices.Sort(names)
		for _, name := range names {
			if dir == "." && strings.EqualFold(name, ".git") {
				continue
			}
			rel := path.Join(dir, name)
			have, err := describe(func() (fs.FileInfo, error) { return root.Lstat(rel) },
				func() (*os.File, error) { return root.Open(rel) },
				func() (string, error) { return root.Readlink(rel) }, rel)
			if err != nil {
				return fmt.Errorf("OUT_DIR: %w", err)
			}
			if have.Birth, err = birthAt(int(f.Fd()), name, 0); err != nil {
				return fmt.Errorf("OUT_DIR: %w", err)
			}
			if have.Kind == "l" {
				if have.Xattr, err = linkXattr(int(f.Fd()), name); err != nil {
					return fmt.Errorf("OUT_DIR: %w", err)
				}
			}
			if !slices.ContainsFunc(byPath[rel], func(i int) bool { return same(&have, &known[i]) }) {
				if len(known) == 0 {
					return fmt.Errorf("OUT_DIR holds %s and has no export list: give an empty folder (only .git is kept) or a previous export", rel)
				}
				return fmt.Errorf("OUT_DIR holds %s, which is not the previous export's (changed or foreign): nothing is cleared", rel)
			}
			if have.Kind == "d" {
				if err := walk(rel); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(".")
}

// clearOld удаляет записи прошлого прогона: файлы и ссылки, затем папки от глубоких к мелким, только пустые.
// Записи, которой уже нет, — не ошибка.
func clearOld(root *os.Root, old []entry) error {
	var paths []string
	for i := range old {
		if !slices.Contains(paths, old[i].Path) {
			paths = append(paths, old[i].Path)
		}
	}
	// глубже — раньше: содержимое папки уходит до неё самой
	slices.SortFunc(paths, func(a, b string) int { return strings.Count(b, "/") - strings.Count(a, "/") })
	for _, p := range paths {
		if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear OUT_DIR: %w", err)
		}
		if transferStep != nil {
			if err := transferStep("removed"); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeNew переносит выгрузку из stage в OUT_DIR: каждую папку, файл и ссылку создаёт заново, существующий путь —
// ошибка, не перезапись. Каждую созданную запись пишет в журнал; возвращает их.
func writeNew(root *os.Root, stage string, entries []entry, journal *os.File) ([]entry, error) {
	done := make([]entry, 0, len(entries))
	for i := range entries {
		var (
			have entry
			err  error
		)
		if e := &entries[i]; e.Kind == "f" {
			have, err = placeFile(root, filepath.Join(stage, filepath.FromSlash(e.Path)), e, journal)
		} else {
			have, err = writeOther(root, e, journal)
		}
		if err != nil {
			return nil, fmt.Errorf("write OUT_DIR: %w", err)
		}
		done = append(done, have)
		if transferStep != nil {
			if err := transferStep("wrote"); err != nil {
				return nil, err
			}
		}
	}
	return done, nil
}

// writeOther создаёт папку с её правами или ссылку под случайным именем из журнала, пишет в журнал её запись с
// inode и переименовывает в e.Path без замены.
func writeOther(root *os.Root, e *entry, journal *os.File) (entry, error) {
	dir, err := root.Open(path.Dir(e.Path))
	if err != nil {
		return entry{}, fmt.Errorf("folder of %s: %w", e.Path, err)
	}
	defer func() { _ = dir.Close() }()
	random := make([]byte, 16)
	_, _ = rand.Read(random) // crypto/rand.Read не возвращает ошибок
	name := tempPrefix + hex.EncodeToString(random)
	tmp := path.Join(path.Dir(e.Path), name)
	if err := logEntry(journal, entry{Kind: "t" + e.Kind, Path: tmp, Link: e.Link}); err != nil {
		return entry{}, err
	}
	if transferStep != nil {
		if err := transferStep("intended"); err != nil {
			return entry{}, err
		}
	}
	if e.Kind == "d" {
		if err = root.Mkdir(tmp, 0o700); err == nil {
			err = root.Chmod(tmp, e.Mode)
		}
	} else {
		err = root.Symlink(e.Link, tmp)
	}
	if err != nil {
		return entry{}, fmt.Errorf("create %s: %w", e.Path, err)
	}
	if transferStep != nil {
		if err := transferStep("created"); err != nil {
			return entry{}, err
		}
	}
	have, err := describe(func() (fs.FileInfo, error) { return root.Lstat(tmp) },
		func() (*os.File, error) { return root.Open(tmp) },
		func() (string, error) { return root.Readlink(tmp) }, tmp)
	if err != nil {
		return entry{}, err
	}
	if have.Birth, err = birthAt(int(dir.Fd()), name, 0); err != nil {
		return entry{}, err
	}
	if have.Kind == "l" {
		if have.Xattr, err = linkXattr(int(dir.Fd()), name); err != nil {
			return entry{}, err
		}
	}
	have.Path, have.At = e.Path, tmp
	if err := logEntry(journal, have); err != nil {
		return entry{}, err
	}
	if transferStep != nil {
		if err := transferStep("journaled"); err != nil {
			return entry{}, err
		}
	}
	if err := unix.Renameat2(int(dir.Fd()), name, int(dir.Fd()), path.Base(e.Path), unix.RENAME_NOREPLACE); err != nil {
		return entry{}, fmt.Errorf("name %s: %w", e.Path, err)
	}
	have.At = ""
	return have, nil
}

// placeFile пишет файл без имени в его папке OUT_DIR, ставит права, пишет запись в журнал и только затем даёт
// файлу имя; занятое имя — ошибка linkat, не перезапись.
func placeFile(root *os.Root, src string, e *entry, journal *os.File) (entry, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return entry{}, fmt.Errorf("read stage: %w", err)
	}
	dir, err := root.Open(path.Dir(e.Path))
	if err != nil {
		return entry{}, fmt.Errorf("folder of %s: %w", e.Path, err)
	}
	defer func() { _ = dir.Close() }()
	fd, err := openUnnamed(int(dir.Fd()))
	if err != nil {
		return entry{}, fmt.Errorf("%s: unnamed file: %w", e.Path, err)
	}
	f := os.NewFile(uintptr(fd), e.Path)
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		return entry{}, fmt.Errorf("%s: %w", e.Path, err)
	}
	if err := f.Chmod(e.Mode); err != nil {
		return entry{}, fmt.Errorf("%s: %w", e.Path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return entry{}, fmt.Errorf("%s: %w", e.Path, err)
	}
	// describe закрывает то, что открыл: ему — копия дескриптора
	have, err := describe(func() (fs.FileInfo, error) { return f.Stat() }, func() (*os.File, error) {
		d, err := unix.Dup(fd)
		if err != nil {
			return nil, fmt.Errorf("dup: %w", err)
		}
		return os.NewFile(uintptr(d), e.Path), nil
	}, nil, e.Path)
	if err != nil {
		return entry{}, err
	}
	if have.Birth, err = birthAt(fd, "", unix.AT_EMPTY_PATH); err != nil {
		return entry{}, err
	}
	if err := logEntry(journal, have); err != nil {
		return entry{}, err
	}
	if transferStep != nil {
		if err := transferStep("journaled"); err != nil {
			return entry{}, err
		}
	}
	// по дескриптору, не через /proc/self/fd: подменённый /proc дал бы имя чужому файлу
	if err := unix.Linkat(fd, "", int(dir.Fd()), path.Base(e.Path), unix.AT_EMPTY_PATH); err != nil {
		return entry{}, fmt.Errorf("%s: name the file: %w", e.Path, err)
	}
	return have, nil
}

// loadKnown — записи прошлого прогона: список и журнал оборванного. Запись журнала, ещё не переименованная, —
// и под своим именем, и под случайным. Строка td или tl, за которой в журнале есть строка с inode под тем же
// случайным именем, снята ею: иначе под этим именем подошла бы любая папка.
func loadKnown(file, jfile, out string) ([]entry, error) {
	known, err := loadList(file, out)
	if err != nil {
		return nil, err
	}
	logged, err := loadJournal(jfile)
	if err != nil {
		return nil, err
	}
	named := map[string]bool{}
	for i := range logged {
		if logged[i].At != "" {
			named[logged[i].At] = true
		}
	}
	for i := range logged {
		e := logged[i]
		if (e.Kind == "td" || e.Kind == "tl") && named[e.Path] {
			continue
		}
		at := e.At
		e.At = ""
		known = append(known, e)
		if at != "" {
			e.Path = at
			known = append(known, e)
		}
	}
	return known, nil
}

// probeUnnamed — OUT_DIR держит файлы без имени: иначе перенос встал бы уже после очистки своего. Время рождения
// проверять здесь не нужно: записи прошлого прогона без него не пройдут verifyOut, а новый файл узнаёт его до имени.
func probeUnnamed(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("OUT_DIR: %w", err)
	}
	defer func() { _ = dir.Close() }()
	fd, err := openUnnamed(int(dir.Fd()))
	if err != nil {
		return fmt.Errorf("OUT_DIR does not keep unnamed files (O_TMPFILE): %w", err)
	}
	return unix.Close(fd)
}

// writeLogged — перенос с новым журналом jfile.
func writeLogged(root *os.Root, stage string, fresh []entry, jfile string) ([]entry, error) {
	journal, err := os.OpenFile(jfile, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("export journal: %w", err)
	}
	done, err := writeNew(root, stage, fresh, journal)
	if cerr := journal.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("export journal: %w", cerr)
	}
	return done, err
}

// replaceExport кладёт собранную в stage выгрузку в OUT_DIR out: сверка, список прошлого и нового прогона,
// очистка своего, перенос с журналом, список с inode. Обрыв на любом шаге оставляет список и журнал, по которым
// следующий прогон узнает и старые записи, и уже перенесённые новые.
func replaceExport(out, stage string) error {
	fresh, err := scanStage(stage)
	if err != nil {
		return err
	}
	file, err := listFile(out)
	if err != nil {
		return err
	}
	jfile := journalFile(file)
	old, err := loadKnown(file, jfile, out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("create OUT_DIR: %w", err)
	}
	root, err := os.OpenRoot(out)
	if err != nil {
		return fmt.Errorf("OUT_DIR: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := verifyOut(root, old); err != nil {
		return err
	}
	if err := probeUnnamed(root); err != nil {
		return err
	}
	// список с журналом оборванного прогона — до первого удаления; журнал после этого не нужен, а после очистки —
	// и список: всё новое прогон держит в журнале
	if err := saveList(file, out, old); err != nil {
		return err
	}
	if err := os.Remove(jfile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("export journal: %w", err)
	}
	if err := clearOld(root, old); err != nil {
		return err
	}
	if err := saveList(file, out, nil); err != nil {
		return err
	}
	if transferStep != nil {
		if err := transferStep("cleared"); err != nil {
			return err
		}
	}
	done, err := writeLogged(root, stage, fresh, jfile)
	if err != nil {
		return err
	}
	if err := saveList(file, out, done); err != nil {
		return err
	}
	if err := os.Remove(jfile); err != nil {
		return fmt.Errorf("export journal: %w", err)
	}
	return nil
}
