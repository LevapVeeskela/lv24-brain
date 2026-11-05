package imap

import (
	"strings"
	"testing"
)

func TestExtractSnippetPlainText(t *testing.T) {
	header := []byte("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	body := []byte("Hello=20world,\r\nthis is a long message with many words that should be truncated.")
	got := ExtractSnippet(header, body, 20)
	if got != "Hello world, this is" {
		t.Fatalf("snippet = %q", got)
	}
}

func TestExtractSnippetHTML(t *testing.T) {
	header := []byte("Content-Type: text/html; charset=utf-8\r\n\r\n")
	body := []byte("<html><body><p>Hello <b>world</b></p><script>ignore()</script></body></html>")
	got := ExtractSnippet(header, body, 100)
	if strings.Contains(got, "<") || strings.Contains(got, "ignore") {
		t.Fatalf("snippet kept markup: %q", got)
	}
	if !strings.Contains(got, "Hello world") {
		t.Fatalf("snippet = %q", got)
	}
}

func TestExtractSnippetMultipartPrefersPlainText(t *testing.T) {
	header := []byte("Content-Type: multipart/alternative; boundary=abc\r\n\r\n")
	body := []byte("--abc\r\nContent-Type: text/html\r\n\r\n<p>HTML version</p>\r\n" +
		"--abc\r\nContent-Type: text/plain\r\n\r\nPlain version\r\n--abc--\r\n")
	got := ExtractSnippet(header, body, 100)
	if got != "Plain version" {
		t.Fatalf("snippet = %q", got)
	}
}

func TestExtractSnippetCollapsesControls(t *testing.T) {
	header := []byte("Content-Type: text/plain\r\n\r\n")
	body := []byte("a\x00b\n\tc")
	got := ExtractSnippet(header, body, 100)
	if got != "a b c" {
		t.Fatalf("snippet = %q", got)
	}
}
