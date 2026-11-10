package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aiseeq/akit/secretmask"
)

// maskKeyLen — длина ключа меток, который tl создаёт сам.
const maskKeyLen = 32

// hide маскирует весь вывод tl. Без ключа — secretmask.Mask; start ставит маску с метками
// (длина и метка скрытого значения по ключу), когда ключ есть или создан.
var hide = secretmask.Mask

// maskKeyPath — $XDG_STATE_HOME/akit/mask-key, без XDG_STATE_HOME — ~/.local/state/akit/mask-key.
// Ключ постоянный: метки одного значения совпадают между вызовами tl.
func maskKeyPath(home string) string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "akit", "mask-key")
}

// loadMaskKey читает ключ из path, а нет файла — создаёт его (0600) из crypto/rand. Короткий ключ
// в файле — ошибка, не замена: иначе метки молча разошлись бы с прежними.
func loadMaskKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if key, err = createMaskKey(path); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("read mask key: %w", err)
	}
	if len(key) < secretmask.MinTagKeyLen {
		return nil, fmt.Errorf("mask key %s: %d bytes, need at least %d", path, len(key), secretmask.MinTagKeyLen)
	}
	return key, nil
}

func createMaskKey(path string) ([]byte, error) {
	key := make([]byte, maskKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("make mask key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("make mask key directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) { // другой tl создал его первым
		if key, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("read mask key: %w", err)
		}
		return key, nil
	}
	if err != nil {
		return nil, fmt.Errorf("create mask key: %w", err)
	}
	_, werr := f.Write(key)
	if err := errors.Join(werr, f.Close()); err != nil {
		return nil, fmt.Errorf("write mask key: %w", err)
	}
	return key, nil
}

// useMaskKey ставит маску с метками по ключу из path. Ключа не получить — вывод маскируется
// без длины и метки, и tl говорит об этом в stderr.
func useMaskKey(path string) error {
	key, err := loadMaskKey(path)
	if err == nil {
		var tagger *secretmask.Tagger
		if tagger, err = secretmask.NewTagger(key); err == nil {
			hide = tagger.Mask
			return nil
		}
	}
	hide = secretmask.Mask
	return fmt.Errorf("hidden values are shown without length and tag: %w", err)
}
