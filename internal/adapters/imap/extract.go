package imap

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxSnippetDecodeBytes = 64 << 10

// ExtractSnippet renders a short plain-text snippet from message header and
// body bytes. It never returns the full body and never keeps HTML markup.
func ExtractSnippet(header, body []byte, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = 280
	}
	headers := parseHeader(header)
	contentType := headers.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = ""
	}

	text := ""
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		text = multipartText(body, params["boundary"])
	case mediaType == "text/html":
		text = stripHTML(decodeTransfer(body, headers.Get("Content-Transfer-Encoding")))
	case strings.HasPrefix(mediaType, "text/"):
		text = string(decodeTransfer(body, headers.Get("Content-Transfer-Encoding")))
	default:
		decoded := decodeTransfer(body, headers.Get("Content-Transfer-Encoding"))
		if looksHTML(decoded) {
			text = stripHTML(decoded)
		} else {
			text = string(decoded)
		}
	}
	return cleanSnippet(text, maxRunes)
}

func parseHeader(raw []byte) textproto.MIMEHeader {
	if len(raw) == 0 {
		return textproto.MIMEHeader{}
	}
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	headers, err := reader.ReadMIMEHeader()
	if err != nil {
		return textproto.MIMEHeader{}
	}
	return headers
}

func multipartText(body []byte, boundary string) string {
	if boundary == "" {
		return string(decodeTransfer(body, ""))
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	htmlFallback := ""
	for i := 0; i < 12; i++ {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		partType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if strings.HasPrefix(partType, "multipart/") {
			_ = part.Close()
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(part, maxSnippetDecodeBytes))
		_ = part.Close()
		decoded := decodeTransfer(raw, part.Header.Get("Content-Transfer-Encoding"))
		switch partType {
		case "text/plain":
			return string(decoded)
		case "text/html":
			if htmlFallback == "" {
				htmlFallback = stripHTML(decoded)
			}
		default:
			if partType == "" && htmlFallback == "" && looksHTML(decoded) {
				htmlFallback = stripHTML(decoded)
			}
		}
	}
	return htmlFallback
}

func decodeTransfer(raw []byte, encoding string) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		decoded, err := io.ReadAll(io.LimitReader(quotedprintable.NewReader(bytes.NewReader(raw)), maxSnippetDecodeBytes))
		if err == nil {
			return decoded
		}
	case "base64":
		decoded, err := io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(raw)), maxSnippetDecodeBytes))
		if err == nil {
			return decoded
		}
	}
	return raw
}

func stripHTML(raw []byte) string {
	value := string(raw)
	lower := strings.ToLower(value)
	var b strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '<' {
			b.WriteByte(value[i])
			i++
			continue
		}
		end := strings.IndexByte(value[i:], '>')
		if end < 0 {
			break
		}
		tag := strings.TrimSpace(lower[i+1 : i+end])
		name, _, _ := strings.Cut(tag, " ")
		name = strings.TrimSuffix(name, "/")
		if name == "script" || name == "style" {
			closeTag := "</" + name
			closeAt := strings.Index(lower[i+end:], closeTag)
			if closeAt < 0 {
				break
			}
			i += end + closeAt + len(closeTag)
			if gt := strings.IndexByte(value[i:], '>'); gt >= 0 {
				i += gt + 1
			} else {
				i = len(value)
			}
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(' ')
		i += end + 1
	}
	return html.UnescapeString(b.String())
}

func looksHTML(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	for _, marker := range []string{"<html", "<body", "<div", "<p>", "<br", "<a ", "</"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func cleanSnippet(value string, maxRunes int) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.Map(func(r rune) rune {
		switch {
		case r == '\n', r == '\t':
			return ' '
		case unicode.IsControl(r):
			return ' '
		default:
			return r
		}
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if maxRunes > 0 && utf8.RuneCountInString(value) > maxRunes {
		value = string([]rune(value)[:maxRunes])
	}
	return strings.TrimSpace(value)
}
