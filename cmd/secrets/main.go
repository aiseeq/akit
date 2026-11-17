// Package main implements the secrets registry command.
package main

import (
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Keep the working command name in one place until the final name is chosen.
const commandName = "secrets"

var usage = fmt.Sprintf(`%s — поиск и сверка реестра секретов.

Использование:
  %s find <слово> [--root DIR]
  %s add <файл>… --section <раздел> --what <описание> --from <источник> --where <где ещё> [--root DIR]
  %s check [--root DIR]

Реестр: <root>/LEDGER.md; root по умолчанию — $HOME/secrets.
Строка таблицы: Место | Что | Откуда | Проверка | Где ещё | Статус.
add сам считает sha256[:12], данные сертификата (serial и срок) и
sha256[:12] открытого ключа; проверяет пару сертификата с ключом,
устанавливает файлу права 600 и папке 700. Значения секретов не печатает.
check находит расхождения файлов и строк, широкие права и истекающие
в ближайшие 30 дней сертификаты.
`, commandName, commandName, commandName, commandName)

type config struct{ root string }

type row struct {
	section string
	text    string
	place   string
	rel     string
}

type secretInfo struct {
	path, rel, verification, fileHash string
	data                              []byte
	certificates                      []*x509.Certificate
	publicHashes                      []string
	encrypted                         bool
}

func main() {
	code := execute(os.Args[1:], os.Stdout, os.Stderr, time.Now())
	os.Exit(code)
}

func execute(args []string, out, errOut io.Writer, now time.Time) int {
	cfg, args, err := extractRoot(args)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", commandName, err)
		return 2
	}
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(out, usage)
		return 0
	}
	if len(args) > 1 && subcommandHelp(args[0], args[1:]) {
		fmt.Fprint(out, usage)
		return 0
	}
	if cfg.root == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			fmt.Fprintf(errOut, "%s: home: %v\n", commandName, e)
			return 1
		}
		cfg.root = filepath.Join(home, "secrets")
	}
	cfg.root, err = filepath.Abs(cfg.root)
	if err != nil {
		fmt.Fprintf(errOut, "%s: root: %v\n", commandName, err)
		return 1
	}
	if len(args) == 0 {
		fmt.Fprintf(errOut, "%s: нужна подкоманда (-h для справки)\n", commandName)
		return 2
	}
	switch args[0] {
	case "find":
		return find(cfg, args[1:], out, errOut)
	case "add":
		return add(cfg, args[1:], out, errOut)
	case "check":
		if len(args) != 1 {
			fmt.Fprintf(errOut, "%s: check не принимает аргументы\n", commandName)
			return 2
		}
		return check(cfg, out, errOut, now)
	default:
		fmt.Fprintf(errOut, "%s: неизвестная подкоманда %q (-h для справки)\n", commandName, args[0])
		return 2
	}
}

func subcommandHelp(command string, args []string) bool {
	for i, arg := range args {
		if arg != "-h" && arg != "--help" {
			continue
		}
		if i > 0 && (args[i-1] == "--section" || args[i-1] == "--what" || args[i-1] == "--from" || args[i-1] == "--where") {
			continue
		}
		if command == "find" || command == "check" || command == "add" {
			return true
		}
	}
	return false
}

// --root is extracted before command parsing, so it works after subcommand args.
func extractRoot(args []string) (config, []string, error) {
	var cfg config
	kept := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--root" {
			if cfg.root != "" {
				return cfg, nil, errors.New("--root указан больше одного раза")
			}
			if i+1 == len(args) || args[i+1] == "" {
				return cfg, nil, errors.New("--root требует каталог")
			}
			i++
			cfg.root = args[i]
		} else if strings.HasPrefix(a, "--root=") {
			if cfg.root != "" {
				return cfg, nil, errors.New("--root указан больше одного раза")
			}
			cfg.root = strings.TrimPrefix(a, "--root=")
			if cfg.root == "" {
				return cfg, nil, errors.New("--root требует каталог")
			}
		} else {
			kept = append(kept, a)
		}
	}
	return cfg, kept, nil
}

