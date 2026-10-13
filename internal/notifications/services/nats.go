package services

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	natsDefaultPort  = "4222"
	natsMaxLine      = 64 << 10
	natsMaxPayload   = 1 << 20
	natsClientName   = "patrol-notifications"
	natsProtoVersion = 1
)

// NatsOptions configure a nats service. Nkey must be a user seed ("SU..."): the client
// signs the server nonce with it.
type NatsOptions struct {
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	User     string            `json:"user"`
	Password string            `json:"password"`
	Nkey     string            `json:"nkey"`
	// InsecureSkipVerify disables TLS verification for tls:// servers.
	InsecureSkipVerify flexBool `json:"insecureSkipVerify"`
}

type natsService struct {
	opts   NatsOptions
	server *url.URL
	deps   Deps
	signer *nkeySigner
}

func newNatsService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts NatsOptions
	if err := decodeOptions(TypeNats, raw, &opts); err != nil {
		return nil, err
	}
	r.Add(opts.Password, opts.Nkey)
	u, err := url.Parse(strings.TrimSpace(opts.URL))
	if err != nil || opts.URL == "" {
		return nil, fmt.Errorf("%w: nats: url is required (nats://host:port)", ErrInvalidOptions)
	}
	switch strings.ToLower(u.Scheme) {
	case "nats", "tls":
	default:
		return nil, fmt.Errorf("%w: nats: url scheme must be nats or tls", ErrInvalidOptions)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("%w: nats: url has no host", ErrInvalidOptions)
	}
	if u.User != nil {
		if p, ok := u.User.Password(); ok {
			r.Add(p)
		} else {
			r.Add(u.User.Username())
		}
	}
	for k := range opts.Headers {
		if strings.ContainsAny(k, ":\r\n ") || k == "" {
			return nil, fmt.Errorf("%w: nats: invalid header name %q", ErrInvalidOptions, k)
		}
	}
	var signer *nkeySigner
	if opts.Nkey != "" {
		signer, err = parseNkeySeed(opts.Nkey)
		if err != nil {
			return nil, fmt.Errorf("%w: nats: nkey: %v", ErrInvalidOptions, err)
		}
	}
	return &natsService{opts: opts, server: u, deps: deps, signer: signer}, nil
}

type natsInfo struct {
	TLSRequired bool   `json:"tls_required"`
	Headers     bool   `json:"headers"`
	Nonce       string `json:"nonce"`
	MaxPayload  int64  `json:"max_payload"`
}

func (s *natsService) Send(ctx context.Context, n Notification, dest Destination) error {
	subject := strings.TrimSpace(dest.Recipient)
	if subject == "" || strings.ContainsAny(subject, " \t\r\n") {
		return fmt.Errorf("%w: nats: invalid subject %q", ErrInvalidNotification, dest.Recipient)
	}
	payload := []byte(n.Message)
	if len(payload) > natsMaxPayload {
		return fmt.Errorf("%w: nats: message exceeds %d bytes", ErrInvalidNotification, natsMaxPayload)
	}
	host := s.server.Hostname()
	port := s.server.Port()
	if port == "" {
		port = natsDefaultPort
	}
	addr := net.JoinHostPort(host, port)
	if err := validateEndpoint(&url.URL{Scheme: "nats", Host: addr}, []string{"nats"}, s.deps.Network.ValidateURL); err != nil {
		return err
	}
	conn, err := s.deps.dialer()(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("%w: nats: connecting to %s: %v", ErrDeliveryFailed, addr, err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline := time.Now().Add(s.deps.timeout())
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	rd := bufio.NewReaderSize(conn, 4096)
	line, err := natsReadLine(rd)
	if err != nil {
		return fmt.Errorf("%w: nats: reading INFO: %v", ErrDeliveryFailed, err)
	}
	op, arg, _ := strings.Cut(line, " ")
	if !strings.EqualFold(op, "INFO") {
		return fmt.Errorf("%w: nats: expected INFO, got %q", ErrDeliveryFailed, truncateRunes(line, 64))
	}
	var info natsInfo
	if err := json.Unmarshal([]byte(arg), &info); err != nil {
		return fmt.Errorf("%w: nats: invalid INFO: %v", ErrDeliveryFailed, err)
	}
	useTLS := strings.EqualFold(s.server.Scheme, "tls") || info.TLSRequired
	var rw io.ReadWriter = conn
	if useTLS {
		tc := tls.Client(conn, tlsConfig(s.deps, host, bool(s.opts.InsecureSkipVerify)))
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("%w: nats: TLS handshake: %v", ErrDeliveryFailed, err)
		}
		rw = tc
		rd = bufio.NewReaderSize(tc, 4096)
	}
	if len(s.opts.Headers) > 0 && !info.Headers {
		return fmt.Errorf("%w: nats: server does not support headers", ErrDeliveryFailed)
	}
	if info.MaxPayload > 0 && int64(len(payload)) > info.MaxPayload {
		return fmt.Errorf("%w: nats: message exceeds server max_payload %d", ErrInvalidNotification, info.MaxPayload)
	}

	connect := map[string]any{
		"verbose": false, "pedantic": false, "tls_required": useTLS, "name": natsClientName,
		"lang": "go", "version": "1.0.0", "protocol": natsProtoVersion, "headers": info.Headers,
	}
	user, pass := s.opts.User, s.opts.Password
	if s.server.User != nil && user == "" {
		if p, ok := s.server.User.Password(); ok {
			user, pass = s.server.User.Username(), p
		} else {
			connect["auth_token"] = s.server.User.Username()
		}
	}
	if user != "" {
		connect["user"] = user
		connect["pass"] = pass
	}
	if s.signer != nil {
		if info.Nonce == "" {
			return fmt.Errorf("%w: nats: server did not send a nonce for nkey authentication", ErrDeliveryFailed)
		}
		connect["nkey"] = s.signer.public
		connect["sig"] = base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.signer.key, []byte(info.Nonce)))
	}
	connectJSON, err := json.Marshal(connect)
	if err != nil {
		return fmt.Errorf("nats: encoding CONNECT: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteString("CONNECT ")
	buf.Write(connectJSON)
	buf.WriteString("\r\n")
	if len(s.opts.Headers) > 0 {
		var hdr bytes.Buffer
		hdr.WriteString("NATS/1.0\r\n")
		keys := make([]string, 0, len(s.opts.Headers))
		for k := range s.opts.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := strings.NewReplacer("\r", " ", "\n", " ").Replace(s.opts.Headers[k])
			fmt.Fprintf(&hdr, "%s: %s\r\n", k, v)
		}
		hdr.WriteString("\r\n")
		fmt.Fprintf(&buf, "HPUB %s %d %d\r\n", subject, hdr.Len(), hdr.Len()+len(payload))
		buf.Write(hdr.Bytes())
	} else {
		fmt.Fprintf(&buf, "PUB %s %d\r\n", subject, len(payload))
	}
	buf.Write(payload)
	buf.WriteString("\r\nPING\r\n")
	if _, err := rw.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("%w: nats: publishing: %v", ErrDeliveryFailed, err)
	}
	for {
		line, err := natsReadLine(rd)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("nats: %w", ctx.Err())
			}
			return fmt.Errorf("%w: nats: awaiting PONG: %v", ErrDeliveryFailed, err)
		}
		op, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(op) {
		case "PONG":
			return nil
		case "+OK", "INFO":
		case "PING":
			if _, err := rw.Write([]byte("PONG\r\n")); err != nil {
				return fmt.Errorf("%w: nats: %v", ErrDeliveryFailed, err)
			}
		case "-ERR":
			return fmt.Errorf("%w: nats: server error: %s", ErrDeliveryFailed, strings.Trim(arg, "' "))
		default:
			return fmt.Errorf("%w: nats: unexpected server message %q", ErrDeliveryFailed, truncateRunes(line, 64))
		}
	}
}

