package clients

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/textproto"
	"os/exec"
	"sort"
	"strings"

	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/mailer"
)

var _ mailer.Mailer = (*Sendmail)(nil)

// Sendmail implements [mailer.Mailer] interface and defines a mail
// client that sends emails via the "sendmail" *nix command.
//
// This client is usually recommended only for development and testing.
type Sendmail struct {
	onSend *hook.Hook[*mailer.SendEvent]
}

// OnSend implements [mailer.SendInterceptor] interface.
func (c *Sendmail) OnSend() *hook.Hook[*mailer.SendEvent] {
	if c.onSend == nil {
		c.onSend = &hook.Hook[*mailer.SendEvent]{}
	}
	return c.onSend
}

// Send implements [mailer.Mailer] interface.
func (c *Sendmail) Send(m *mailer.Message) error {
	if c.onSend != nil {
		return c.onSend.Trigger(&mailer.SendEvent{Message: m}, func(e *mailer.SendEvent) error {
			return c.send(e.Message)
		})
	}

	return c.send(m)
}

func (c *Sendmail) send(m *mailer.Message) error {
	toAddresses := mailer.AddressesToStrings(m.To, false)

	headers := make(textproto.MIMEHeader)
	headers.Set("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	headers.Set("From", m.From.String())
	headers.Set("Content-Type", "text/html; charset=UTF-8")
	headers.Set("To", strings.Join(toAddresses, ","))

	if len(m.Cc) > 0 {
		headers.Set("Cc", strings.Join(mailer.AddressesToStrings(m.Cc, false), ","))
	}

	if len(m.Bcc) > 0 {
		headers.Set("Bcc", strings.Join(mailer.AddressesToStrings(m.Bcc, false), ","))
	}

	cmdPath, err := findSendmailPath()
	if err != nil {
		return err
	}

	var buffer bytes.Buffer

	// write
	// ---
	if err := writeHeaders(&buffer, headers); err != nil {
		return err
	}
	if _, err := buffer.Write([]byte("\r\n")); err != nil {
		return err
	}
	if m.HTML != "" {
		if _, err := buffer.Write([]byte(m.HTML)); err != nil {
			return err
		}
	} else {
		if _, err := buffer.Write([]byte(m.Text)); err != nil {
			return err
		}
	}
	// ---

	sendmail := exec.Command(cmdPath, "-i", "-t")
	sendmail.Stdin = &buffer

	return sendmail.Run()
}

func findSendmailPath() (string, error) {
	options := []string{
		"/usr/sbin/sendmail",
		"/usr/bin/sendmail",
		"sendmail",
	}

	for _, option := range options {
		path, err := exec.LookPath(option)
		if err == nil {
			return path, err
		}
	}

	return "", errors.New("failed to locate a sendmail executable path")
}

var headerNewlineToSpace = strings.NewReplacer("\n", " ", "\r", " ")

// writeHeaders writes h in wire format (sorted by key), the same way
// as http.Header.Write does, without depending on net/http.
func writeHeaders(w io.Writer, h textproto.MIMEHeader) error {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if !validHeaderFieldName(k) {
			continue
		}
		for _, v := range h[k] {
			v = headerNewlineToSpace.Replace(v)
			v = strings.Trim(v, " \t\r\n")
			if _, err := io.WriteString(w, k+": "+v+"\r\n"); err != nil {
				return err
			}
		}
	}

	return nil
}

// validHeaderFieldName reports whether v is a valid RFC 7230 header field name (token).
func validHeaderFieldName(v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		isToken := c > 32 && c < 127 && !strings.ContainsRune("()<>@,;:\\\"/[]?={}", rune(c))
		if !isToken {
			return false
		}
	}
	return true
}
