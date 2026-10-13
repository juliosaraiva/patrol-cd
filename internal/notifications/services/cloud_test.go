package services

import (
	"bufio"
	"crypto/ed25519"
	"crypto/md5" //nolint:gosec // SQS protocol checksum
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------------------
// NATS

type natsCapture struct {
	connect map[string]any
	op      string
	subject string
	headers string
	payload string
}

// startNATS runs a one-connection-at-a-time fake NATS server. errAfterConnect, when set,
// is sent instead of accepting the publish.
func startNATS(t *testing.T, info map[string]any, errAfterConnect string) (string, <-chan natsCapture) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	out := make(chan natsCapture, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				infoJSON, _ := json.Marshal(info)
				_, _ = fmt.Fprintf(conn, "INFO %s\r\n", infoJSON)
				rd := bufio.NewReader(conn)
				var c natsCapture
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimRight(line, "\r\n")
					op, arg, _ := strings.Cut(line, " ")
					switch op {
					case "CONNECT":
						_ = json.Unmarshal([]byte(arg), &c.connect)
						if errAfterConnect != "" {
							_, _ = fmt.Fprintf(conn, "-ERR '%s'\r\n", errAfterConnect)
							return
						}
					case "PUB", "HPUB":
						parts := strings.Fields(arg)
						c.op, c.subject = op, parts[0]
						total, _ := strconv.Atoi(parts[len(parts)-1])
						buf := make([]byte, total+2)
						if _, err := io.ReadFull(rd, buf); err != nil {
							return
						}
						data := string(buf[:total])
						if op == "HPUB" {
							hdrLen, _ := strconv.Atoi(parts[1])
							c.headers, data = data[:hdrLen], data[hdrLen:]
						}
						c.payload = data
					case "PING":
						_, _ = conn.Write([]byte("PONG\r\n"))
						out <- c
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), out
}

func TestNatsPublish(t *testing.T) {
	t.Parallel()
	addr, got := startNATS(t, map[string]any{"server_id": "x", "headers": true, "max_payload": 1 << 20}, "")
	svc := mustNew(t, TypeNats, map[string]any{"url": "nats://" + addr, "user": "patrol", "password": "nats-pw", "headers": map[string]string{"my-header": "my-value"}}, testDeps())
	mustSend(t, svc, Notification{Message: "Deployment guestbook is ready!"}, Destination{Recipient: "deploy.events"})
	c := <-got
	if c.op != "HPUB" || c.subject != "deploy.events" || c.payload != "Deployment guestbook is ready!" {
		t.Fatalf("capture %+v", c)
	}
	if !strings.HasPrefix(c.headers, "NATS/1.0\r\n") || !strings.Contains(c.headers, "my-header: my-value\r\n") {
		t.Fatalf("headers %q", c.headers)
	}
	if c.connect["user"] != "patrol" || c.connect["pass"] != "nats-pw" || c.connect["headers"] != true {
		t.Fatalf("connect %v", c.connect)
	}
}

func TestNatsNkeyAndErrors(t *testing.T) {
	t.Parallel()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	addr, got := startNATS(t, map[string]any{"server_id": "x", "nonce": "server-nonce"}, "")
	svc := mustNew(t, TypeNats, map[string]any{"url": "nats://" + addr, "nkey": encodeNkeySeed(seed)}, testDeps())
	mustSend(t, svc, Notification{Message: "m"}, Destination{Recipient: "subj"})
	c := <-got
	if c.op != "PUB" {
		t.Fatalf("op %q", c.op)
	}
	if c.connect["nkey"] != encodeNkey(nkeyPrefixUser, pub) || !strings.HasPrefix(c.connect["nkey"].(string), "U") {
		t.Fatalf("nkey %v", c.connect["nkey"])
	}
	sig, err := base64.RawURLEncoding.DecodeString(c.connect["sig"].(string))
	if err != nil || !ed25519.Verify(pub, []byte("server-nonce"), sig) {
		t.Fatalf("invalid nonce signature")
	}

	errAddr, _ := startNATS(t, map[string]any{"server_id": "x"}, "Authorization Violation")
	svc = mustNew(t, TypeNats, map[string]any{"url": "nats://" + errAddr}, testDeps())
	err = send(t, svc, Notification{Message: "m"}, Destination{Recipient: "subj"})
	if !errors.Is(err, ErrDeliveryFailed) || !strings.Contains(err.Error(), "Authorization Violation") {
		t.Fatalf("got %v", err)
	}
	if err := send(t, svc, Notification{Message: "m"}, Destination{Recipient: "bad subject"}); !errors.Is(err, ErrInvalidNotification) {
		t.Fatalf("got %v", err)
	}
	if _, err := New(TypeNats, mustJSON(t, map[string]any{"url": "nats://x", "nkey": "SUNOTAVALIDSEED"}), testDeps()); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("invalid seed: got %v", err)
	}
}

func TestNkeySeedRoundTrip(t *testing.T) {
	t.Parallel()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	enc := encodeNkeySeed(seed)
	if !strings.HasPrefix(enc, "SU") {
		t.Fatalf("user seeds start with SU, got %q", enc[:2])
	}
	s, err := parseNkeySeed(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.public, "U") || len(s.public) != 56 {
		t.Fatalf("public key %q", s.public)
	}
	// Corrupt one character: checksum must fail.
	b := []byte(enc)
	if b[10] == 'A' {
		b[10] = 'B'
	} else {
		b[10] = 'A'
	}
	if _, err := parseNkeySeed(string(b)); err == nil {
		t.Fatal("corrupted seed accepted")
	}
}

// ---------------------------------------------------------------------------------------
// AWS SQS