func natsReadLine(rd *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		chunk, isPrefix, err := rd.ReadLine()
		if err != nil {
			return "", err
		}
		sb.Write(chunk)
		if sb.Len() > natsMaxLine {
			return "", errors.New("protocol line too long")
		}
		if !isPrefix {
			return sb.String(), nil
		}
	}
}

// ---------------------------------------------------------------------------------------
// NATS nkeys (ed25519 user seeds), implemented from the nkeys encoding: base32 (no padding)
// of prefix bytes + payload + CRC16/XMODEM (little endian).

const (
	nkeyPrefixSeed = 18 << 3 // 'S'
	nkeyPrefixUser = 20 << 3 // 'U'
)

var nkeyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

type nkeySigner struct {
	key    ed25519.PrivateKey
	public string
}

func crc16XModem(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func parseNkeySeed(seed string) (*nkeySigner, error) {
	raw, err := nkeyEncoding.DecodeString(strings.TrimSpace(seed))
	if err != nil {
		return nil, errors.New("seed is not valid base32")
	}
	if len(raw) != 2+ed25519.SeedSize+2 {
		return nil, errors.New("seed has invalid length")
	}
	body, sum := raw[:len(raw)-2], binary.LittleEndian.Uint16(raw[len(raw)-2:])
	if crc16XModem(body) != sum {
		return nil, errors.New("seed checksum mismatch")
	}
	if body[0]&0xF8 != nkeyPrefixSeed {
		return nil, errors.New("value is not a seed")
	}
	kind := (body[0]&7)<<5 | (body[1]&0xF8)>>3
	if kind != nkeyPrefixUser {
		return nil, errors.New("seed is not a user seed")
	}
	key := ed25519.NewKeyFromSeed(body[2:])
	pub := key.Public().(ed25519.PublicKey)
	return &nkeySigner{key: key, public: encodeNkey(nkeyPrefixUser, pub)}, nil
}

func encodeNkey(prefix byte, payload []byte) string {
	raw := append([]byte{prefix}, payload...)
	var sum [2]byte
	binary.LittleEndian.PutUint16(sum[:], crc16XModem(raw))
	return nkeyEncoding.EncodeToString(append(raw, sum[:]...))
}

// encodeNkeySeed encodes an ed25519 seed as a user nkey seed (used by tests).
func encodeNkeySeed(seed []byte) string {
	b0 := byte(nkeyPrefixSeed) | byte(nkeyPrefixUser)>>5
	b1 := (byte(nkeyPrefixUser) & 31) << 3
	raw := append([]byte{b0, b1}, seed...)
	var sum [2]byte
	binary.LittleEndian.PutUint16(sum[:], crc16XModem(raw))
	return nkeyEncoding.EncodeToString(append(raw, sum[:]...))
}