func readRegistry(root string) ([]byte, []row, error) {
	path := filepath.Join(root, "LEDGER.md")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("чтение %s: %w", path, err)
	}
	if line := firstBareCRLine(b); line != 0 {
		return nil, nil, fmt.Errorf("реестр %s:%d содержит неподдерживаемый одиночный CR", path, line)
	}
	var rows []row
	section := ""
	registryText := strings.TrimPrefix(string(b), "\uFEFF")
	for line := range strings.SplitSeq(registryText, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "## "); ok {
			section = strings.TrimSpace(after)
			continue
		}
		if !strings.HasPrefix(line, "|") || isSeparatorRow(line) || strings.Contains(line, "| Место") {
			continue
		}
		cells := splitRow(line)
		if len(cells) != 6 {
			continue
		}
		place := unmd(strings.TrimSpace(cells[0]))
		rel := ""
		if strings.HasPrefix(place, "`~/secrets/") && strings.HasSuffix(place, "`") {
			rel = strings.TrimSuffix(strings.TrimPrefix(place, "`~/secrets/"), "`")
		}
		rows = append(rows, row{section: section, text: line, place: place, rel: filepath.FromSlash(rel)})
	}
	return b, rows, nil
}

func firstBareCRLine(contents []byte) int {
	line := 1
	for i, b := range contents {
		if b == '\r' && (i+1 == len(contents) || contents[i+1] != '\n') {
			return line
		}
		if b == '\n' {
			line++
		}
	}
	return 0
}

func unmd(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '\\' || s[i+1] == '|') {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isSeparatorRow(line string) bool {
	cells := splitRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		if !strings.Contains(cell, "-") || strings.Trim(cell, ":- ") != "" {
			return false
		}
	}
	return true
}

func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	if len(line) < 2 || line[0] != '|' || line[len(line)-1] != '|' {
		return nil
	}
	var cells []string
	start := 1
	for i := 1; i < len(line)-1; i++ {
		if line[i] == '\\' {
			i++
			continue
		}
		if line[i] == '|' {
			cells = append(cells, strings.TrimSpace(line[start:i]))
			start = i + 1
		}
	}
	cells = append(cells, strings.TrimSpace(line[start:len(line)-1]))
	return cells
}

func find(cfg config, args []string, out, errOut io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(errOut, "%s: find требует одно слово\n", commandName)
		return 2
	}
	_, rows, err := readRegistry(cfg.root)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", commandName, err)
		return 1
	}
	needle := strings.ToLower(args[0])
	count := 0
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.text), needle) {
			fmt.Fprintf(out, "## %s\n%s\n", r.section, r.text)
			count++
		}
	}
	if count == 0 {
		fmt.Fprintln(out, "в реестре нет")
	}
	return 0
}

func add(cfg config, args []string, out, errOut io.Writer) int {
	files, values, err := parseAddArgs(args)
	if err != nil {
		return addError(errOut, err)
	}
	if strings.ContainsAny(values["--section"], "\r\n") {
		return addError(errOut, errors.New("имя раздела не должно содержать перевод строки"))
	}
	values["--section"] = strings.TrimSpace(values["--section"])
	if values["--section"] == "" {
		return addError(errOut, errors.New("имя раздела не должно быть пустым"))
	}
	regPath := filepath.Join(cfg.root, "LEDGER.md")
	content, rows, err := readRegistry(cfg.root)
	if err != nil {
		return addError(errOut, err)
	}
	infos := make([]*secretInfo, 0, len(files))
	seen := map[string]bool{}
	for _, name := range files {
		path, rel, e := confinedPath(cfg.root, name)
		if e != nil {
			return addError(errOut, e)
		}
		if filepath.Clean(rel) == "LEDGER.md" {
			return addError(errOut, errors.New("нельзя добавить сам файл LEDGER.md"))
		}
		if seen[rel] {
			return addError(errOut, fmt.Errorf("файл повторён в аргументах: %s", rel))
		}
		seen[rel] = true
		for _, r := range rows {
			if r.rel == rel {
				return addError(errOut, fmt.Errorf("для файла уже есть строка: %s", rel))
			}
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return addError(errOut, fmt.Errorf("чтение %s: %w", rel, e))
		}
		info := &secretInfo{path: path, rel: rel, data: data}
		fileSum := sha256.Sum256(data)
		info.fileHash = hex.EncodeToString(fileSum[:])[:12]
		info.verification, info.certificates, info.publicHashes, info.encrypted, e = verification(data)
		if e != nil {
			return addError(errOut, fmt.Errorf("%s: %w", rel, e))
		}
		infos = append(infos, info)
	}
	if err := validatePairs(infos); err != nil {
		return addError(errOut, err)
	}
	updated, err := insertRows(content, values["--section"], rowsForAdd(infos, values))
	if err != nil {
		return addError(errOut, err)
	}
	for _, info := range infos {
		if err := os.Chmod(info.path, 0o600); err != nil {
			return addError(errOut, fmt.Errorf("права файла %s: %w", info.rel, err))
		}
		if err := os.Chmod(filepath.Dir(info.path), 0o700); err != nil {
			return addError(errOut, fmt.Errorf("права папки %s: %w", filepath.Dir(info.path), err))
		}
	}
	if err := os.WriteFile(regPath, updated, 0o600); err != nil {
		return addError(errOut, fmt.Errorf("запись %s: %w", regPath, err))
	}
	for _, info := range infos {
		fmt.Fprintf(out, "добавлен %s, длина %d, sha256 %s, проверка %s\n", info.rel, len(info.data), info.fileHash, info.verification)
	}
	return 0
}

