package imap

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
)

func renderFolder(account, folder string, mailbox MailboxState, cursor FolderCursor, messages []FetchedMessage, generatedAt time.Time) []byte {
	fields := map[string]string{
		"kind":           "connector_dump",
		"source":         Name,
		"project_id":     "mail:" + account,
		"schema_version": "1",
		"updated_at":     connectors.FormatTime(generatedAt),
		"account":        account,
		"folder":         folder,
		"uid_validity":   strconv.FormatUint(uint64(mailbox.UIDValidity), 10),
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s / %s\n\n", connectors.EscapeInline(account), connectors.EscapeInline(folder))
	fmt.Fprintf(&b, "- UID validity: %d\n", mailbox.UIDValidity)
	fmt.Fprintf(&b, "- Last UID: %d\n", cursor.LastUID)
	fmt.Fprintf(&b, "- Messages: %d\n", len(messages))

	if len(messages) == 0 {
		b.WriteString("\nNo new messages.\n")
		return connectors.Document(fields, b.String())
	}

	for _, message := range messages {
		subject := "(no subject)"
		var from, to []string
		messageID := ""
		date := message.InternalDate
		if message.Envelope != nil {
			if strings.TrimSpace(message.Envelope.Subject) != "" {
				subject = message.Envelope.Subject
			}
			from = formatAddresses(message.Envelope.From)
			to = formatAddresses(message.Envelope.To)
			messageID = message.Envelope.MessageID
			if !message.Envelope.Date.IsZero() {
				date = message.Envelope.Date
			}
		}
		fmt.Fprintf(&b, "\n## UID %d - %s\n\n", message.UID, connectors.EscapeInline(connectors.Truncate(subject, 300)))
		fmt.Fprintf(&b, "- From: %s\n", valueOr(strings.Join(from, ", "), "unknown"))
		if len(to) > 0 {
			fmt.Fprintf(&b, "- To: %s\n", strings.Join(to, ", "))
		}
		fmt.Fprintf(&b, "- Date: %s\n", connectors.DisplayTime(date, "unknown"))
		fmt.Fprintf(&b, "- Internal date: %s\n", connectors.DisplayTime(message.InternalDate, "unknown"))
		fmt.Fprintf(&b, "- Size: %d\n", message.Size)
		if messageID != "" {
			fmt.Fprintf(&b, "- Message-ID: %s\n", connectors.EscapeInline(messageID))
		}
		fmt.Fprintf(&b, "- Snippet: %s\n", valueOr(connectors.EscapeInline(message.Snippet), "none"))
	}

	return connectors.Document(fields, b.String())
}

func formatAddresses(addresses []imapv2.Address) []string {
	out := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if address.IsGroupStart() || address.IsGroupEnd() {
			continue
		}
		value := address.Addr()
		if value == "" {
			continue
		}
		name := strings.TrimSpace(address.Name)
		if name != "" {
			value = connectors.EscapeInline(name) + " <" + value + ">"
		}
		out = append(out, value)
	}
	return out
}

func valueOr(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
