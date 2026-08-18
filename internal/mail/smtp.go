package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TLS modes, mirroring the configuration values. Named here as well so this
// package does not import internal/config.
const (
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"
	TLSNone     = "none"
)

// SMTPOptions is one relay, as this package needs it.
type SMTPOptions struct {
	// Host and Port are dialled; Host is also the TLS server name and the realm
	// PLAIN authenticates against, so it must be the name on the certificate
	// rather than an address.
	Host string
	Port int

	Username string
	Password string

	// From is the envelope sender and the From header. Either a bare address or
	// a display-name form; config validation has already parsed it.
	From string

	TLS     string
	Timeout time.Duration
}

// SMTPSender delivers over SMTP, with the smallest surface that can honestly
// claim to work.
//
// What is supported: STARTTLS on submission, implicit TLS, or an unencrypted
// connection to a relay that needs no credentials; PLAIN authentication, over
// an encrypted connection only. What is not: LOGIN, CRAM-MD5, XOAUTH2, client
// certificates, and any relay that requires them. That list is in
// docs/configuration.md too, because a mailer that implies universal
// compatibility and fails at the first send is worse than one that says what it
// does.
type SMTPSender struct {
	opts SMTPOptions
	from *mail.Address
}

func NewSMTPSender(opts SMTPOptions) (*SMTPSender, error) {
	from, err := mail.ParseAddress(opts.From)
	if err != nil {
		return nil, fmt.Errorf("mail: sender address %q: %w", opts.From, err)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	return &SMTPSender{opts: opts, from: from}, nil
}

// Addr is the relay this sender talks to, for log lines.
func (s *SMTPSender) Addr() string {
	return net.JoinHostPort(s.opts.Host, fmt.Sprint(s.opts.Port))
}

// Verify opens a connection, greets, and hangs up without sending anything.
//
// Called at boot so a mistyped host, a wrong port or a rejected password is
// reported once, at startup, by the process that could have told you — instead
// of surfacing weeks later as an invitation that never arrived. It is a
// warning and not a fatal error: the relay being down is not a reason for a
// link shortener to stop serving redirects, and anything queued in the meantime
// is retried from the outbox.
//
// **Called off the startup path**, in a goroutine of its own — never inline
// before the HTTP listener binds. This dials, so an unreachable relay costs the
// caller the whole of Timeout, and a caller that is the boot sequence spends
// that with nothing serving (F173, D166).
func (s *SMTPSender) Verify(ctx context.Context) error {
	c, cleanup, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	return c.Quit()
}

// Send delivers one message.
func (s *SMTPSender) Send(ctx context.Context, to, subject, body string) error {
	msg, err := BuildMessage(s.from, to, subject, body, time.Now())
	if err != nil {
		return err
	}

	c, cleanup, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	rcpt, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("mail: recipient %q: %w", to, err)
	}
	if err := c.Mail(s.from.Address); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(rcpt.Address); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	// DotWriter, which is what Data returns, dot-stuffs the payload: a body line
	// of "." cannot end the DATA phase early and let the rest be read as SMTP
	// commands.
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return fmt.Errorf("mail: write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: finish message: %w", err)
	}
	return c.Quit()
}

// connect dials, upgrades and authenticates, returning a client ready for a
// transaction and the function that closes it.
func (s *SMTPSender) connect(ctx context.Context) (*smtp.Client, func(), error) {
	deadline := time.Now().Add(s.opts.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	tlsCfg := &tls.Config{ServerName: s.opts.Host, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Deadline: deadline}

	var (
		conn net.Conn
		err  error
	)
	if s.opts.TLS == TLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", s.Addr())
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", s.Addr())
	}
	if err != nil {
		return nil, nil, fmt.Errorf("mail: connect to %s: %w", s.Addr(), err)
	}
	// One deadline for the whole conversation. net/smtp has no context, so this
	// is what stops a relay that accepts the connection and then says nothing
	// from holding the scheduler until the process restarts.
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("mail: set deadline: %w", err)
	}

	c, err := smtp.NewClient(conn, s.opts.Host)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("mail: greet %s: %w", s.Addr(), err)
	}
	cleanup := func() { _ = c.Close() }

	if s.opts.TLS == TLSStartTLS {
		// Refused rather than skipped when the relay does not offer it. Falling
		// back to an unencrypted session would send the password — and the
		// message — in clear, having been told to use TLS.
		if ok, _ := c.Extension("STARTTLS"); !ok {
			cleanup()
			return nil, nil, fmt.Errorf(
				"mail: %s does not offer STARTTLS; set LINKCTRL_SMTP_TLS to %q for a "+
					"relay that expects TLS from the first byte, or to %q for a relay "+
					"that needs no encryption", s.Addr(), TLSImplicit, TLSNone)
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("mail: STARTTLS with %s: %w", s.Addr(), err)
		}
	}

	if s.opts.Username != "" {
		// PlainAuth refuses to send credentials over an unencrypted connection
		// unless the server is localhost, which is a check worth keeping rather
		// than working around. Configuration refuses the same combination
		// earlier, so reaching that error here means the connection is not what
		// the configuration said it would be.
		auth := smtp.PlainAuth("", s.opts.Username, s.opts.Password, s.opts.Host)
		if err := c.Auth(auth); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("mail: authenticate to %s as %q: %w",
				s.Addr(), s.opts.Username, err)
		}
	}
	return c, cleanup, nil
}

// ErrHeaderInjection is returned when a value that becomes a header carries a
// line break.
var ErrHeaderInjection = errors.New("mail: header value contains a line break")

// BuildMessage assembles one RFC 5322 message.
//
// Exported so its properties can be asserted directly rather than inferred from
// what a fake relay happened to receive.
func BuildMessage(from *mail.Address, to, subject, body string, now time.Time) ([]byte, error) {
	rcpt, err := mail.ParseAddress(to)
	if err != nil {
		return nil, fmt.Errorf("mail: recipient %q: %w", to, err)
	}
	// The renderer already strips control characters from every value it
	// interpolates, so this can only fire on a value that reached a header
	// without going through it. That is exactly the case worth failing on
	// rather than trusting a second time.
	for _, v := range []string{subject, from.Address, from.Name, rcpt.Address, rcpt.Name} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, ErrHeaderInjection
		}
	}

	var b strings.Builder
	h := func(name, value string) {
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(value)
		b.WriteString("\r\n")
	}
	h("From", from.String())
	h("To", rcpt.String())
	// Encoded-word for anything outside ASCII. QEncoding returns the input
	// unchanged when it is already ASCII, so an English subject stays readable
	// in a raw message.
	h("Subject", mime.QEncoding.Encode("utf-8", subject))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", messageID(from.Address))
	h("MIME-Version", "1.0")
	h("Content-Type", `text/plain; charset="utf-8"`)
	h("Content-Transfer-Encoding", "8bit")
	// RFC 3834. Stops a recipient's vacation responder from replying to a
	// machine, and tells a mail system this was not typed by a person.
	h("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")

	// SMTP is a CRLF protocol. Bodies are written with bare newlines everywhere
	// else in this codebase, so the conversion happens once, here.
	body = strings.ReplaceAll(body, "\r\n", "\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String()), nil
}

// messageID builds a globally unique Message-ID in the sender's domain.
func messageID(fromAddr string) string {
	domain := "localhost"
	if i := strings.LastIndex(fromAddr, "@"); i >= 0 && i+1 < len(fromAddr) {
		domain = fromAddr[i+1:]
	}
	return "<" + uuid.Must(uuid.NewV7()).String() + "@" + domain + ">"
}