func parseAddArgs(args []string) ([]string, map[string]string, error) {
	files := []string{}
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			key, val, eq := strings.Cut(a, "=")
			if !eq {
				if i+1 >= len(args) {
					return nil, nil, fmt.Errorf("%s требует значение", a)
				}
				i++
				val = args[i]
			}
			switch key {
			case "--section", "--what", "--from", "--where":
				if val == "" || values[key] != "" {
					return nil, nil, fmt.Errorf("%s требуется один раз с непустым значением", key)
				}
				values[key] = val
			default:
				return nil, nil, fmt.Errorf("неизвестный флаг %q", key)
			}
		} else {
			files = append(files, a)
		}
	}
	for _, key := range []string{"--section", "--what", "--from", "--where"} {
		if values[key] == "" {
			return nil, nil, fmt.Errorf("обязателен %s", key)
		}
	}
	if len(files) == 0 {
		return nil, nil, errors.New("нужен хотя бы один файл")
	}
	return files, values, nil
}

func rowsForAdd(infos []*secretInfo, values map[string]string) string {
	var b strings.Builder
	for _, info := range infos {
		b.WriteString(fmt.Sprintf("| `~/secrets/%s` | %s | %s | %s | %s | действует |\n", md(filepath.ToSlash(info.rel)), md(values["--what"]), md(values["--from"]), md(info.verification), md(values["--where"])))
	}
	return b.String()
}

func addError(w io.Writer, err error) int { fmt.Fprintf(w, "%s: %v\n", commandName, err); return 1 }

func confinedPath(root, name string) (string, string, error) {
	path := name
	path, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve file path: %w", err)
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("root %s: %w", root, err)
	}
	fileReal, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", fmt.Errorf("файл %s: %w", path, err)
	}
	rel, err := filepath.Rel(rootReal, fileReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("файл вне root: %s", name)
	}
	st, err := os.Stat(fileReal)
	if err != nil {
		return "", "", fmt.Errorf("stat %s: %w", fileReal, err)
	}
	if !st.Mode().IsRegular() {
		return "", "", fmt.Errorf("не обычный файл: %s", rel)
	}
	return fileReal, rel, nil
}

func verification(data []byte) (string, []*x509.Certificate, []string, bool, error) {
	h := sha256.Sum256(data)
	base := "sha256 " + hex.EncodeToString(h[:])[:12]
	parsed, err := inspectPEM(data)
	if err != nil {
		return "", nil, nil, false, err
	}
	if parsed.encrypted {
		return base + "; ключ зашифрован, sha открытого ключа не посчитан", parsed.certificates, nil, true, nil
	}
	if len(parsed.certificates) > 0 {
		checks := make([]string, 0, len(parsed.certificates))
		for _, cert := range parsed.certificates {
			serial := strings.ToUpper(cert.SerialNumber.Text(16))
			if len(serial)%2 != 0 {
				serial = "0" + serial
			}
			checks = append(checks, fmt.Sprintf("serial %s; до %s", serial, cert.NotAfter.Format(time.DateOnly)))
		}
		checks = append(checks, publicKeyChecks(parsed.publicHashes)...)
		return strings.Join(checks, "; "), parsed.certificates, parsed.publicHashes, false, nil
	}
	if len(parsed.publicHashes) > 0 {
		return strings.Join(publicKeyChecks(parsed.publicHashes), "; "), nil, parsed.publicHashes, false, nil
	}
	return base, nil, nil, false, nil
}

