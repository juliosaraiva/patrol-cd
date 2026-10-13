package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxEmailRecipients = 100
	maxEmailBodyBytes  = 4 << 20
)

// EmailOptions configure an email (SMTP) service. Port 465 uses implicit TLS; any other
// port upgrades with STARTTLS when the server offers it. RequireTLS (Patrol extension)
// refuses to deliver over an unencrypted connection.
type EmailOptions struct {
	Host               string     `json:"host"`
	Port               flexString `json:"port"`
	Username           string     `json:"username"`
	Password           string     `json:"password"`
	From               string     `json:"from"`
	HTML               flexBool   `json:"html"`
	InsecureSkipVerify flexBool   `json:"insecure_skip_verify"`
	ImplicitTLS        *flexBool  `json:"implicitTLS"`
	RequireTLS         flexBool   `json:"requireTLS"`
	HelloName          string     `json:"helloName"`
}

type emailService struct {
	opts        EmailOptions
	port        int
	from        *mail.Address
	implicitTLS bool
	deps        Deps
}

func newEmailService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts EmailOptions
	if err := decodeOptions(TypeEmail, raw, &opts); err != nil {
		return nil, err
	}
	r.Add(opts.Password)
	opts.Host = strings.TrimSpace(opts.Host)
	if opts.Host == "" || strings.ContainsAny(opts.Host, "/\r\n ") {
		return nil, fmt.Errorf("%w: email: host is required", ErrInvalidOptions)
	}
	port := int64(587)
	if strings.TrimSpace(string(opts.Port)) != "" {
		p, err := opts.Port.int64()
		if err != nil || p <= 0 || p > 65535 {
			return nil, fmt.Errorf("%w: email: invalid port", ErrInvalidOptions)
		}
		port = p
	}
	from, err := mail.ParseAddress(strings.TrimSpace(opts.From))
	if err != nil {
		return nil, fmt.Errorf("%w: email: invalid from address: %v", ErrInvalidOptions, err)
	}
	implicit := port == 465
	if opts.ImplicitTLS != nil {
		implicit = bool(*opts.ImplicitTLS)
	}
	return &emailService{opts: opts, port: int(port), from: from, implicitTLS: implicit, deps: deps}, nil
}

func (s *emailService) Send(ctx context.Context, n Notification, dest Destination) error {
	var to []string
	for _, part := range strings.Split(dest.Recipient, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		addr, err := mail.ParseAddress(part)
		if err != nil {
			return fmt.Errorf("%w: email: invalid recipient %q", ErrInvalidNotification, part)
		}
		to = append(to, addr.Address)
	}
	if len(to) == 0 {
		return fmt.Errorf("%w: email: no recipients", ErrInvalidNotification)
	}
	if len(to) > maxEmailRecipients {
		return fmt.Errorf("%w: email: more than %d recipients", ErrInvalidNotification, maxEmailRecipients)
	}
	subject, body := "", n.Message
	if n.Email != nil {
		subject = n.Email.Subject
		body = firstNonEmpty(n.Email.Body, body)
	}
	msg, err := s.buildMessage(to, subject, body)
	if err != nil {
		return err
	}
	return s.deliver(ctx, to, msg)
}

func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
}

