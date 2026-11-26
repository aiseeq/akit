// Package codehash computes hashes for Go package source dependencies.
package codehash

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// IgnoredGoFiles and EmbedFiles are included because build-tagged source and embedded data can affect builds.
const listFormat = `{{if .Module}}{{.Module.Path}}\t{{.Module.Version}}\t{{.Module.Dir}}\t` +
	`{{with .Module.Replace}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{else}}\t\t{{end}}\t{{.Dir}}\t` +
	`{{join .GoFiles ","}},{{join .CgoFiles ","}},{{join .IgnoredGoFiles ","}},{{join .EmbedFiles ","}},` +
	`{{join .CFiles ","}},{{join .HFiles ","}},{{join .SFiles ","}},{{join .SysoFiles ","}}{{end}}`

type moduleFile struct {
	Go        string
	Toolchain string
	Godebug   []struct {
		Key   string
		Value string
	}
	Replace []struct {
		Old struct {
			Path    string
			Version string
		}
		New struct {
			Path    string
			Version string
		}
	}
}

// Hash returns a hash of the package's non-test source dependencies, dependency
// module versions, local replacement files, and build-affecting go.mod directives.
// excludedFile is omitted by basename from the package at packageDir.
func Hash(packageDir, excludedFile string) (string, error) {
	var err error
	packageDir, err = filepath.Abs(packageDir)
	if err != nil {
		return "", fmt.Errorf("absolute package path: %w", err)
	}
	formatString := strings.ReplaceAll(listFormat, `\t`, "\t")
	cmd := exec.Command("go", "list", "-deps", "-f", formatString, ".")
	cmd.Dir = packageDir
	out, err := cmd.Output()
	if err != nil {
		return "", commandError("go list -deps", err)
	}
	goEnv := exec.Command("go", "env", "GOMOD")
	goEnv.Dir = packageDir
	gomod, err := goEnv.Output()
	if err != nil {
		return "", commandError("go env GOMOD", err)
	}
	modEdit := exec.Command("go", "mod", "edit", "-json", strings.TrimSpace(string(gomod)))
	modEdit.Dir = packageDir
	modJSON, err := modEdit.Output()
	if err != nil {
		return "", commandError("go mod edit -json", err)
	}
	var mod moduleFile
	if err := json.Unmarshal(modJSON, &mod); err != nil {
		return "", fmt.Errorf("parse go.mod: %w", err)
	}
	hash := sha256.New()
	if err := writeHash(hash, "go %s\ntoolchain %s\n", mod.Go, mod.Toolchain); err != nil {
		return "", err
	}
	for _, gd := range mod.Godebug {
		if err := writeHash(hash, "godebug %s=%s\n", gd.Key, gd.Value); err != nil {
			return "", err
		}
	}
	for _, r := range mod.Replace {
		if err := writeHash(hash, "replace %s@%s => %s@%s\n", r.Old.Path, r.Old.Version, r.New.Path, r.New.Version); err != nil {
			return "", err
		}
	}
	if err := hashPackages(hash, out, packageDir, excludedFile); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func commandError(name string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) != 0 {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return fmt.Errorf("%s: %w", name, err)
}

func writeHash(w io.Writer, format string, args ...any) error {
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		return fmt.Errorf("write code hash: %w", err)
	}
	return nil
}

func hashPackages(hash io.Writer, out []byte, packageDir, excludedFile string) error {
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 8 {
			return fmt.Errorf("unexpected go list line %q", line)
		}
		modPath, version, root, replacement, replacementVersion, replacementDir, dir, files := fields[0], fields[1], fields[2], fields[3], fields[4], fields[5], fields[6], fields[7]
		if replacement != "" && replacementVersion != "" || replacement == "" && version != "" {
			if err := writeHash(hash, "module %s@%s => %s@%s\n", modPath, version, replacement, replacementVersion); err != nil {
				return err
			}
			continue
		}
		if replacement != "" {
			root = replacementDir
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return fmt.Errorf("relative package path: %w", err)
		}
		if err := hashPackageFiles(hash, files, modPath, rel, dir, packageDir, excludedFile); err != nil {
			return err
		}
	}
	return nil
}

func hashPackageFiles(hash io.Writer, files, modPath, rel, dir, packageDir, excludedFile string) error {
	for name := range strings.SplitSeq(files, ",") {
		if name == "" || strings.HasSuffix(name, "_test.go") || dir == packageDir && name == excludedFile {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.Join(dir, name), err)
		}
		if err := writeHash(hash, "file %s/%s %d\n", modPath, filepath.ToSlash(filepath.Join(rel, name)), len(data)); err != nil {
			return err
		}
		if _, err := hash.Write(data); err != nil {
			return fmt.Errorf("hash %s: %w", filepath.Join(dir, name), err)
		}
	}
	return nil
}
