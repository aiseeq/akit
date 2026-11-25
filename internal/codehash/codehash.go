// Package codehash computes hashes for Go package source dependencies.
package codehash

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go list -deps: %w: %s", err, strings.TrimSpace(string(out)))
	}
	goEnv := exec.Command("go", "env", "GOMOD")
	goEnv.Dir = packageDir
	gomod, err := goEnv.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	modJSON, err := exec.Command("go", "mod", "edit", "-json", strings.TrimSpace(string(gomod))).Output()
	if err != nil {
		return "", fmt.Errorf("go mod edit -json: %w", err)
	}
	var mod moduleFile
	if err := json.Unmarshal(modJSON, &mod); err != nil {
		return "", fmt.Errorf("parse go.mod: %w", err)
	}
	hash := sha256.New()
	write := func(format string, args ...any) { fmt.Fprintf(hash, format, args...) }
	write("go %s\ntoolchain %s\n", mod.Go, mod.Toolchain)
	for _, gd := range mod.Godebug {
		write("godebug %s=%s\n", gd.Key, gd.Value)
	}
	for _, r := range mod.Replace {
		write("replace %s@%s => %s@%s\n", r.Old.Path, r.Old.Version, r.New.Path, r.New.Version)
	}
	if err := hashPackages(hash, out, packageDir, excludedFile); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashPackages(hash interface{ Write([]byte) (int, error) }, out []byte, packageDir, excludedFile string) error {
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 8 {
			return fmt.Errorf("unexpected go list line %q", line)
		}
		modPath, version, root, replacement, replacementVersion, replacementDir, dir, files := fields[0], fields[1], fields[2], fields[3], fields[4], fields[5], fields[6], fields[7]
		if replacement != "" && replacementVersion != "" || replacement == "" && version != "" {
			fmt.Fprintf(hash, "module %s@%s => %s@%s\n", modPath, version, replacement, replacementVersion)
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

func hashPackageFiles(hash interface{ Write([]byte) (int, error) }, files, modPath, rel, dir, packageDir, excludedFile string) error {
	for name := range strings.SplitSeq(files, ",") {
		if name == "" || strings.HasSuffix(name, "_test.go") || dir == packageDir && name == excludedFile {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.Join(dir, name), err)
		}
		fmt.Fprintf(hash, "file %s/%s %d\n", modPath, filepath.ToSlash(filepath.Join(rel, name)), len(data))
		if _, err := hash.Write(data); err != nil {
			return fmt.Errorf("hash %s: %w", filepath.Join(dir, name), err)
		}
	}
	return nil
}
