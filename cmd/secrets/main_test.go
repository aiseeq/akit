package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testHeader = "# Registry\n\n## Alpha\n\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\n|---|---|---|---|---|---|\n| `~/secrets/other/present.txt` | Alpha API token | team | sha256 abcd | — | действует |\n| `~/secrets/other/missing.txt` | old alpha | team | sha256 dead | — | действует |\n\n## Beta\n\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\n|---|---|---|---|---|---|\n| — | Beta webhook secret | — | — | — | инвентаризация |\n"

func setup(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "secrets")
	for _, d := range []string{root, filepath.Join(root, "other"), filepath.Join(root, "tls")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "LEDGER.md"), []byte(testHeader), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other", "present.txt"), []byte("safe synthetic value"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func invokeTest(root string, now time.Time, args ...string) (int, string, string) {
	args = append(args, "--root", root)
	var out, stderr bytes.Buffer
	code := execute(args, &out, &stderr, now)
	return code, out.String(), stderr.String()
}

func writeTestFile(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatal(err)
	}
}

func chmodTest(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func removeTest(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFindMatchesCaseInsensitiveAndInventory(t *testing.T) {
	root := setup(t)
	for _, tc := range []struct{ word, want string }{{"aLpHa", "present.txt"}, {"not-present", "в реестре нет"}, {"webhook", "инвентаризация"}} {
		code, out, err := invokeTest(root, time.Now(), "find", tc.word)
		if code != 0 || !strings.Contains(out, tc.want) || err != "" {
			t.Errorf("find %q: code=%d out=%q err=%q", tc.word, code, out, err)
		}
	}
}

func TestAddCreatesPrivateFileAndSectionRowWithoutValue(t *testing.T) {
	root := setup(t)
	path := filepath.Join(root, "svc", "token.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	value := "synthetic-secret-value-xyz"
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, err := invokeTest(root, time.Now(), "add", path, "--section", "Gamma", "--what", "Gamma token", "--from", "test source", "--where", "—")
	if code != 0 || err != "" {
		t.Fatalf("add: code=%d out=%q err=%q", code, out, err)
	}
	if strings.Contains(out, value) {
		t.Fatal("secret value printed")
	}
	st, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode=%o", st.Mode().Perm())
	}
	st, statErr = os.Stat(filepath.Dir(path))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode=%o", st.Mode().Perm())
	}
	b := readTestFile(t, filepath.Join(root, "LEDGER.md"))
	sum := sha256Bytes([]byte(value))
	if !strings.Contains(string(b), "## Gamma") || !strings.Contains(string(b), "svc/token.txt") || !strings.Contains(string(b), sum[:12]) || strings.Contains(string(b), value) {
		t.Fatalf("unexpected registry: %s", b)
	}
	if !strings.Contains(out, sum[:12]) || !strings.Contains(out, "длина") {
		t.Fatalf("output omitted safe metadata: %q", out)
	}
	code, _, duplicateErr := invokeTest(root, time.Now(), "add", path, "--section", "Gamma", "--what", "again", "--from", "x", "--where", "—")
	if code == 0 {
		t.Fatalf("duplicate add succeeded: %s", duplicateErr)
	}
	count := bytes.Count(b, []byte("svc/token.txt"))
	again := readTestFile(t, filepath.Join(root, "LEDGER.md"))
	if bytes.Count(again, []byte("svc/token.txt")) != count {
		t.Fatal("duplicate add changed row count")
	}
}

func sha256Bytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestAddRejectsFileOutsideRoot(t *testing.T) {
	root := setup(t)
	outside := filepath.Join(t.TempDir(), "outside")
	writeTestFile(t, outside, []byte("x"), 0o600)
	code, _, err := invokeTest(root, time.Now(), "add", outside, "--section", "Alpha", "--what", "x", "--from", "x", "--where", "—")
	if code == 0 || !strings.Contains(err, "вне root") {
		t.Fatalf("code=%d err=%q", code, err)
	}
}

func makePEMPair(t *testing.T, dir, name string, expiry time.Time) (string, string, *rsa.PrivateKey) {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(0x23c6), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: expiry, KeyUsage: x509.KeyUsageDigitalSignature}
	der, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	cert := filepath.Join(dir, name+".pem")
	kp := filepath.Join(dir, name+".key")
	if e = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); e != nil {
		t.Fatal(e)
	}
	return cert, kp, key
}

func TestAddPEMCertificateKeyPairAndMismatchAreAtomic(t *testing.T) {
	root := setup(t)
	aCert, aKey, _ := makePEMPair(t, filepath.Join(root, "tls"), "a", time.Now().Add(400*24*time.Hour))
	_, bKey, _ := makePEMPair(t, filepath.Join(root, "tls"), "b", time.Now().Add(10*24*time.Hour))
	cCert, _, _ := makePEMPair(t, filepath.Join(root, "tls"), "c", time.Now().Add(400*24*time.Hour))
	code, out, err := invokeTest(root, time.Now(), "add", aCert, aKey, "--section", "Gamma", "--what", "mTLS", "--from", "test", "--where", "—")
	if code != 0 || err != "" || strings.Contains(out, "BEGIN") {
		t.Fatalf("pair add: code=%d out=%q err=%q", code, out, err)
	}
	b := readTestFile(t, filepath.Join(root, "LEDGER.md"))
	if !strings.Contains(string(b), "serial 23C6") || !strings.Contains(string(b), time.Now().Add(400*24*time.Hour).Format(time.DateOnly)) || !strings.Contains(string(b), "sha256 публичного ключа") {
		t.Fatalf("missing certificate/key checks: %s", b)
	}
	before := string(b)
	code, _, err = invokeTest(root, time.Now(), "add", cCert, bKey, "--section", "Gamma", "--what", "wrong", "--from", "test", "--where", "—")
	if code == 0 || !strings.Contains(err, "не пара") {
		t.Fatalf("mismatch accepted: code=%d err=%q", code, err)
	}
	after := readTestFile(t, filepath.Join(root, "LEDGER.md"))
	if string(after) != before {
		t.Fatal("mismatched pair changed registry")
	}
	if strings.Contains(string(after), "c.pem") || strings.Contains(string(after), "b.key") {
		t.Fatal("mismatched pair wrote a row")
	}
}

func TestCheckDistinguishesInaccessibleRegistryFileFromMissing(t *testing.T) {
	root := setup(t)
	registryPath := filepath.Join(root, "LEDGER.md")
	blocked := filepath.Join(root, "other", "present.txt")
	var out, errOut bytes.Buffer
	stat := func(path string) (os.FileInfo, error) {
		if path == blocked {
			return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrPermission}
		}
		return os.Stat(path)
	}
	code := checkWithStat(config{root: root}, &out, &errOut, time.Now(), stat)
	if code != 1 || errOut.Len() != 0 || !strings.Contains(out.String(), "файл строки недоступен: `~/secrets/other/present.txt`") {
		t.Fatalf("inaccessible row target: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), "строка без файла: `~/secrets/other/present.txt`") {
		t.Fatalf("permission failure mislabeled as missing: %q", out.String())
	}
	if !strings.Contains(out.String(), "строка без файла: `~/secrets/other/missing.txt`") {
		t.Fatalf("actual missing file was not reported: %q", out.String())
	}
	if _, err := os.Stat(registryPath); err != nil {
		t.Fatal(err)
	}
}