func TestAwsSqs(t *testing.T) {
	t.Parallel()
	var srvURL string
	srv, rec := newServer(t, func(w http.ResponseWriter, c captured) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		switch c.Header.Get("X-Amz-Target") {
		case "AmazonSQS.GetQueueUrl":
			var in map[string]any
			_ = json.Unmarshal(c.Body, &in)
			_ = json.NewEncoder(w).Encode(map[string]any{"QueueUrl": srvURL + "/" + fmt.Sprint(in["QueueOwnerAWSAccountId"]) + "/" + fmt.Sprint(in["QueueName"])})
		case "AmazonSQS.SendMessage":
			var in map[string]any
			_ = json.Unmarshal(c.Body, &in)
			sum := md5.Sum([]byte(in["MessageBody"].(string))) //nolint:gosec // protocol checksum
			_ = json.NewEncoder(w).Encode(map[string]any{"MessageId": "m-1", "MD5OfMessageBody": hex.EncodeToString(sum[:])})
		default:
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"__type":"InvalidAction"}`))
		}
	})
	srvURL = srv.URL
	svc := mustNew(t, TypeAwsSqs, map[string]any{"region": "us-east-2", "queue": "myqueue", "account": "1234567", "key": "AKIDEXAMPLE", "secret": "aws-secret-key", "endpointUrl": srv.URL}, testDeps())
	n := Notification{Message: "Deployment guestbook is ready!", AwsSqs: &AwsSqsNotification{MessageAttributes: map[string]string{"app": "guestbook"}, MessageGroupID: "guestbook-deployment"}}
	mustSend(t, svc, n, Destination{Service: "awssqs", Recipient: "overwrite-myqueue"})
	reqs := rec.all()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	get := reqs[0].json(t)
	if get["QueueName"] != "overwrite-myqueue" || get["QueueOwnerAWSAccountId"] != "1234567" {
		t.Fatalf("GetQueueUrl %v", get)
	}
	if auth := reqs[1].Header.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || !strings.Contains(auth, "/us-east-2/sqs/") {
		t.Fatalf("Authorization %q", auth)
	}
	sendBody := reqs[1].json(t)
	if sendBody["QueueUrl"] != srv.URL+"/1234567/overwrite-myqueue" || sendBody["MessageBody"] != "Deployment guestbook is ready!" || sendBody["MessageGroupId"] != "guestbook-deployment" {
		t.Fatalf("SendMessage %v", sendBody)
	}
	attr := sendBody["MessageAttributes"].(map[string]any)["app"].(map[string]any)
	if attr["DataType"] != "String" || attr["StringValue"] != "guestbook" {
		t.Fatalf("attributes %v", sendBody["MessageAttributes"])
	}
	if strings.Contains(string(reqs[1].Body), "aws-secret-key") {
		t.Fatalf("secret key sent in body")
	}
}

// ---------------------------------------------------------------------------------------
// GCP Pub/Sub

func TestGCPPubSub(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	srv, rec := newServer(t, func(w http.ResponseWriter, c captured) {
		switch {
		case c.Path == "/token":
			form, _ := url.ParseQuery(string(c.Body))
			if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || form.Get("assertion") == "" {
				writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
			writeJSON(w, 200, map[string]any{"access_token": "ya29.test-token", "token_type": "Bearer", "expires_in": 3600})
		case strings.HasSuffix(c.Path, ":publish"):
			writeJSON(w, 200, map[string]any{"messageIds": []string{"1"}})
		default:
			writeJSON(w, 404, map[string]any{})
		}
	})
	sa, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "my-project", "private_key_id": "kid", "private_key": keyPEM,
		"client_email": "patrol@my-project.iam.gserviceaccount.com", "client_id": "1", "token_uri": srv.URL + "/token",
	})
	svc := mustNew(t, TypeGCPPubSub, map[string]any{"project": "my-project", "topic": "my-topic", "keyJSON": string(sa), "endpoint": srv.URL}, testDeps())
	n := Notification{Message: "Deployment ready", GCPPubSub: &GCPPubSubNotification{Attributes: map[string]string{"app": "guestbook"}}}
	mustSend(t, svc, n, Destination{Recipient: "override-topic"})
	mustSend(t, svc, n, Destination{})
	var publishes []captured
	tokens := 0
	for _, r := range rec.all() {
		if r.Path == "/token" {
			tokens++
		} else {
			publishes = append(publishes, r)
		}
	}
	if tokens != 1 {
		t.Fatalf("token requests = %d, want 1 (cached)", tokens)
	}
	if publishes[0].Path != "/v1/projects/my-project/topics/override-topic:publish" || publishes[1].Path != "/v1/projects/my-project/topics/my-topic:publish" {
		t.Fatalf("paths %q %q", publishes[0].Path, publishes[1].Path)
	}
	if publishes[0].Header.Get("Authorization") != "Bearer ya29.test-token" {
		t.Fatalf("Authorization %q", publishes[0].Header.Get("Authorization"))
	}
	msg := publishes[0].json(t)["messages"].([]any)[0].(map[string]any)
	data, _ := base64.StdEncoding.DecodeString(msg["data"].(string))
	if string(data) != "Deployment ready" || msg["attributes"].(map[string]any)["app"] != "guestbook" {
		t.Fatalf("message %v", msg)
	}
}

func TestGCPPubSubRejectsExternalAccountKeys(t *testing.T) {
	t.Parallel()
	ext, _ := json.Marshal(map[string]any{"type": "external_account", "audience": "x", "token_url": "http://169.254.169.254/token", "credential_source": map[string]any{"url": "http://169.254.169.254/"}})
	svc := mustNew(t, TypeGCPPubSub, map[string]any{"project": "p", "topic": "t", "keyJSON": string(ext)}, testDeps())
	if err := send(t, svc, Notification{Message: "m"}, Destination{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("external account credentials must be rejected, got %v", err)
	}
}
