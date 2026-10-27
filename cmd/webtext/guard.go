package main

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

// internalSuffixes — имена, которых нет в публичном DNS: адрес такого хоста в архив не уходит.
var internalSuffixes = []string{".localhost", ".test", ".local", ".internal", ".lan", ".home.arpa", ".corp", ".intranet"}

// secretWords — части имени параметра query, по которым в нём токен, ключ или подпись.
var secretWords = map[string]bool{
	"token": true, "key": true, "apikey": true, "secret": true, "sig": true, "signature": true, "auth": true,
	"password": true, "passwd": true, "pwd": true, "session": true, "sessionid": true, "sid": true, "code": true,
	"credential": true, "credentials": true, "jwt": true, "ticket": true, "otp": true, "hash": true,
	"accesstoken": true, "access_token": true, "nonce": true,
}

var (
	nameWords = regexp.MustCompile(`[^a-z0-9]+`)
	// opaque — значение, похожее на токен: длинная строка из букв и цифр вперемешку.
	opaque = regexp.MustCompile(`^[A-Za-z0-9_\-.~+/=]{32,}$`)
	digit  = regexp.MustCompile(`[0-9]`)
	letter = regexp.MustCompile(`[A-Za-z]`)
)

// resolveTimeout — сколько ждать DNS, чтобы узнать, не внутренний ли хост.
const resolveTimeout = 2 * time.Second

// archivable — можно ли отдать адрес Wayback: не внутренний хост, без учётных данных и токенов. Причина — для отчёта.
func archivable(ctx context.Context, u *url.URL) (bool, string) {
	if u.User != nil {
		return false, "credentials in URL"
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || !strings.Contains(host, ".") && !strings.Contains(host, ":") {
		return false, "internal host"
	}
	for _, s := range internalSuffixes {
		if strings.HasSuffix(host, s) {
			return false, "internal host"
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if private(ip) {
			return false, "internal host"
		}
	} else if internal, err := privateDNS(ctx, host); err == nil && internal {
		return false, "host resolves to an internal address"
	}
	// Не разрешился — не внутренний: публичный сайт мог пропасть, а архив его помнит.
	query := u.Query()
	names := slices.Sorted(maps.Keys(query))
	for _, name := range names {
		vals := query[name]
		for _, w := range nameWords.Split(strings.ToLower(name), -1) {
			if secretWords[w] {
				return false, "token in query (" + name + ")"
			}
		}
		for _, v := range vals {
			if opaque.MatchString(v) && digit.MatchString(v) && letter.MatchString(v) {
				return false, "token-like value in query (" + name + ")"
			}
		}
	}
	return true, ""
}

// cgnat — 100.64.0.0/10, адреса провайдера и VPN вроде Tailscale.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func private(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || cgnat.Contains(ip)
}

// privateDNS — хост указывает на внутренний адрес.
func privateDNS(ctx context.Context, host string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", host, err)
	}
	if slices.ContainsFunc(addrs, private) {
		return true, nil
	}
	return false, nil
}

// pdfTimeout — потолок pdftotext: PDF на сотни страниц разбирается секунды.
const pdfTimeout = 30 * time.Second

// pdfText — текст PDF через pdftotext (poppler-utils): разбор PDF своими руками — тысячи строк чужого формата.
func pdfText(ctx context.Context, body []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pdftotext", "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(body)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if _, lookErr := exec.LookPath("pdftotext"); lookErr != nil {
			return "", fmt.Errorf("pdf: %w (sudo dnf install poppler-utils)", lookErr)
		}
		return "", fmt.Errorf("pdf: pdftotext: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return tidy(strings.ReplaceAll(out.String(), "\f", "\n")), nil
}
