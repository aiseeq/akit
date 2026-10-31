package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// mount — строка /proc/self/mountinfo: файловая система dev, её папка root видна в точке point.
type mount struct {
	dev, root, point string
}

// readMounts — точки монтирования процесса; вне Linux — пусто. На Linux без /proc/self/mountinfo (chroot,
// песочница без /proc) точек не узнать — это ошибка, а не пустой список: очистка зашла бы в точку монтирования.
func readMounts() ([]mount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if errors.Is(err, fs.ErrNotExist) && runtime.GOOS != "linux" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mounts: %w", err)
	}
	defer func() { _ = f.Close() }()
	return parseMounts(f)
}

// parseMounts — строки mountinfo. Поля разделяет ровно один пробел, а пробел, таб и перевод строки внутри поля
// экранированы; прочие пробельные символы (U+00A0, \v) — часть пути, поэтому делить по " ", не strings.Fields.
func parseMounts(r io.Reader) ([]mount, error) {
	var mounts []mount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), " ")
		if len(fields) < 5 {
			return nil, fmt.Errorf("mounts: short line %q", sc.Text())
		}
		mounts = append(mounts, mount{dev: fields[2], root: unescapeMount(fields[3]), point: unescapeMount(fields[4])})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mounts: %w", err)
	}
	return mounts, nil
}

// kernelName — путь p так, как его пишет ядро и в mountinfo: на папке без учёта регистра EvalSymlinks оставляет
// регистр, как его написали (…/OUT), а точка монтирования в mountinfo — с регистром на диске (…/out), и сверка по
// тексту их бы не сошлась. Вне Linux — p как есть.
func kernelName(p string) (string, error) {
	if runtime.GOOS != "linux" {
		return p, nil
	}
	// O_NONBLOCK: канал на месте файла не повесит открытие
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", p, err)
	}
	defer func() { _ = f.Close() }()
	name, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		return "", fmt.Errorf("kernel path of %s: %w", p, err)
	}
	if !filepath.IsAbs(name) || strings.HasSuffix(name, " (deleted)") {
		return "", fmt.Errorf("kernel path of %s: %q", p, name)
	}
	return name, nil
}

// unescapeMount — поле mountinfo с восьмеричными \040, \011, \012, \134 вместо пробела, таба, перевода строки и \.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// aliases — все пути, под которыми видна папка p (путь без ссылок): bind mount показывает папку файловой системы
// в другой точке, и та же папка видна через любую точку той же файловой системы, чья папка её содержит. Сам p —
// всегда среди них. Путь, закрытый другим монтированием, тоже попадает в список: лишний путь даёт только отказ.
func aliases(p string, mounts []mount) []string {
	var own *mount
	ownRel := ""
	for i, m := range mounts {
		// на одной точке позднее монтирование закрывает раннее: строки идут по порядку монтирования
		if r, ok := under(p, m.point); ok && (own == nil || len(m.point) >= len(own.point)) {
			own, ownRel = &mounts[i], r
		}
	}
	out := []string{p}
	if own == nil {
		return out
	}
	inFS := filepath.Join(own.root, ownRel)
	for _, m := range mounts {
		r, ok := under(inFS, m.root)
		if m.dev != own.dev || !ok {
			continue
		}
		if a := filepath.Join(m.point, r); a != p {
			out = append(out, a)
		}
	}
	return out
}

// under — путь p относительно dir, если p совпадает с dir или лежит внутри него, по тексту.
func under(p, dir string) (string, bool) {
	r, err := filepath.Rel(dir, p)
	return r, err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