func publicKeyChecks(hashes []string) []string {
	checks := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		checks = append(checks, "sha256 публичного ключа "+hash[:12])
	}
	return checks
}

type pemDetails struct {
	certificates []*x509.Certificate
	publicHashes []string
	encrypted    bool
	keyBlocks    int
}

func pemBeginMarker(kind string) string { return "-----BEGIN " + kind + "-----" }

func inspectPEM(data []byte) (pemDetails, error) {
	var details pemDetails
	rest := data
	for len(rest) > 0 {
		block, tail := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = tail
		cert, publicHash, encrypted, keyBlock, err := parsePEMBlock(block)
		if err != nil {
			return pemDetails{}, err
		}
		if cert != nil {
			details.certificates = append(details.certificates, cert)
		}
		if publicHash != "" {
			details.publicHashes = append(details.publicHashes, publicHash)
		}
		details.encrypted = details.encrypted || encrypted
		if keyBlock {
			details.keyBlocks++
		}
	}
	raw := string(data)
	if strings.Count(raw, pemBeginMarker("CERTIFICATE")) > len(details.certificates) {
		return pemDetails{}, errors.New("не удалось разобрать CERTIFICATE PEM")
	}
	keyMarkers := strings.Count(raw, pemBeginMarker("PRIVATE KEY")) + strings.Count(raw, pemBeginMarker("ENCRYPTED PRIVATE KEY")) + strings.Count(raw, pemBeginMarker("RSA PRIVATE KEY")) + strings.Count(raw, pemBeginMarker("EC PRIVATE KEY"))
	if keyMarkers > details.keyBlocks {
		return pemDetails{}, errors.New("не удалось разобрать закрытый ключ PEM")
	}
	return details, nil
}

func parsePEMBlock(block *pem.Block) (*x509.Certificate, string, bool, bool, error) {
	switch block.Type {
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, "", false, false, fmt.Errorf("не удалось разобрать CERTIFICATE: %w", err)
		}
		return cert, "", false, false, nil
	case "ENCRYPTED PRIVATE KEY":
		return nil, "", true, true, nil
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, "", false, true, fmt.Errorf("не удалось разобрать PRIVATE KEY: %w", err)
		}
		hash, err := hashPublicKey(key)
		return nil, hash, false, true, err
	case "RSA PRIVATE KEY":
		if x509.IsEncryptedPEMBlock(block) {
			return nil, "", true, true, nil
		}
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, "", false, true, fmt.Errorf("не удалось разобрать RSA PRIVATE KEY: %w", err)
		}
		hash, err := hashPublicKey(key)
		return nil, hash, false, true, err
	case "EC PRIVATE KEY":
		if x509.IsEncryptedPEMBlock(block) {
			return nil, "", true, true, nil
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, "", false, true, fmt.Errorf("не удалось разобрать EC PRIVATE KEY: %w", err)
		}
		hash, err := hashPublicKey(key)
		return nil, hash, false, true, err
	default:
		return nil, "", false, false, nil
	}
}

func hashPublicKey(key any) (string, error) {
	privateKey, ok := key.(interface{ Public() crypto.PublicKey })
	if !ok {
		return "", errors.New("неподдерживаемый закрытый ключ")
	}
	der, err := x509.MarshalPKIXPublicKey(privateKey.Public())
	if err != nil {
		return "", fmt.Errorf("не удалось получить открытый ключ: %w", err)
	}
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:]), nil
}

