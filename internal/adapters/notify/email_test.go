package notify_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/antoniojosev/trapline/internal/adapters/notify"
	"github.com/antoniojosev/trapline/internal/domain"
)

// smtpServer is the smallest thing net/smtp will talk to.
//
// A real listener rather than a mock, for the same reason the SQLite adapters
// are tested against a real database (ADR 009): what is being verified here is
// the conversation, and a mock of an SMTP server is a mock of the thing that
// could be wrong.
type smtpServer struct {
	listener net.Listener
	ehlo     string

	mu      sync.Mutex
	message strings.Builder
	from    string
	to      []string
}

func startSMTP(t *testing.T, ehlo string) *smtpServer {
	t.Helper()
	var sockets net.ListenConfig
	listener, err := sockets.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	server := &smtpServer{listener: listener, ehlo: ehlo}
	go server.accept()
	t.Cleanup(func() { _ = listener.Close() })
	return server
}

func (s *smtpServer) address() (host string, port int) {
	addr, _ := s.listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func (s *smtpServer) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

func (s *smtpServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	write := func(format string, args ...any) {
		_, _ = fmt.Fprintf(conn, format+"\r\n", args...)
	}

	write("220 test ESMTP")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "EHLO"):
			write("250-test")
			write("250 %s", s.ehlo)
		case strings.HasPrefix(command, "HELO"):
			write("250 test")
		case strings.HasPrefix(command, "MAIL FROM"):
			s.mu.Lock()
			s.from = strings.TrimSpace(line)
			s.mu.Unlock()
			write("250 ok")
		case strings.HasPrefix(command, "RCPT TO"):
			s.mu.Lock()
			s.to = append(s.to, strings.TrimSpace(line))
			s.mu.Unlock()
			write("250 ok")
		case command == "DATA":
			write("354 go ahead")
			for {
				body, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(body, "\r\n") == "." {
					break
				}
				s.mu.Lock()
				s.message.WriteString(body)
				s.mu.Unlock()
			}
			write("250 queued")
		case command == "QUIT":
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

func (s *smtpServer) received() (message, from string, to []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.message.String(), s.from, append([]string(nil), s.to...)
}

func TestEmailIsDelivered(t *testing.T) {
	server := startSMTP(t, "SIZE 10240")
	host, port := server.address()

	dispatcher := notify.New(notify.Options{})
	channel := domain.AlertChannel{Type: domain.ChannelEmail, Config: domain.ChannelConfig{
		Host: host, Port: port, From: "alerts@example.test",
		To: []string{"ops@example.test", "oncall@example.test"},
	}}
	if err := dispatcher.Send(context.Background(), &channel, payload(), "d"); err != nil {
		t.Fatalf("sending: %v", err)
	}

	message, from, to := server.received()
	if !strings.Contains(from, "alerts@example.test") {
		t.Errorf("MAIL FROM was %q", from)
	}
	if len(to) != 2 {
		t.Errorf("got %d recipients, want 2: %v", len(to), to)
	}
	for _, want := range []string{
		"From: alerts@example.test",
		"Content-Type: text/plain; charset=utf-8",
		"ValueError: boom",
		"https://errors.example.test/projects/2/issues/14",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not contain %q:\n%s", want, message)
		}
	}
	// A raw non-ASCII byte in a header is a message some relays reject and
	// others mangle, and an issue title is whatever an exception said.
	if !strings.Contains(message, "Subject: ") {
		t.Errorf("no subject:\n%s", message)
	}
}

func TestSubjectIsEncodedWhenItIsNotASCII(t *testing.T) {
	server := startSMTP(t, "SIZE 10240")
	host, port := server.address()

	unicode := payload()
	unicode.Title = "ValueError: cañón"

	dispatcher := notify.New(notify.Options{})
	channel := domain.AlertChannel{Type: domain.ChannelEmail, Config: domain.ChannelConfig{
		Host: host, Port: port, From: "a@b.test", To: []string{"c@d.test"},
	}}
	if err := dispatcher.Send(context.Background(), &channel, unicode, "d"); err != nil {
		t.Fatalf("sending: %v", err)
	}

	message, _, _ := server.received()
	headers, body, found := strings.Cut(message, "\r\n\r\n")
	if !found {
		t.Fatalf("the message has no header/body separator:\n%s", message)
	}
	if strings.ContainsAny(headers, "ñó") {
		t.Errorf("a header carries a raw non-ASCII byte:\n%s", headers)
	}
	if !strings.Contains(headers, "=?utf-8?") {
		t.Errorf("the subject was not encoded:\n%s", headers)
	}
	// The body is a different matter: it is declared utf-8, so the text
	// belongs there as written. Encoding it would be an unreadable message.
	if !strings.Contains(body, "cañón") {
		t.Errorf("the body lost the text:\n%s", body)
	}
}

// TestStartTLSIsRefusedWhenTheRelayDoesNotOfferIt: a channel that asked for
// TLS must fail loudly rather than send in the clear.
func TestStartTLSIsRefusedWhenTheRelayDoesNotOfferIt(t *testing.T) {
	server := startSMTP(t, "SIZE 10240")
	host, port := server.address()

	dispatcher := notify.New(notify.Options{})
	channel := domain.AlertChannel{Type: domain.ChannelEmail, Config: domain.ChannelConfig{
		Host: host, Port: port, From: "a@b.test", To: []string{"c@d.test"}, StartTLS: true,
	}}
	err := dispatcher.Send(context.Background(), &channel, payload(), "d")
	if err == nil {
		t.Fatal("a channel that requires STARTTLS sent in the clear")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

func TestAnUnreachableRelayIsAnError(t *testing.T) {
	dispatcher := notify.New(notify.Options{})
	channel := domain.AlertChannel{Type: domain.ChannelEmail, Config: domain.ChannelConfig{
		Host: "127.0.0.1", Port: 1, From: "a@b.test", To: []string{"c@d.test"},
	}}
	if err := dispatcher.Send(context.Background(), &channel, payload(), "d"); err == nil {
		t.Fatal("dialing a closed port succeeded")
	}
	_ = strconv.Itoa
}