func TestEscapedMarkdownFieldsRoundTripForFindCheckAndDuplicate(t *testing.T) {
	root := setup(t)
	path := filepath.Join(root, "svc", "pipe.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, []byte("synthetic"), 0o600)
	code, _, err := invokeTest(root, time.Now(), "add", path, "--section", "Alpha", "--what", "a|b\nc", "--from", `x\|y`, "--where", `z\`)
	if code != 0 {
		t.Fatalf("add escaped fields: code=%d err=%q", code, err)
	}
	_, rows, e := readRegistry(root)
	if e != nil {
		t.Fatal(e)
	}
	var found *row
	for i := range rows {
		if strings.Contains(rows[i].text, "pipe.txt") {
			found = &rows[i]
		}
	}
	if found == nil || len(splitRow(found.text)) != 6 {
		t.Fatalf("escaped row was not parsed as six cells: %#v", found)
	}
	code, out, err := invokeTest(root, time.Now(), "find", "pipe")
	if code != 0 || !strings.Contains(out, "pipe.txt") {
		t.Fatalf("find escaped row: code=%d out=%q err=%q", code, out, err)
	}
	code, _, err = invokeTest(root, time.Now(), "add", path, "--section", "Alpha", "--what", "again", "--from", "x", "--where", "—")
	if code == 0 || !strings.Contains(err, "уже есть строка") {
		t.Fatalf("duplicate escaped row accepted: code=%d err=%q", code, err)
	}
	code, out, err = invokeTest(root, time.Now(), "check")
	if code != 1 || strings.Contains(out, "svc/pipe.txt") || err != "" {
		t.Fatalf("check did not recognize escaped row: code=%d out=%q err=%q", code, out, err)
	}
}

func TestEscapedPathsRoundTripForRepeatAndCheck(t *testing.T) {
	root := setup(t)
	writeTestFile(t, filepath.Join(root, "other", "missing.txt"), []byte("synthetic"), 0o600)
	for _, name := range []string{"pipe|name", `back\slash`} {
		path := filepath.Join(root, "other", name)
		writeTestFile(t, path, []byte("synthetic"), 0o600)
		args := []string{"add", path, "--section", "Alpha", "--what", "path", "--from", "test", "--where", "—"}
		if code, _, err := invokeTest(root, time.Now(), args...); code != 0 || err != "" {
			t.Fatalf("add %q: code=%d err=%q", name, code, err)
		}
		if code, _, err := invokeTest(root, time.Now(), args...); code == 0 || !strings.Contains(err, "уже есть строка") {
			t.Fatalf("duplicate %q: code=%d err=%q", name, code, err)
		}
	}
	if code, out, err := invokeTest(root, time.Now(), "check"); code != 0 || !strings.Contains(out, "в порядке") || err != "" {
		t.Fatalf("check escaped paths: code=%d out=%q err=%q", code, out, err)
	}
}

func TestAddBundleVerifiesInlineKeyAndWritesItsHash(t *testing.T) {
	root := setup(t)
	cert, key, private := makePEMPair(t, filepath.Join(root, "tls"), "bundle-own", time.Now().Add(400*24*time.Hour))
	bundle := filepath.Join(root, "tls", "bundle.pem")
	writeTestFile(t, bundle, append(readTestFile(t, cert), readTestFile(t, key)...), 0o600)
	der, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(der)
	wantHash := hex.EncodeToString(h[:])[:12]
	code, _, errText := invokeTest(root, time.Now(), "add", bundle, "--section", "Bundle", "--what", "pair", "--from", "test", "--where", "—")
	if code != 0 || errText != "" {
		t.Fatalf("matching bundle: code=%d err=%q", code, errText)
	}
	if got := string(readTestFile(t, filepath.Join(root, "LEDGER.md"))); !strings.Contains(got, wantHash) {
		t.Fatalf("inline public hash %s omitted: %s", wantHash, got)
	}
	_, foreign, _ := makePEMPair(t, filepath.Join(root, "tls"), "bundle-foreign", time.Now().Add(400*24*time.Hour))
	bad := filepath.Join(root, "tls", "bad-bundle.pem")
	writeTestFile(t, bad, append(readTestFile(t, cert), readTestFile(t, foreign)...), 0o600)
	before := readTestFile(t, filepath.Join(root, "LEDGER.md"))
	code, _, errText = invokeTest(root, time.Now(), "add", bad, "--section", "Bundle", "--what", "bad", "--from", "test", "--where", "—")
	if code == 0 || !strings.Contains(errText, "не пара") {
		t.Fatalf("foreign inline key accepted: code=%d err=%q", code, errText)
	}
	if !bytes.Equal(before, readTestFile(t, filepath.Join(root, "LEDGER.md"))) {
		t.Fatal("foreign inline key changed registry")
	}
	ecCert, ecKey, _ := makeECPEMPair(t, filepath.Join(root, "tls"), "bundle-ec")
	wrongLocalBundle := filepath.Join(root, "tls", "wrong-local.pem")
	writeTestFile(t, wrongLocalBundle, append(readTestFile(t, cert), readTestFile(t, ecKey)...), 0o600)
	before = readTestFile(t, filepath.Join(root, "LEDGER.md"))
	code, _, errText = invokeTest(root, time.Now(), "add", wrongLocalBundle, ecCert, "--section", "Bundle", "--what", "bad local pair", "--from", "test", "--where", "—")
	if code == 0 || !strings.Contains(errText, "не пара") {
		t.Fatalf("bundle key matched a certificate from a different file: code=%d err=%q", code, errText)
	}
	if !bytes.Equal(before, readTestFile(t, filepath.Join(root, "LEDGER.md"))) {
		t.Fatal("bundle with a mismatched local certificate changed registry")
	}
	foreignEC := filepath.Join(root, "tls", "bundle-foreign-ec.key")
	_, foreignECKey, _ := makeECPEMPair(t, filepath.Join(root, "tls"), "bundle-foreign-ec")
	writeTestFile(t, foreignEC, readTestFile(t, foreignECKey), 0o600)
	for name, parts := range map[string][][]byte{
		"two-keys-own-first.pem": {readTestFile(t, cert), readTestFile(t, key), readTestFile(t, foreignEC)},
		"two-keys-own-last.pem":  {readTestFile(t, cert), readTestFile(t, foreignEC), readTestFile(t, key)},
	} {
		path := filepath.Join(root, "tls", name)
		var bundleBytes []byte
		for _, part := range parts {
			bundleBytes = append(bundleBytes, part...)
		}
		writeTestFile(t, path, bundleBytes, 0o600)
		before = readTestFile(t, filepath.Join(root, "LEDGER.md"))
		code, _, errText = invokeTest(root, time.Now(), "add", path, "--section", "Bundle", "--what", "two keys", "--from", "test", "--where", "—")
		if code == 0 || !strings.Contains(errText, "не пара") {
			t.Errorf("bundle with a foreign key %s accepted: code=%d err=%q", name, code, errText)
		}
		if !bytes.Equal(before, readTestFile(t, filepath.Join(root, "LEDGER.md"))) {
			t.Errorf("bundle %s changed registry after pair failure", name)
		}
	}
}

func TestMalformedPKCS8AndNonBase64PrivateKeyAreRejected(t *testing.T) {
	root := setup(t)
	for name, contents := range map[string][]byte{
		"bad-der.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}),
		"bad-text.pem": []byte(pemBeginMarker("PRIVATE KEY") + "\nnot base64!\n" + strings.Replace(pemBeginMarker("PRIVATE KEY"), "BEGIN", "END", 1) + "\n"),
	} {
		path := filepath.Join(root, "tls", name)
		writeTestFile(t, path, contents, 0o600)
		code, _, err := invokeTest(root, time.Now(), "add", path, "--section", "Broken", "--what", "key", "--from", "test", "--where", "—")
		if code == 0 || !strings.Contains(err, "разобрать") {
			t.Errorf("%s accepted: code=%d err=%q", name, code, err)
		}
	}
}

func TestCheckFindsOwnerUnreadableFileAndDirectoryWithoutStopping(t *testing.T) {
	root := setup(t)
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	chmodTest(t, locked, 0o000)
	file := filepath.Join(root, "other", "owner-unreadable.txt")
	writeTestFile(t, file, []byte("synthetic"), 0o600)
	chmodTest(t, file, 0o040)
	var out bytes.Buffer
	count, err := scanRootWithReader(root, map[string]bool{}, &out, time.Now(), func(string) ([]byte, error) {
		return []byte("synthetic readable data"), nil
	})
	if err != nil || count == 0 || !strings.Contains(out.String(), "папка недоступна: "+locked) || !strings.Contains(out.String(), "файл не читается: "+file) || !strings.Contains(out.String(), "present.txt") {
		t.Fatalf("owner mode or later finding missed: count=%d out=%q err=%v", count, out.String(), err)
	}
}

func TestCheckDirectoryStatErrorIsFindingAndContinues(t *testing.T) {
	root := setup(t)
	blocked := filepath.Join(root, "rd", "sub")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	later := filepath.Join(root, "z", "later")
	if err := os.MkdirAll(filepath.Dir(later), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, later, []byte("synthetic"), 0o600)
	var out bytes.Buffer
	count, err := scanRootWithFS(root, map[string]bool{}, &out, time.Now(), func(string) ([]byte, error) {
		return []byte("synthetic"), nil
	}, func(path string) (os.FileInfo, error) {
		if path == blocked {
			return nil, os.ErrPermission
		}
		return os.Stat(path)
	}, func(_ string, d os.DirEntry) (os.FileInfo, error) { return d.Info() })
	if err != nil || count == 0 || !strings.Contains(out.String(), blocked) || !strings.Contains(out.String(), later) {
		t.Fatalf("directory stat error stopped scan: count=%d out=%q err=%v", count, out.String(), err)
	}
}

func TestCheckEntryInfoErrorIsFindingAndContinues(t *testing.T) {
	root := setup(t)
	blocked := filepath.Join(root, "other", "metadata-error")
	later := filepath.Join(root, "z", "later")
	if err := os.MkdirAll(filepath.Dir(later), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, blocked, []byte("synthetic"), 0o600)
	writeTestFile(t, later, []byte("synthetic"), 0o600)
	var out bytes.Buffer
	count, err := scanRootWithFS(root, map[string]bool{}, &out, time.Now(), func(string) ([]byte, error) {
		return []byte("synthetic"), nil
	}, os.Stat, func(path string, d os.DirEntry) (os.FileInfo, error) {
		if path == blocked {
			return nil, os.ErrPermission
		}
		return d.Info()
	})
	if err != nil || count == 0 || !strings.Contains(out.String(), blocked) || !strings.Contains(out.String(), later) {
		t.Fatalf("entry info error stopped scan: count=%d out=%q err=%v", count, out.String(), err)
	}
}

func TestCheckReadErrorIsFindingAndScanContinues(t *testing.T) {
	root := setup(t)
	blocked := filepath.Join(root, "other", "read-error.txt")
	other := filepath.Join(root, "other", "later-orphan.txt")
	writeTestFile(t, blocked, []byte("synthetic"), 0o600)
	writeTestFile(t, other, []byte("synthetic"), 0o600)
	var out bytes.Buffer
	count, err := scanRootWithReader(root, map[string]bool{}, &out, time.Now(), func(path string) ([]byte, error) {
		if path == blocked {
			return nil, os.ErrPermission
		}
		return os.ReadFile(path)
	})
	if err != nil || count < 2 || !strings.Contains(out.String(), "read-error.txt") || !strings.Contains(out.String(), "later-orphan.txt") {
		t.Fatalf("read error stopped scan: count=%d out=%q err=%v", count, out.String(), err)
	}
}

func TestReadRegistryTrimsIndentedRowsAndDoesNotTreatEmptyCellsAsSeparator(t *testing.T) {
	root := setup(t)
	registry := "## Alpha\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\n| --- | --- | --- | --- | --- | --- |\n  | — | — | — | — | — | — |\n|  |  |  |  |  |  |\n"
	writeTestFile(t, filepath.Join(root, "LEDGER.md"), []byte(registry), 0o600)
	_, rows, err := readRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("wanted two data rows (including empty cells), got %#v", rows)
	}
	if isSeparatorRow("| --- | --- | --- | --- | --- | --- |") != true {
		t.Fatal("spaced separator not recognized")
	}
	code, out, errText := invokeTest(root, time.Now(), "find", "-")
	if code != 0 || errText != "" || out != "в реестре нет\n" {
		t.Fatalf("separator was searchable as a data row: code=%d out=%q err=%q", code, out, errText)
	}
}

func TestSubcommandHelpDoesNotReadRegistry(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"add", "--help"}, {"add", "-h"}, {"find", "-h"}, {"check", "--help"}} {
		args = append(args, "--root", filepath.Join(t.TempDir(), "missing-root"))
		var out, stderr bytes.Buffer
		if code := execute(args, &out, &stderr, time.Now()); code != 0 || !strings.Contains(out.String(), "Использование") || stderr.Len() != 0 {
			t.Errorf("help %q: code=%d out=%q err=%q", args, code, out.String(), stderr.String())
		}
	}
}

func TestCertificateSerialUsesOpenSSLHexWidthAndCRLFInsertion(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(0x0abc), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	check, _, _, _, err := verification(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if err != nil || !strings.Contains(check, "serial 0ABC") {
		t.Fatalf("serial format: %q err=%v", check, err)
	}
	content := []byte("## Alpha\r\n\r\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\r\n|---|---|---|---|---|---|\r\n")
	updated, err := insertRows(content, "Alpha", "| place | what | from | check | where | действует |\n")
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range updated {
		if b == '\n' && (i == 0 || updated[i-1] != '\r') {
			t.Fatalf("bare LF at offset %d: %q", i, updated)
		}
	}
}

func TestInsertRowsPreservesMixedLineEndingsOutsideInsertion(t *testing.T) {
	header := "| Место | Что | Откуда | Проверка | Где ещё | Статус |\n"
	separator := "| --- | --- | --- | --- | --- | --- |\n"
	registry := []byte("# Notes\n\nfree text line\r\n## Alpha\n\n" + header + separator + "| — | inventory | — | — | — | инвентаризация |\n")
	rowText := "| place | what | from | sha256 abcdef | where | действует |\n"
	updated, err := insertRows(registry, "Alpha", rowText)
	if err != nil {
		t.Fatal(err)
	}
	insertAt := strings.Index(string(registry), separator) + len(separator)
	want := make([]byte, 0, len(registry)+len(rowText))
	want = append(want, registry[:insertAt]...)
	want = append(want, rowText...)
	want = append(want, registry[insertAt:]...)
	if !bytes.Equal(updated, want) {
		t.Fatalf("mixed EOL bytes outside insertion changed:\n got %q\nwant %q", updated, want)
	}
	if bytes.Contains(updated, []byte("\r\r")) {
		t.Fatalf("inserted mixed EOL registry contains doubled CR: %q", updated)
	}
}

func TestInsertRowsAddsSeparatorEOLAtTerminalTableSeparator(t *testing.T) {
	content := []byte("## Alpha\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\n|---|---|---|---|---|---|")
	row := "| place | what | from | check | where | действует |\n"
	updated, err := insertRows(content, "Alpha", row)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte{}, content...), '\n'), []byte(row)...)
	if !bytes.Equal(updated, want) {
		t.Fatalf("terminal table separator was not separated by one EOL: got %q want %q", updated, want)
	}
}

func TestRegistryBOMFindAddAndCheckPreserveBytes(t *testing.T) {
	root := setup(t)
	regPath := filepath.Join(root, "LEDGER.md")
	original := []byte("\uFEFF## Alpha\n\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\n|---|---|---|---|---|---|\n| `~/secrets/other/present.txt` | item | source | sha256 abcdef123456 | — | действует |\n")
	writeTestFile(t, regPath, original, 0o600)
	code, out, errText := invokeTest(root, time.Now(), "find", "present")
	if code != 0 || errText != "" || !strings.Contains(out, "## Alpha\n") {
		t.Fatalf("find BOM registry: code=%d out=%q err=%q", code, out, errText)
	}
	path := filepath.Join(root, "other", "bom-add.txt")
	writeTestFile(t, path, []byte("synthetic"), 0o600)
	code, _, errText = invokeTest(root, time.Now(), "add", path, "--section", "Alpha", "--what", "added", "--from", "synthetic", "--where", "—")
	if code != 0 || errText != "" {
		t.Fatalf("add BOM registry: code=%d err=%q", code, errText)
	}
	updated := readTestFile(t, regPath)
	if !bytes.HasPrefix(updated, []byte("\uFEFF## Alpha")) || strings.Count(string(updated), "## Alpha") != 1 {
		t.Fatalf("BOM or section heading changed: %q", updated)
	}
	addedRow := []byte("| `~/secrets/other/bom-add.txt`")
	rowAt := bytes.Index(updated, addedRow)
	if rowAt < 0 {
		t.Fatalf("added row missing: %q", updated)
	}
	lineStart := bytes.LastIndexByte(updated[:rowAt], '\n') + 1
	lineEndRel := bytes.IndexByte(updated[rowAt:], '\n')
	if lineEndRel < 0 {
		t.Fatalf("added row has no line ending: %q", updated)
	}
	withoutAdded := append(append([]byte(nil), updated[:lineStart]...), updated[rowAt+lineEndRel+1:]...)
	if !bytes.Equal(withoutAdded, original) {
		t.Fatalf("registry bytes beyond inserted row changed: %q", updated)
	}
	code, out, errText = invokeTest(root, time.Now(), "check")
	if code != 0 || errText != "" || !strings.Contains(out, "в порядке") {
		t.Fatalf("check BOM registry: code=%d out=%q err=%q", code, out, errText)
	}
}

func TestCROnlyRegistryIsRejectedWithoutChanges(t *testing.T) {
	root := setup(t)
	regPath := filepath.Join(root, "LEDGER.md")
	crOnly := []byte("# Registry\n\n## Alpha\n\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\r|---|---|---|---|---|---|\r| `~/secrets/other/present.txt` | item | source | sha256 abcdef123456 | — | действует |\r")
	writeTestFile(t, regPath, crOnly, 0o600)
	for _, args := range [][]string{{"find", "item"}, {"check"}} {
		code, _, errText := invokeTest(root, time.Now(), args...)
		if code == 0 || !strings.Contains(errText, "LEDGER.md:5") || !strings.Contains(errText, "CR") {
			t.Errorf("CR-only %v was not rejected with reason: code=%d err=%q", args, code, errText)
		}
	}
	path := filepath.Join(root, "other", "cr-only-add.txt")
	writeTestFile(t, path, []byte("synthetic"), 0o600)
	code, _, errText := invokeTest(root, time.Now(), "add", path, "--section", "Alpha", "--what", "item", "--from", "synthetic", "--where", "—")
	if code == 0 || !strings.Contains(errText, "LEDGER.md:5") || !strings.Contains(errText, "CR") {
		t.Fatalf("add to CR-only registry was not rejected: code=%d err=%q", code, errText)
	}
	if !bytes.Equal(readTestFile(t, regPath), crOnly) {
		t.Fatal("rejecting CR-only registry changed it")
	}
}

func TestFirstBareCRLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents string
		want     int
	}{
		{"lone CR inside an LF registry", "## A\n\n| a | b\r c | d |\n", 3},
		{"CR at the end of a CRLF registry", "## A\r\n\r\nx\r", 3},
		{"CRLF only", "## A\r\n\r\nx\r\n", 0},
		{"LF only", "## A\n\nx\n", 0},
	} {
		if got := firstBareCRLine([]byte(tc.contents)); got != tc.want {
			t.Errorf("%s: firstBareCRLine = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCheckDanglingSymlinkInRegistryIsMissingFile(t *testing.T) {
	root := setup(t)
	link := filepath.Join(root, "other", "link")
	if err := os.Symlink("gone-target", link); err != nil {
		t.Fatal(err)
	}
	regPath := filepath.Join(root, "LEDGER.md")
	registry := strings.Replace(testHeader, "\n\n## Beta", "\n| `~/secrets/other/link` | link | synthetic | — | — | действует |\n\n## Beta", 1)
	writeTestFile(t, regPath, []byte(registry), 0o600)
	code, out, errText := invokeTest(root, time.Now(), "check")
	want := "строка без файла: `~/secrets/other/link`"
	if code != 1 || errText != "" || !strings.Contains(out, want) {
		t.Fatalf("dangling symlink row: code=%d out=%q err=%q", code, out, errText)
	}
}

func TestInsertRowsUsesRequiredEOLChoices(t *testing.T) {
	row := "| place | what | from | check | where | действует |\n"
	t.Run("new section CRLF majority", func(t *testing.T) {
		content := []byte("# A\n## Old\r\n| a |\r\n| b |\r\n| c |\r\n")
		got, err := insertRows(content, "New", row)
		if err != nil || !bytes.Contains(got, []byte("## New\r\n\r\n")) {
			t.Fatalf("new section EOL: got=%q err=%v", got, err)
		}
	})
	t.Run("separator EOL beats majority", func(t *testing.T) {
		content := []byte("# A\r\n## B\r\n| filler |\r\n| filler |\r\n| filler |\r\n| filler |\r\n| filler |\r\n| filler |\r\n## Alpha\n\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\n|---|---|---|---|---|---|\n")
		got, err := insertRows(content, "Alpha", row)
		if err != nil || !bytes.Contains(got, []byte("|---|---|---|---|---|---|\n| place | what | from | check | where | действует |\n")) {
			t.Fatalf("separator EOL did not win: got=%q err=%v", got, err)
		}
	})
	t.Run("LF majority with first CRLF", func(t *testing.T) {
		got := majorityEOL(splitRegistryLines("first\r\nsecond\nthird\nfourth\n"))
		if got != "\n" {
			t.Fatalf("majority EOL=%q, want LF", got)
		}
	})
	t.Run("tie uses first EOL", func(t *testing.T) {
		got := majorityEOL(splitRegistryLines("first\r\nsecond\n"))
		if got != "\r\n" {
			t.Fatalf("tie EOL=%q, want first CRLF", got)
		}
	})
	t.Run("previous line when separator is terminal", func(t *testing.T) {
		content := []byte("# A\n\n## Old\n| a |\n| b |\n## C\r\n\r\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\r\n|---|---|---|---|---|---|")
		got, err := insertRows(content, "C", row)
		if err != nil || !bytes.Contains(got, []byte("|---|---|---|---|---|---|\r\n| place | what | from | check | where | действует |\r\n")) {
			t.Fatalf("previous-line EOL was not used: got=%q err=%v", got, err)
		}
	})
	t.Run("CRLF blank suffix", func(t *testing.T) {
		content := []byte("## A\r\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\r\n|---|---|---|---|---|---|\r\n\r\n")
		got, err := insertRows(content, "B", row)
		if err != nil || bytes.Contains(got, []byte("\r\n\r\n\r\n## B")) || !bytes.Contains(got, []byte("\r\n\r\n## B")) {
			t.Fatalf("blank CRLF suffix not preserved cleanly: got=%q err=%v", got, err)
		}
	})
}

func TestAddDoesNotDuplicateSectionWithExtraHeadingSpace(t *testing.T) {
	root := setup(t)
	path := filepath.Join(root, "other", "spaced-heading.txt")
	writeTestFile(t, path, []byte("synthetic"), 0o600)
	content := []byte(strings.Replace(testHeader, "## Alpha", "##  Alpha", 1))
	writeTestFile(t, filepath.Join(root, "LEDGER.md"), content, 0o600)
	code, _, errText := invokeTest(root, time.Now(), "add", path, "--section", "Alpha", "--what", "space", "--from", "synthetic", "--where", "—")
	registry := string(readTestFile(t, filepath.Join(root, "LEDGER.md")))
	headings := 0
	for line := range strings.SplitSeq(registry, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "## "); ok && strings.TrimSpace(after) == "Alpha" {
			headings++
		}
	}
	if code != 0 || errText != "" || headings != 1 {
		t.Fatalf("section with extra heading space duplicated: code=%d headings=%d err=%q", code, headings, errText)
	}
}

func TestAddPublicKeyHashHasExactlyTwelveHexDigits(t *testing.T) {
	root := setup(t)
	_, keyPath, _ := makePEMPair(t, filepath.Join(root, "tls"), "hash-width", time.Now().Add(400*24*time.Hour))
	code, _, errText := invokeTest(root, time.Now(), "add", keyPath, "--section", "Hash", "--what", "key", "--from", "synthetic", "--where", "—")
	if code != 0 || errText != "" {
		t.Fatalf("add key: code=%d err=%q", code, errText)
	}
	registry := string(readTestFile(t, filepath.Join(root, "LEDGER.md")))
	for line := range strings.SplitSeq(registry, "\n") {
		if !strings.Contains(line, "hash-width.key") {
			continue
		}
		if !strings.Contains(line, "sha256 публичного ключа ") || !strings.Contains(line, " |") {
			t.Fatalf("public-key hash token missing: %q", line)
		}
		field := strings.SplitN(line, "sha256 публичного ключа ", 2)[1]
		token := strings.Fields(field)[0]
		if len(token) != 12 {
			t.Fatalf("public-key hash token has %d digits, want 12: %q", len(token), token)
		}
		for _, r := range token {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("non-hex character in public-key hash token %q", token)
			}
		}
		return
	}
	t.Fatal("key row missing from registry")
}

func TestAddECParametersPairAndAllowsUnpairedCACertificate(t *testing.T) {
	root := setup(t)
	certPath, keyPath, wantHash := makeECPEMPair(t, filepath.Join(root, "tls"), "ec")
	code, out, err := invokeTest(root, time.Now(), "add", certPath, keyPath, "--section", "EC", "--what", "ec tls", "--from", "synthetic", "--where", "—")
	if code != 0 || err != "" || !strings.Contains(out, wantHash) {
		t.Fatalf("EC PARAMETERS pair: code=%d out=%q err=%q", code, out, err)
	}
	caCert, _, _ := makePEMPair(t, filepath.Join(root, "tls"), "ca", time.Now().Add(400*24*time.Hour))
	clientCert, clientKey, _ := makePEMPair(t, filepath.Join(root, "tls"), "client", time.Now().Add(400*24*time.Hour))
	code, _, err = invokeTest(root, time.Now(), "add", caCert, clientKey, clientCert, "--section", "Chain", "--what", "CA plus client pair", "--from", "synthetic", "--where", "—")
	if code != 0 || err != "" {
		t.Fatalf("unpaired CA alongside matching client pair rejected: code=%d err=%q", code, err)
	}
}

func TestPKCS8PrivateKeyGetsPublicKeyHash(t *testing.T) {
	root := setup(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tls", "pkcs8.pem")
	writeTestFile(t, path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(publicDER)
	want := hex.EncodeToString(h[:])[:12]
	code, out, errText := invokeTest(root, time.Now(), "add", path, "--section", "PKCS8", "--what", "key", "--from", "synthetic", "--where", "—")
	if code != 0 || errText != "" || !strings.Contains(out, want) {
		t.Fatalf("PKCS#8 key hash: code=%d out=%q err=%q want=%s", code, out, errText, want)
	}
}

func makeECPEMPair(t *testing.T, dir, name string) (string, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(0x2a), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(400 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	params := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	writeTestFile(t, certPath, certPEM, 0o600)
	writeTestFile(t, keyPath, append(params, keyPEM...), 0o600)
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(publicDER)
	return certPath, keyPath, hex.EncodeToString(h[:])[:12]
}

func TestEncryptedKeysAreMarkedAndCannotBePaired(t *testing.T) {
	root := setup(t)
	enc8 := filepath.Join(root, "tls", "enc8.pem")
	enc8PEM := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte{1, 2, 3}})
	writeTestFile(t, enc8, enc8PEM, 0o600)
	code, out, err := invokeTest(root, time.Now(), "add", enc8, "--section", "Encrypted", "--what", "key", "--from", "synthetic", "--where", "—")
	fileHash := sha256Bytes(enc8PEM)[:12]
	registry := string(readTestFile(t, filepath.Join(root, "LEDGER.md")))
	rowStart := strings.Index(registry, "enc8.pem")
	if code != 0 || err != "" || !strings.Contains(out, "ключ зашифрован") || rowStart < 0 || !strings.Contains(registry[rowStart:], "sha256 "+fileHash) || !strings.Contains(registry[rowStart:], "ключ зашифрован") {
		t.Fatalf("encrypted PKCS#8: code=%d out=%q err=%q", code, out, err)
	}
	enc1 := filepath.Join(root, "tls", "enc1.pem")
	writeTestFile(t, enc1, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-256-CBC,0011223344556677"}, Bytes: []byte{1, 2, 3}}), 0o600)
	code, out, err = invokeTest(root, time.Now(), "add", enc1, "--section", "Encrypted", "--what", "legacy key", "--from", "synthetic", "--where", "—")
	if code != 0 || err != "" || !strings.Contains(out, "ключ зашифрован") {
		t.Fatalf("encrypted PKCS#1: code=%d out=%q err=%q", code, out, err)
	}
	ecPrivate, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	ecDER, e := x509.MarshalECPrivateKey(ecPrivate)
	if e != nil {
		t.Fatal(e)
	}
	ecEncrypted, e := x509.EncryptPEMBlock(rand.Reader, "EC PRIVATE KEY", ecDER, []byte("synthetic-passphrase"), x509.PEMCipherAES256)
	if e != nil {
		t.Fatal(e)
	}
	ecPath := filepath.Join(root, "tls", "enc-ec.pem")
	writeTestFile(t, ecPath, pem.EncodeToMemory(ecEncrypted), 0o600)
	code, out, err = invokeTest(root, time.Now(), "add", ecPath, "--section", "Encrypted", "--what", "legacy EC key", "--from", "synthetic", "--where", "—")
	if code != 0 || err != "" || !strings.Contains(out, "ключ зашифрован") {
		t.Fatalf("encrypted EC PEM: code=%d out=%q err=%q", code, out, err)
	}
	cert, _, _ := makePEMPair(t, filepath.Join(root, "tls"), "encrypted-pair", time.Now().Add(400*24*time.Hour))
	before := readTestFile(t, filepath.Join(root, "LEDGER.md"))
	enc8Pair := filepath.Join(root, "tls", "enc8-pair.pem")
	writeTestFile(t, enc8Pair, enc8PEM, 0o600)
	code, _, err = invokeTest(root, time.Now(), "add", cert, enc8Pair, "--section", "Encrypted", "--what", "unverifiable", "--from", "synthetic", "--where", "—")
	if code == 0 || !strings.Contains(err, "зашифрован") {
		t.Fatalf("encrypted key with cert accepted without cause: code=%d err=%q", code, err)
	}
	if !bytes.Equal(before, readTestFile(t, filepath.Join(root, "LEDGER.md"))) {
		t.Fatal("encrypted mismatched pair changed registry")
	}
}

func TestMalformedCertificatePEMIsRejected(t *testing.T) {
	root := setup(t)
	path := filepath.Join(root, "tls", "broken.pem")
	writeTestFile(t, path, []byte("-----BEGIN CERTIFICATE-----\nnot-base64-at-all\n-----END CERTIFICATE-----\n"), 0o600)
	code, _, err := invokeTest(root, time.Now(), "add", path, "--section", "Broken", "--what", "x", "--from", "x", "--where", "—")
	if code == 0 || !strings.Contains(err, "CERTIFICATE") {
		t.Fatalf("broken cert accepted: code=%d err=%q", code, err)
	}
}

func TestAddRejectsRegistryAndBadSectionAndTreatsHelpAsValue(t *testing.T) {
	root := setup(t)
	before := readTestFile(t, filepath.Join(root, "LEDGER.md"))
	code, _, err := invokeTest(root, time.Now(), "add", filepath.Join(root, "LEDGER.md"), "--section", "A", "--what", "x", "--from", "x", "--where", "—")
	if code == 0 || !strings.Contains(err, "LEDGER.md") {
		t.Fatalf("registry file add: code=%d err=%q", code, err)
	}
	path := filepath.Join(root, "other", "new.txt")
	writeTestFile(t, path, []byte("synthetic"), 0o600)
	code, _, err = invokeTest(root, time.Now(), "add", path, "--section", "Beta\n## Alpha", "--what", "x", "--from", "x", "--where", "—")
	if code == 0 || !strings.Contains(err, "перевод строки") {
		t.Fatalf("multiline section accepted: code=%d err=%q", code, err)
	}
	code, out, err := invokeTest(root, time.Now(), "add", path, "--section", "Beta", "--what", "-h", "--from", "x", "--where", "—")
	if code != 0 || strings.Contains(out, "Использование:") || !strings.Contains(string(readTestFile(t, filepath.Join(root, "LEDGER.md"))), "| -h |") {
		t.Fatalf("-h treated as help instead of value: code=%d out=%q err=%q", code, out, err)
	}
	if bytes.Equal(before, readTestFile(t, filepath.Join(root, "LEDGER.md"))) {
		t.Fatal("valid add did not update registry")
	}
	code, _, err = invokeTest(root, time.Now(), "add", path, "--section", " ", "--what", "empty", "--from", "x", "--where", "—")
	if code == 0 || !strings.Contains(err, "пустым") {
		t.Fatalf("blank section accepted: code=%d err=%q", code, err)
	}
}

func TestAddRelativePathUsesWorkingDirectoryAndRejectsDirectory(t *testing.T) {
	root := setup(t)
	path := filepath.Join(root, "other", "relative.txt")
	writeTestFile(t, path, []byte("synthetic"), 0o600)
	t.Chdir(filepath.Dir(path))
	code, out, errText := invokeTest(root, time.Now(), "add", "./relative.txt", "--section", "Relative", "--what", "cwd", "--from", "synthetic", "--where", "—")
	if code != 0 || errText != "" || !strings.Contains(out, "relative.txt") {
		t.Fatalf("cwd-relative add: code=%d out=%q err=%q", code, out, errText)
	}
	dir := filepath.Join(root, "other", "folder")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, errText = invokeTest(root, time.Now(), "add", dir, "--section", "Relative", "--what", "dir", "--from", "x", "--where", "—")
	if code == 0 || !strings.Contains(errText, "не обычный файл") {
		t.Fatalf("directory add accepted: code=%d err=%q", code, errText)
	}
}

func TestSectionTrimCRLFAndSpacedSeparator(t *testing.T) {
	root := setup(t)
	path := filepath.Join(root, "other", "crlf.txt")
	writeTestFile(t, path, []byte("x"), 0o600)
	registry := "  ## Alpha \r\n\r\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\r\n| --- | --- | --- | --- | --- | --- |\r\n| — | inventory | — | — | — | инвентаризация |\r\n"
	writeTestFile(t, filepath.Join(root, "LEDGER.md"), []byte(registry), 0o600)
	code, _, err := invokeTest(root, time.Now(), "add", path, "--section", "Alpha", "--what", "crlf", "--from", "synthetic", "--where", "—")
	if code != 0 || err != "" {
		t.Fatalf("CRLF/spaced separator add: code=%d err=%q", code, err)
	}
	got := string(readTestFile(t, filepath.Join(root, "LEDGER.md")))
	if strings.Count(got, "## Alpha") != 1 {
		t.Fatalf("created duplicate trimmed section: %s", got)
	}
	for i := 0; i < len(got); i++ {
		if got[i] == '\n' && (i == 0 || got[i-1] != '\r') {
			t.Fatalf("CRLF registry acquired a bare LF at %d: %q", i, got)
		}
	}
	code, out, err := invokeTest(root, time.Now(), "find", "inventory")
	if code != 0 || !strings.Contains(out, "инвентаризация") {
		t.Fatalf("spaced separator parsed as row: code=%d out=%q err=%q", code, out, err)
	}
}

func TestCheckSymlinkRootUnreadableFilesAndCertificateChains(t *testing.T) {
	root := setup(t)
	writeTestFile(t, filepath.Join(root, "other", "missing.txt"), []byte("synthetic"), 0o600)
	symlinkOrphan := filepath.Join(root, "other", "symlink-orphan.txt")
	writeTestFile(t, symlinkOrphan, []byte("synthetic orphan"), 0o600)
	link := filepath.Join(filepath.Dir(root), "root-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	code, out, err := invokeTest(link, time.Now(), "check")
	if code != 1 || !strings.Contains(out, "symlink-orphan.txt") || err != "" {
		t.Fatalf("check through root symlink: code=%d out=%q err=%q", code, out, err)
	}
	removeTest(t, symlinkOrphan)
	orphan := filepath.Join(root, "other", "orphan2.txt")
	locked := filepath.Join(root, "other", "locked.txt")
	writeTestFile(t, orphan, []byte("synthetic orphan"), 0o600)
	writeTestFile(t, locked, []byte("synthetic locked"), 0o000)
	chmodTest(t, locked, 0o000)
	code, out, err = invokeTest(root, time.Now(), "check")
	if code != 1 || err != "" || !strings.Contains(out, "orphan2.txt") || !strings.Contains(out, "locked.txt") {
		t.Fatalf("check stops or omits unreadable file: code=%d out=%q err=%q", code, out, err)
	}
	chmodTest(t, locked, 0o600)
	now := time.Now()
	far, _, _ := makePEMPair(t, filepath.Join(root, "tls"), "chain-far", now.Add(400*24*time.Hour))
	soon, _, _ := makePEMPair(t, filepath.Join(root, "tls"), "chain-soon", now.Add(5*24*time.Hour))
	chain := filepath.Join(root, "tls", "chain.pem")
	writeTestFile(t, chain, append(readTestFile(t, far), readTestFile(t, soon)...), 0o600)
	code, out, err = invokeTest(root, now, "check")
	if code != 1 || err != "" || !strings.Contains(out, "сертификат истекает в ближайшие 30 дней: "+chain) {
		t.Fatalf("second chain cert expiry omitted: code=%d out=%q err=%q", code, out, err)
	}
}

func TestFindIncludesSectionAndDefaultRootUsesSyntheticHome(t *testing.T) {
	root := setup(t)
	var out, stderr bytes.Buffer
	if code := execute([]string{"find", "webhook", "--root", root}, &out, &stderr, time.Now()); code != 0 || !strings.Contains(out.String(), "## Beta") {
		t.Fatalf("find omitted section header: code=%d out=%q err=%q", code, out.String(), stderr.String())
	}
	home := t.TempDir()
	defaultRoot := filepath.Join(home, "secrets")
	if err := os.MkdirAll(defaultRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(defaultRoot, "LEDGER.md"), []byte("## Synthetic\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\n|---|---|---|---|---|---|\n| — | token | — | — | — | инвентаризация |\n"), 0o600)
	t.Setenv("HOME", home)
	var helpOut, helpErr bytes.Buffer
	if code := execute([]string{"find", "token"}, &helpOut, &helpErr, time.Now()); code != 0 || !strings.Contains(helpOut.String(), "token") {
		t.Fatalf("default HOME root failed: code=%d out=%q err=%q", code, helpOut.String(), helpErr.String())
	}
}

func TestCheckFindsOwnerExecuteAndGroupDirectoryPermissionsAndIgnoresFarExpiry(t *testing.T) {
	root := setup(t)
	chmodTest(t, filepath.Join(root, "other", "present.txt"), 0o700)
	chmodTest(t, filepath.Join(root, "other"), 0o750)
	far, _, _ := makePEMPair(t, filepath.Join(root, "tls"), "far-only", time.Now().Add(400*24*time.Hour))
	code, out, err := invokeTest(root, time.Now(), "check")
	if code != 1 || err != "" || !strings.Contains(out, "права файла шире 600") || !strings.Contains(out, "права папки шире 700") {
		t.Fatalf("wide owner/group permissions missed: code=%d out=%q err=%q", code, out, err)
	}
	if strings.Contains(out, "сертификат истекает") {
		t.Fatalf("far certificate reported as expiring: %s", out)
	}
	if _, statErr := os.Stat(far); statErr != nil {
		t.Fatal(statErr)
	}
}

func TestDuplicateInputAndRepeatRootAreRejected(t *testing.T) {
	root := setup(t)
	path := filepath.Join(root, "other", "twice.txt")
	writeTestFile(t, path, []byte("x"), 0o600)
	args := []string{"add", path, path, "--section", "X", "--what", "x", "--from", "x", "--where", "—", "--root", root}
	var out, stderr bytes.Buffer
	if code := execute(args, &out, &stderr, time.Now()); code == 0 || !strings.Contains(stderr.String(), "повторён") {
		t.Fatalf("duplicate input accepted: code=%d stderr=%q", code, stderr.String())
	}
	args = []string{"check", "--root", root, "--root", root}
	out.Reset()
	stderr.Reset()
	if code := execute(args, &out, &stderr, time.Now()); code != 2 || !strings.Contains(stderr.String(), "больше одного раза") {
		t.Fatalf("duplicate root accepted: code=%d stderr=%q", code, stderr.String())
	}
}

func TestAddStaysInExistingNonFinalSection(t *testing.T) {
	root := setup(t)
	path := filepath.Join(root, "other", "alpha-new.txt")
	writeTestFile(t, path, []byte("synthetic"), 0o600)
	code, _, err := invokeTest(root, time.Now(), "add", path, "--section", "Alpha", "--what", "new alpha row", "--from", "synthetic", "--where", "—")
	if code != 0 || err != "" {
		t.Fatalf("add into first section: code=%d err=%q", code, err)
	}
	registry := string(readTestFile(t, filepath.Join(root, "LEDGER.md")))
	alpha := strings.Index(registry, "## Alpha")
	beta := strings.Index(registry, "## Beta")
	added := strings.Index(registry, "alpha-new.txt")
	if alpha < 0 || beta < 0 || added < alpha || added > beta {
		t.Fatalf("row inserted outside Alpha: %s", registry)
	}
	if strings.Count(registry, "## Beta") != 1 {
		t.Fatalf("later section changed: %s", registry)
	}
}

func TestNewSectionIsSeparatedAfterRegistryWithoutFinalNewline(t *testing.T) {
	root := setup(t)
	registryPath := filepath.Join(root, "LEDGER.md")
	writeTestFile(t, registryPath, []byte("# Registry\n\nTail"), 0o600)
	path := filepath.Join(root, "other", "new-section.txt")
	writeTestFile(t, path, []byte("synthetic"), 0o600)
	code, _, err := invokeTest(root, time.Now(), "add", path, "--section", "New", "--what", "text", "--from", "synthetic", "--where", "—")
	if code != 0 || err != "" {
		t.Fatalf("add new section: code=%d err=%q", code, err)
	}
	got := string(readTestFile(t, registryPath))
	if !strings.Contains(got, "Tail\n\n## New") {
		t.Fatalf("new heading lacks separating blank line: %q", got)
	}
}

func TestAddRefusesTargetSectionWithoutTableInsteadOfUsingNextSectionsTable(t *testing.T) {
	root := setup(t)
	registryPath := filepath.Join(root, "LEDGER.md")
	registry := "## Broken\nno table here\n\n## Valid\n| Место | Что | Откуда | Проверка | Где ещё | Статус |\n|---|---|---|---|---|---|\n| — | inventory | — | — | — | инвентаризация |\n"
	writeTestFile(t, registryPath, []byte(registry), 0o600)
	path := filepath.Join(root, "other", "broken-section.txt")
	writeTestFile(t, path, []byte("synthetic"), 0o600)
	code, _, err := invokeTest(root, time.Now(), "add", path, "--section", "Broken", "--what", "text", "--from", "synthetic", "--where", "—")
	if code == 0 || !strings.Contains(err, "нет таблицы") {
		t.Fatalf("used the following section's table: code=%d err=%q", code, err)
	}
	if got := string(readTestFile(t, registryPath)); got != registry {
		t.Fatalf("failed insertion changed registry: %q", got)
	}
}

func TestInvalidPrivateKeyAndDuplicateFieldsAreErrors(t *testing.T) {
	root := setup(t)
	badKey := filepath.Join(root, "tls", "broken.key")
	writeTestFile(t, badKey, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("synthetic invalid DER")}), 0o600)
	code, _, err := invokeTest(root, time.Now(), "add", badKey, "--section", "Bad", "--what", "key", "--from", "x", "--where", "—")
	if code == 0 || !strings.Contains(err, "разобрать") {
		t.Fatalf("broken private key accepted: code=%d err=%q", code, err)
	}
	path := filepath.Join(root, "other", "duplicate-field.txt")
	writeTestFile(t, path, []byte("synthetic"), 0o600)
	before := readTestFile(t, filepath.Join(root, "LEDGER.md"))
	code, _, err = invokeTest(root, time.Now(), "add", path, "--section", "X", "--what", "one", "--what", "two", "--from", "x", "--where", "—")
	if code == 0 || !strings.Contains(err, "один раз") {
		t.Fatalf("duplicate --what accepted: code=%d err=%q", code, err)
	}
	if !bytes.Equal(before, readTestFile(t, filepath.Join(root, "LEDGER.md"))) {
		t.Fatal("duplicate field changed registry")
	}
}

func TestCheckRejectsUnexpectedArgumentsAndRootEqualsWorks(t *testing.T) {
	root := setup(t)
	var out, stderr bytes.Buffer
	if code := execute([]string{"check", "extra", "--root", root}, &out, &stderr, time.Now()); code != 2 || !strings.Contains(stderr.String(), "не принимает аргументы") {
		t.Fatalf("check accepted an extra argument: code=%d stderr=%q", code, stderr.String())
	}
	out.Reset()
	stderr.Reset()
	if code := execute([]string{"check", "--root=" + root}, &out, &stderr, time.Now()); code != 1 || !strings.Contains(out.String(), "missing.txt") {
		t.Fatalf("--root= was not parsed: code=%d out=%q stderr=%q", code, out.String(), stderr.String())
	}
}

func TestCheckReportsFourFindingsAndCleanRegistry(t *testing.T) {
	root := setup(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	orphan := filepath.Join(root, "other", "orphan.txt")
	writeTestFile(t, orphan, []byte("orphan-private"), 0o644)
	chmodTest(t, filepath.Join(root, "other", "present.txt"), 0o640)
	cert, _, _ := makePEMPair(t, filepath.Join(root, "tls"), "soon", now.Add(10*24*time.Hour))
	code, out, err := invokeTest(root, now, "check")
	if code != 1 || err != "" {
		t.Fatalf("check findings: code=%d out=%q err=%q", code, out, err)
	}
	for _, want := range []string{"orphan.txt", "missing.txt", "present.txt", "soon.pem"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s: %s", want, out)
		}
	}
	if !strings.Contains(out, "сертификат истекает в ближайшие 30 дней: "+cert) {
		t.Errorf("soon certificate was not reported as expiring: %s", out)
	}
	for _, bad := range []string{"LEDGER.md", "safe synthetic value", "orphan-private", "a.pem"} {
		if strings.Contains(out, bad) {
			t.Errorf("unexpected sensitive/clean path %q in %s", bad, out)
		}
	}
	chmodTest(t, filepath.Join(root, "other", "present.txt"), 0o600)
	removeTest(t, orphan)
	writeTestFile(t, filepath.Join(root, "other", "missing.txt"), []byte("synthetic replacement"), 0o600)
	removeTest(t, cert)
	removeTest(t, strings.TrimSuffix(cert, ".pem")+".key")
	md := readTestFile(t, filepath.Join(root, "LEDGER.md"))
	lines := strings.Split(string(md), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if !strings.Contains(line, "soon.pem") {
			kept = append(kept, line)
		}
	}
	writeTestFile(t, filepath.Join(root, "LEDGER.md"), []byte(strings.Join(kept, "\n")), 0o600)
	code, out, err = invokeTest(root, now, "check")
	if code != 0 || !strings.Contains(out, "в порядке") || err != "" {
		t.Fatalf("clean check: code=%d out=%q err=%q", code, out, err)
	}
}

func TestCheckReportsRootAndNestedDirectoryPermissions(t *testing.T) {
	root := setup(t)
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, err := invokeTest(root, time.Now(), "check")
	if code != 1 || err != "" || !strings.Contains(out, root) || !strings.Contains(out, nested) {
		t.Fatalf("wide directory modes not reported: code=%d out=%q err=%q", code, out, err)
	}
}

func TestRootFlagAfterSubcommandAndHelp(t *testing.T) {
	root := setup(t)
	var out, err bytes.Buffer
	if code := execute([]string{"find", "alpha", "--root", root}, &out, &err, time.Now()); code != 0 || !strings.Contains(out.String(), "present.txt") {
		t.Fatalf("late root flag: %d %q %q", code, out.String(), err.String())
	}
	out.Reset()
	if code := execute([]string{"-h"}, &out, &err, time.Now()); code != 0 || !strings.Contains(out.String(), "find") || !strings.Contains(out.String(), "sha256") {
		t.Fatalf("help: %d %q", code, out.String())
	}
}