func validatePairs(infos []*secretInfo) error {
	var certs []*x509.Certificate
	var encrypted bool
	for _, info := range infos {
		certs = append(certs, info.certificates...)
		encrypted = encrypted || info.encrypted
	}
	if len(certs) > 0 && encrypted {
		return errors.New("ключ зашифрован, sha открытого ключа не посчитан; пару сертификата проверить нельзя")
	}
	for _, keyFile := range infos {
		hashes := keyFile.publicHashes
		if len(hashes) == 0 {
			continue
		}
		candidates := certs
		if len(keyFile.certificates) > 0 {
			candidates = keyFile.certificates
		}
		if len(candidates) == 0 {
			continue
		}
		for _, publicHash := range hashes {
			matched := false
			for _, cert := range candidates {
				der, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
				if err != nil {
					return fmt.Errorf("%s: открытый ключ сертификата не читается: %w", keyFile.rel, err)
				}
				h := sha256.Sum256(der)
				if publicHash == hex.EncodeToString(h[:]) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("сертификат и закрытый ключ не пара: %s", keyFile.rel)
			}
		}
	}
	return nil
}

func md(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
}

func insertRows(content []byte, section, rows string) ([]byte, error) {
	text := string(content)
	lines := splitRegistryLines(text)
	if len(lines) > 0 {
		lines[0].body = strings.TrimPrefix(lines[0].body, "\uFEFF")
	}
	sectionName := strings.TrimSpace(section)
	sectionAt, separatorAt := -1, -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line.body)
		if after, ok := strings.CutPrefix(trimmed, "## "); ok {
			if sectionAt >= 0 {
				break
			}
			if strings.TrimSpace(after) == sectionName {
				sectionAt = i
			}
			continue
		}
		if sectionAt >= 0 && separatorAt < 0 && isSeparatorRow(line.body) {
			separatorAt = i
		}
	}
	if sectionAt < 0 {
		eol := majorityEOL(lines)
		prefix := text
		if prefix != "" {
			if !strings.HasSuffix(prefix, "\n") {
				prefix += eol
			}
			if !endsWithBlankLine(prefix) {
				prefix += eol
			}
		}
		addition := strings.Join([]string{"## " + sectionName, "", "| Место | Что | Откуда | Проверка | Где ещё | Статус |", "|---|---|---|---|---|---|"}, eol)
		return []byte(prefix + addition + eol + rowsWithEOL(rows, eol)), nil
	}
	if separatorAt < 0 {
		return nil, errors.New("в разделе нет таблицы")
	}
	line := lines[separatorAt]
	eol := line.eol
	if eol == "" {
		eol = neighboringEOL(lines, separatorAt)
	}
	insertAt := line.end
	prefix := ""
	if line.eol == "" {
		prefix = eol
	}
	inserted := prefix + rowsWithEOL(rows, eol)
	return []byte(text[:insertAt] + inserted + text[insertAt:]), nil
}

type registryLine struct {
	body, eol string
	end       int
}

func splitRegistryLines(text string) []registryLine {
	var lines []registryLine
	for start := 0; start < len(text); {
		relativeLF := strings.IndexByte(text[start:], '\n')
		if relativeLF < 0 {
			lines = append(lines, registryLine{body: text[start:], end: len(text)})
			break
		}
		lf := start + relativeLF
		body, eol := text[start:lf], "\n"
		if before, ok := strings.CutSuffix(body, "\r"); ok {
			body, eol = before, "\r\n"
		}
		end := lf + 1
		lines = append(lines, registryLine{body: body, eol: eol, end: end})
		start = end
	}
	return lines
}

func majorityEOL(lines []registryLine) string {
	counts := map[string]int{"\n": 0, "\r\n": 0}
	first := ""
	for _, line := range lines {
		if line.eol != "" {
			counts[line.eol]++
			if first == "" {
				first = line.eol
			}
		}
	}
	if counts["\r\n"] > counts["\n"] {
		return "\r\n"
	}
	if counts["\n"] > counts["\r\n"] {
		return "\n"
	}
	if first != "" {
		return first
	}
	return "\n"
}

func neighboringEOL(lines []registryLine, index int) string {
	if index > 0 && lines[index-1].eol != "" {
		return lines[index-1].eol
	}
	return majorityEOL(lines)
}

func rowsWithEOL(rows, eol string) string {
	rows = strings.TrimSuffix(rows, "\n")
	return strings.ReplaceAll(rows, "\n", eol) + eol
}

func endsWithBlankLine(text string) bool {
	if !strings.HasSuffix(text, "\n") {
		return false
	}
	text = strings.TrimSuffix(text, "\n")
	text = strings.TrimSuffix(text, "\r")
	return strings.HasSuffix(text, "\n")
}

