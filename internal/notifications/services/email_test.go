package services

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
)

type smtpMessage struct {
	from     string
	to       []string
	data     string
	authUser string
	authPass string
	tls      bool
}

// fakeSMTP is a minimal SMTP server for tests: EHLO, STARTTLS, AUTH PLAIN/LOGIN, MAIL,
// RCPT, DATA, QUIT.
type fakeSMTP struct {
	ln          net.Listener
	tlsCfg      *tls.Config
	startTLS    bool
	implicitTLS bool
	authMechs   string
	password    string

	mu   sync.Mutex
	msgs []smtpMessage
}

func startSMTP(t *testing.T, s *fakeSMTP) (host, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	return h, p
}

func (s *fakeSMTP) messages() []smtpMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]smtpMessage(nil), s.msgs...)
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	isTLS := false
	if s.implicitTLS {
		tc := tls.Server(conn, s.tlsCfg)
		if err := tc.Handshake(); err != nil {
			return
		}
		conn, isTLS = tc, true
	}
	tp := textproto.NewConn(conn)
	var msg smtpMessage
	_ = tp.PrintfLine("220 localhost ESMTP fake")
	b64 := base64.StdEncoding
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "EHLO", "HELO":
			lines := []string{"localhost"}
			if s.startTLS && !isTLS {
				lines = append(lines, "STARTTLS")
			}
			if s.authMechs != "" {
				lines = append(lines, "AUTH "+s.authMechs)
			}
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				_ = tp.PrintfLine("250%s%s", sep, l)
			}
		case "STARTTLS":
			_ = tp.PrintfLine("220 ready")
			tc := tls.Server(conn, s.tlsCfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, isTLS = tc, true
			tp = textproto.NewConn(tc)
		case "AUTH":
			var user, pass string
			switch strings.ToUpper(fields[1]) {
			case "PLAIN":
				dec, _ := b64.DecodeString(fields[2])
				parts := strings.Split(string(dec), "\x00")
				if len(parts) == 3 {
					user, pass = parts[1], parts[2]
				}
			case "LOGIN":
				_ = tp.PrintfLine("334 %s", b64.EncodeToString([]byte("Username:")))
				l1, _ := tp.ReadLine()
				_ = tp.PrintfLine("334 %s", b64.EncodeToString([]byte("Password:")))
				l2, _ := tp.ReadLine()
				u, _ := b64.DecodeString(l1)
				p, _ := b64.DecodeString(l2)
				user, pass = string(u), string(p)
			}
			if pass != s.password {
				_ = tp.PrintfLine("535 5.7.8 authentication failed")
				continue
			}
			msg.authUser, msg.authPass = user, pass
			_ = tp.PrintfLine("235 2.7.0 authenticated")
		case "MAIL":
			msg.from = strings.Trim(strings.TrimPrefix(fields[1], "FROM:"), "<>")
			_ = tp.PrintfLine("250 OK")
		case "RCPT":
			msg.to = append(msg.to, strings.Trim(strings.TrimPrefix(fields[1], "TO:"), "<>"))
			_ = tp.PrintfLine("250 OK")
		case "DATA":
			_ = tp.PrintfLine("354 go ahead")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			msg.data = string(data)
			msg.tls = isTLS
			s.mu.Lock()
			s.msgs = append(s.msgs, msg)
			s.mu.Unlock()
			msg = smtpMessage{}
			_ = tp.PrintfLine("250 OK queued")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		case "RSET", "NOOP":
			_ = tp.PrintfLine("250 OK")
		default:
			_ = tp.PrintfLine("502 not implemented")
		}
	}
}

func parseMail(t *testing.T, data string) (*mail.Message, string) {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatalf("invalid message: %v\n%s", err, data)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(m.Body))
	if err != nil {
		t.Fatal(err)
	}
	// SMTP framing terminates the data with a line break.
	return m, strings.TrimRight(string(body), "\r\n")
}

func TestEmailPlainAuth(t *testing.T) {
	t.Parallel()
	srv := &fakeSMTP{authMechs: "PLAIN LOGIN", password: "smtp-secret"}
	host, port := startSMTP(t, srv)
	svc := mustNew(t, TypeEmail, map[string]any{"host": host, "port": port, "username": "bot", "password": "smtp-secret", "from": "Patrol <patrol@example.com>"}, testDeps())
	n := Notification{Message: "fallback body", Email: &EmailNotification{Subject: "Application guestbook synced\r\nBcc: evil@example.com", Body: "Synced at 12:00 ✓"}}
	mustSend(t, svc, n, Destination{Service: "email", Recipient: "a@example.com, b@example.com"})
	msgs := srv.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d", len(msgs))
	}
	m := msgs[0]
	if m.authUser != "bot" || m.authPass != "smtp-secret" || m.tls {
		t.Fatalf("auth %q/%q tls=%v", m.authUser, m.authPass, m.tls)
	}
	if m.from != "patrol@example.com" || strings.Join(m.to, ",") != "a@example.com,b@example.com" {
		t.Fatalf("envelope from=%q to=%v", m.from, m.to)
	}
	parsed, body := parseMail(t, m.data)
	dec := new(mime.WordDecoder)
	subject, err := dec.DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Application guestbook synced  Bcc: evil@example.com" {
		t.Fatalf("subject = %q", subject)
	}
	if parsed.Header.Get("Bcc") != "" {
		t.Fatalf("header injection: Bcc header present")
	}
	if !strings.HasPrefix(parsed.Header.Get("Content-Type"), "text/plain") || parsed.Header.Get("Message-Id") == "" {
		t.Fatalf("headers %v", parsed.Header)
	}
	if body != "Synced at 12:00 ✓" {
		t.Fatalf("body = %q", body)
	}
	if to := parsed.Header.Get("To"); to != "a@example.com, b@example.com" {
		t.Fatalf("To = %q", to)
	}
}