func (s *emailService) buildMessage(to []string, subject, body string) ([]byte, error) {
	if len(body) > maxEmailBodyBytes {
		return nil, fmt.Errorf("%w: email: body exceeds %d bytes", ErrInvalidNotification, maxEmailBodyBytes)
	}
	var id [12]byte
	_, _ = rand.Read(id[:])
	domain := s.from.Address[strings.LastIndex(s.from.Address, "@")+1:]
	var b bytes.Buffer
	writeHeader := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	writeHeader("From", s.from.String())
	writeHeader("To", strings.Join(to, ", "))
	writeHeader("Subject", mime.QEncoding.Encode("utf-8", sanitizeHeader(subject)))
	writeHeader("Date", s.deps.now().Format(time.RFC1123Z))
	writeHeader("Message-ID", "<"+hex.EncodeToString(id[:])+"@"+sanitizeHeader(domain)+">")
	writeHeader("MIME-Version", "1.0")
	ctype := "text/plain"
	if s.opts.HTML {
		ctype = "text/html"
	}
	writeHeader("Content-Type", ctype+"; charset=UTF-8")
	writeHeader("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(body)); err != nil {
		return nil, fmt.Errorf("email: encoding body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("email: encoding body: %w", err)
	}
	return b.Bytes(), nil
}

func (s *emailService) tlsConfig() *tls.Config {
	return tlsConfig(s.deps, s.opts.Host, bool(s.opts.InsecureSkipVerify))
}

func (s *emailService) deliver(ctx context.Context, to []string, msg []byte) error {
	addr := net.JoinHostPort(s.opts.Host, strconv.Itoa(s.port))
	if err := validateEndpoint(&url.URL{Scheme: "smtp", Host: addr}, []string{"smtp"}, s.deps.Network.ValidateURL); err != nil {
		return err
	}
	conn, err := s.deps.dialer()(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("%w: email: connecting to %s: %v", ErrDeliveryFailed, addr, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline := time.Now().Add(2 * s.deps.timeout())
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	secure := false
	if s.implicitTLS {
		tc := tls.Client(conn, s.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("%w: email: TLS handshake: %v", ErrDeliveryFailed, err)
		}
		conn = tc
		secure = true
	}
	c, err := smtp.NewClient(conn, s.opts.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("%w: email: %v", ErrDeliveryFailed, err)
	}
	defer func() { _ = c.Close() }()
	hello := firstNonEmpty(s.opts.HelloName, "localhost")
	if err := c.Hello(hello); err != nil {
		return fmt.Errorf("%w: email: HELO: %v", ErrDeliveryFailed, err)
	}
	if !secure {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(s.tlsConfig()); err != nil {
				return fmt.Errorf("%w: email: STARTTLS: %v", ErrDeliveryFailed, err)
			}
			secure = true
		}
	}
	if bool(s.opts.RequireTLS) && !secure {
		return fmt.Errorf("%w: email: server does not support TLS and requireTLS is set", ErrDeliveryFailed)
	}
	if s.opts.Username != "" {
		ok, mechs := c.Extension("AUTH")
		if !ok {
			return fmt.Errorf("%w: email: server does not support authentication", ErrDeliveryFailed)
		}
		var auth smtp.Auth
		upper := strings.ToUpper(mechs)
		switch {
		case strings.Contains(upper, "PLAIN"):
			auth = smtp.PlainAuth("", s.opts.Username, s.opts.Password, s.opts.Host)
		case strings.Contains(upper, "LOGIN"):
			auth = &loginAuth{username: s.opts.Username, password: s.opts.Password, host: s.opts.Host}
		case strings.Contains(upper, "CRAM-MD5"):
			auth = smtp.CRAMMD5Auth(s.opts.Username, s.opts.Password)
		default:
			return fmt.Errorf("%w: email: no supported AUTH mechanism (server offers %q)", ErrDeliveryFailed, mechs)
		}
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("%w: email: authentication failed: %v", ErrDeliveryFailed, err)
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return fmt.Errorf("%w: email: MAIL FROM: %v", ErrDeliveryFailed, err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("%w: email: RCPT TO: %v", ErrDeliveryFailed, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("%w: email: DATA: %v", ErrDeliveryFailed, err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("%w: email: writing message: %v", ErrDeliveryFailed, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("%w: email: message rejected: %v", ErrDeliveryFailed, err)
	}
	if err := c.Quit(); err != nil && ctx.Err() == nil {
		// The message was accepted; a failed QUIT is not a delivery failure.
		s.deps.logger().Debug("email: QUIT failed", "error", err)
	}
	return ctx.Err()
}

// loginAuth implements the LOGIN SASL mechanism. Like smtp.PlainAuth it refuses to send
// credentials over an unencrypted connection to a non-local server.
type loginAuth struct {
	username, password, host string
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !isLocalSMTPHost(server.Name) {
		return "", nil, errors.New("unencrypted connection")
	}
	if server.Name != a.host {
		return "", nil, errors.New("wrong host name")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	prompt := strings.ToLower(strings.TrimSpace(string(fromServer)))
	switch {
	case strings.Contains(prompt, "username"):
		return []byte(a.username), nil
	case strings.Contains(prompt, "password"):
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("unexpected LOGIN challenge")
}

func isLocalSMTPHost(name string) bool {
	return name == "localhost" || name == "127.0.0.1" || name == "::1"
}