func check(cfg config, out, errOut io.Writer, now time.Time) int {
	return checkWithStat(cfg, out, errOut, now, os.Stat)
}

func checkWithStat(cfg config, out, errOut io.Writer, now time.Time, stat func(string) (os.FileInfo, error)) int {
	_, rows, err := readRegistry(cfg.root)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", commandName, err)
		return 1
	}
	realRoot, err := filepath.EvalSymlinks(cfg.root)
	if err != nil {
		fmt.Fprintf(errOut, "%s: root %s: %v\n", commandName, cfg.root, err)
		return 1
	}
	known := map[string]bool{}
	missing := 0
	for _, r := range rows {
		if r.rel != "" {
			known[filepath.Clean(r.rel)] = true
			path := filepath.Join(cfg.root, r.rel)
			if _, e := stat(path); e != nil {
				if !os.IsNotExist(e) {
					fmt.Fprintf(out, "файл строки недоступен: %s (%v)\n", r.place, e)
					missing++
					continue
				}
				fmt.Fprintf(out, "строка без файла: %s\n", r.place)
				missing++
			}
		}
	}
	found, err := scanRoot(realRoot, known, out, now)
	if err != nil {
		fmt.Fprintf(errOut, "%s: обход root: %v\n", commandName, err)
		return 1
	}
	if missing+found > 0 {
		return 1
	}
	fmt.Fprintln(out, "реестр в порядке")
	return 0
}

func scanRoot(root string, known map[string]bool, out io.Writer, now time.Time) (int, error) {
	return scanRootWithReader(root, known, out, now, os.ReadFile)
}

func scanRootWithReader(root string, known map[string]bool, out io.Writer, now time.Time, readFile func(string) ([]byte, error)) (int, error) {
	return scanRootWithFS(root, known, out, now, readFile, os.Stat, func(_ string, d os.DirEntry) (os.FileInfo, error) {
		return d.Info()
	})
}

func scanRootWithFS(root string, known map[string]bool, out io.Writer, now time.Time, readFile func(string) ([]byte, error), stat func(string) (os.FileInfo, error), entryInfo func(string, os.DirEntry) (os.FileInfo, error)) (int, error) {
	findings := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			fmt.Fprintf(out, "недоступно при обходе: %s (%v)\n", path, walkErr)
			findings++
			return nil
		}
		if d.IsDir() {
			st, e := stat(path)
			if e != nil {
				fmt.Fprintf(out, "папка недоступна: %s (%v)\n", path, e)
				findings++
				return nil
			}
			if st.Mode().Perm()&0o500 != 0o500 {
				fmt.Fprintf(out, "папка недоступна: %s (нет прав чтения или входа)\n", path)
				findings++
			}
			if st.Mode().Perm()&0o077 != 0 {
				fmt.Fprintf(out, "права папки шире 700: %s\n", path)
				findings++
			}
			return nil
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		if rel == "LEDGER.md" {
			return nil
		}
		st, e := entryInfo(path, d)
		if e != nil {
			fmt.Fprintf(out, "метаданные недоступны: %s (%v)\n", path, e)
			findings++
			return nil
		}
		if st.Mode().IsRegular() {
			if st.Mode().Perm()&0o177 != 0 {
				fmt.Fprintf(out, "права файла шире 600: %s\n", path)
				findings++
			}
			if !known[filepath.Clean(rel)] {
				fmt.Fprintf(out, "файл без строки: %s\n", path)
				findings++
			}
			if st.Mode().Perm()&0o400 == 0 {
				fmt.Fprintf(out, "файл не читается: %s (нет чтения владельцем)\n", path)
				findings++
				return nil
			}
			b, e := readFile(path)
			if e != nil {
				fmt.Fprintf(out, "файл не читается: %s (%v)\n", path, e)
				findings++
				return nil
			}
			_, certs, _, _, e := verification(b)
			if e == nil {
				for _, cert := range certs {
					if !cert.NotAfter.After(now.Add(30 * 24 * time.Hour)) {
						fmt.Fprintf(out, "сертификат истекает в ближайшие 30 дней: %s (%s)\n", path, cert.NotAfter.Format(time.DateOnly))
						findings++
					}
				}
			}
		}
		return nil
	})
	return findings, err
}