func TestEmailStartTLSLogin(t *testing.T) {
	t.Parallel()
	cert, _ := selfSignedCert(t)
	srv := &fakeSMTP{authMechs: "LOGIN", password: "pw", startTLS: true, tlsCfg: &tls.Config{Certificates: []tls.Certificate{cert}}}
	host, port := startSMTP(t, srv)
	svc := mustNew(t, TypeEmail, map[string]any{"host": host, "port": port, "username": "bot", "password": "pw", "from": "patrol@example.com", "insecure_skip_verify": true}, testDeps())
	mustSend(t, svc, Notification{Message: "hello"}, Destination{Recipient: "ops@example.com"})
	m := srv.messages()[0]
	if !m.tls || m.authUser != "bot" {
		t.Fatalf("tls=%v user=%q", m.tls, m.authUser)
	}
	_, body := parseMail(t, m.data)
	if body != "hello" {
		t.Fatalf("body %q", body)
	}
}

func TestEmailImplicitTLSWithCustomCA(t *testing.T) {
	t.Parallel()
	cert, pool := selfSignedCert(t)
	srv := &fakeSMTP{implicitTLS: true, tlsCfg: &tls.Config{Certificates: []tls.Certificate{cert}}}
	host, port := startSMTP(t, srv)
	deps := testDeps()
	deps.Network.RootCAs = func(name string) (*x509.CertPool, error) { return pool, nil }
	svc := mustNew(t, TypeEmail, map[string]any{"host": host, "port": port, "from": "patrol@example.com", "html": true, "implicitTLS": true}, deps)
	mustSend(t, svc, Notification{Message: "<b>deployed</b>", Email: &EmailNotification{Subject: "Déploiement réussi"}}, Destination{Recipient: "ops@example.com"})
	m := srv.messages()[0]
	parsed, body := parseMail(t, m.data)
	if !m.tls || !strings.HasPrefix(parsed.Header.Get("Content-Type"), "text/html") || body != "<b>deployed</b>" {
		t.Fatalf("tls=%v ct=%q body=%q", m.tls, parsed.Header.Get("Content-Type"), body)
	}
	if raw := parsed.Header.Get("Subject"); !strings.HasPrefix(raw, "=?utf-8?q?") {
		t.Fatalf("non-ASCII subject must be RFC 2047 encoded, got %q", raw)
	}
	// Without the CA the handshake fails.
	svc = mustNew(t, TypeEmail, map[string]any{"host": host, "port": port, "from": "patrol@example.com", "implicitTLS": true}, testDeps())
	if err := send(t, svc, Notification{Message: "x"}, Destination{Recipient: "ops@example.com"}); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("got %v", err)
	}
}

func TestEmailFailures(t *testing.T) {
	t.Parallel()
	srv := &fakeSMTP{authMechs: "PLAIN", password: "right"}
	host, port := startSMTP(t, srv)
	svc := mustNew(t, TypeEmail, map[string]any{"host": host, "port": port, "username": "bot", "password": "wrong-password", "from": "patrol@example.com"}, testDeps())
	err := send(t, svc, Notification{Message: "x"}, Destination{Recipient: "ops@example.com"})
	if !errors.Is(err, ErrDeliveryFailed) || strings.Contains(err.Error(), "wrong-password") {
		t.Fatalf("got %v", err)
	}
	if err := send(t, svc, Notification{Message: "x"}, Destination{Recipient: "not an address"}); !errors.Is(err, ErrInvalidNotification) {
		t.Fatalf("got %v", err)
	}
	strict := mustNew(t, TypeEmail, map[string]any{"host": host, "port": port, "from": "patrol@example.com", "requireTLS": true}, testDeps())
	if err := send(t, strict, Notification{Message: "x"}, Destination{Recipient: "ops@example.com"}); !errors.Is(err, ErrDeliveryFailed) || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("requireTLS: got %v", err)
	}
	if _, err := New(TypeEmail, mustJSON(t, map[string]any{"host": "smtp.example.com", "from": "not-an-address"}), testDeps()); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("invalid from: got %v", err)
	}
}
