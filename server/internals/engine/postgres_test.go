package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// rw pairs a reader over client bytes with a buffer capturing proxy replies.
type rw struct {
	in  *bytes.Reader
	out bytes.Buffer
}

func (r *rw) Read(p []byte) (int, error)  { return r.in.Read(p) }
func (r *rw) Write(p []byte) (int, error) { return r.out.Write(p) }

// startupPacket builds a protocol 3.0 StartupMessage from key/value pairs.
func startupPacket(params ...string) []byte {
	var body bytes.Buffer
	body.Write(binary.BigEndian.AppendUint32(nil, protocolVersion3))
	for _, p := range params {
		body.WriteString(p)
		body.WriteByte(0)
	}
	body.WriteByte(0) // terminator

	out := binary.BigEndian.AppendUint32(nil, uint32(body.Len()+4))
	return append(out, body.Bytes()...)
}

// codePacket builds an 8-byte request carrying a magic code (SSL, GSS, cancel).
func codePacket(code uint32) []byte {
	out := binary.BigEndian.AppendUint32(nil, 8)
	return binary.BigEndian.AppendUint32(out, code)
}

func TestResolveExtractsDatabase(t *testing.T) {
	packet := startupPacket("user", "haki", "database", "mydb")
	conn := &rw{in: bytes.NewReader(packet)}

	pg := &Postgres{}
	db, replay, err := pg.Resolve(conn)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if db != "mydb" {
		t.Errorf("database = %q, want %q", db, "mydb")
	}
	if !bytes.Equal(replay, packet) {
		t.Errorf("replay must reproduce the startup packet byte for byte\ngot  %v\nwant %v", replay, packet)
	}
	if conn.out.Len() != 0 {
		t.Errorf("nothing should be written to a plain startup, got %v", conn.out.Bytes())
	}
}

func TestResolveDefaultsDatabaseToUser(t *testing.T) {
	// Postgres treats a missing database as "same name as the user".
	packet := startupPacket("user", "haki")
	conn := &rw{in: bytes.NewReader(packet)}

	db, _, err := (&Postgres{}).Resolve(conn)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if db != "haki" {
		t.Errorf("database = %q, want %q (defaulted from user)", db, "haki")
	}
}

func TestResolveDeclinesSSLThenParsesStartup(t *testing.T) {
	startup := startupPacket("user", "haki", "database", "mydb")
	stream := append(codePacket(sslRequestCode), startup...)
	conn := &rw{in: bytes.NewReader(stream)}

	db, replay, err := (&Postgres{}).Resolve(conn)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if db != "mydb" {
		t.Errorf("database = %q, want %q", db, "mydb")
	}
	if got := conn.out.Bytes(); !bytes.Equal(got, []byte{'N'}) {
		t.Errorf("SSLRequest must be declined with 'N', got %v", got)
	}
	// The declined negotiation must not be replayed: the backend socket is a
	// fresh plaintext connection.
	if !bytes.Equal(replay, startup) {
		t.Errorf("replay must contain only the StartupMessage, got %v", replay)
	}
}

func TestResolveDeclinesGSSEnc(t *testing.T) {
	startup := startupPacket("user", "haki", "database", "mydb")
	stream := append(codePacket(gssEncRequestCode), startup...)
	conn := &rw{in: bytes.NewReader(stream)}

	db, _, err := (&Postgres{}).Resolve(conn)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if db != "mydb" {
		t.Errorf("database = %q, want %q", db, "mydb")
	}
	if got := conn.out.Bytes(); !bytes.Equal(got, []byte{'N'}) {
		t.Errorf("GSSENCRequest must be declined with 'N', got %v", got)
	}
}

func TestResolveHandlesBothNegotiationsInSequence(t *testing.T) {
	// psql can try GSS then SSL before falling back to plaintext.
	startup := startupPacket("user", "haki", "database", "mydb")
	stream := append(codePacket(gssEncRequestCode), codePacket(sslRequestCode)...)
	stream = append(stream, startup...)
	conn := &rw{in: bytes.NewReader(stream)}

	db, _, err := (&Postgres{}).Resolve(conn)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if db != "mydb" {
		t.Errorf("database = %q, want %q", db, "mydb")
	}
	if got := conn.out.Bytes(); !bytes.Equal(got, []byte{'N', 'N'}) {
		t.Errorf("both negotiations must be declined, got %v", got)
	}
}

func TestResolveRejectsCancelRequest(t *testing.T) {
	// A CancelRequest carries a PID and secret, not a database name.
	packet := binary.BigEndian.AppendUint32(nil, 16)
	packet = binary.BigEndian.AppendUint32(packet, cancelRequestCode)
	packet = binary.BigEndian.AppendUint32(packet, 1234)
	packet = binary.BigEndian.AppendUint32(packet, 5678)

	_, _, err := (&Postgres{}).Resolve(&rw{in: bytes.NewReader(packet)})
	if !errors.Is(err, ErrCancelRequest) {
		t.Errorf("err = %v, want ErrCancelRequest", err)
	}
}

func TestResolveRejectsMissingDatabase(t *testing.T) {
	packet := startupPacket("application_name", "psql")
	_, _, err := (&Postgres{}).Resolve(&rw{in: bytes.NewReader(packet)})
	if !errors.Is(err, ErrNoDatabase) {
		t.Errorf("err = %v, want ErrNoDatabase", err)
	}
}

func TestResolveRejectsBadPackets(t *testing.T) {
	tests := []struct {
		name   string
		packet []byte
		want   string
	}{
		{
			name:   "length below minimum",
			packet: binary.BigEndian.AppendUint32(nil, 4),
			want:   "invalid startup packet length",
		},
		{
			name:   "length above cap",
			packet: binary.BigEndian.AppendUint32(nil, maxStartupLen+1),
			want:   "invalid startup packet length",
		},
		{
			name: "unsupported protocol version",
			packet: func() []byte {
				out := binary.BigEndian.AppendUint32(nil, 8)
				return binary.BigEndian.AppendUint32(out, 131072) // protocol 2.0
			}(),
			want: "unsupported postgres protocol version",
		},
		{
			name:   "truncated body",
			packet: binary.BigEndian.AppendUint32(nil, 100),
			want:   "read startup body",
		},
		{
			name:   "empty stream",
			packet: nil,
			want:   "read startup length",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := (&Postgres{}).Resolve(&rw{in: bytes.NewReader(tt.packet)})
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestResolveIgnoresTrailingBytesAfterTerminator(t *testing.T) {
	// Extra bytes beyond the declared length belong to the next message and
	// must not be consumed by the startup parse.
	startup := startupPacket("user", "haki", "database", "mydb")
	stream := append(startup, []byte("Qextra")...)
	conn := &rw{in: bytes.NewReader(stream)}

	_, replay, err := (&Postgres{}).Resolve(conn)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(replay) != len(startup) {
		t.Errorf("replay length = %d, want %d (must not swallow the next message)", len(replay), len(startup))
	}
	rest, _ := io.ReadAll(conn.in)
	if string(rest) != "Qextra" {
		t.Errorf("unread remainder = %q, want %q", rest, "Qextra")
	}
}

func TestWriteErrorProducesValidErrorResponse(t *testing.T) {
	var buf bytes.Buffer
	if err := (&Postgres{}).WriteError(&buf, "3D000", "database \"nope\" does not exist"); err != nil {
		t.Fatalf("WriteError: %v", err)
	}

	got := buf.Bytes()
	if got[0] != 'E' {
		t.Fatalf("packet type = %q, want 'E'", got[0])
	}
	declared := binary.BigEndian.Uint32(got[1:5])
	if int(declared) != len(got)-1 {
		t.Errorf("declared length = %d, want %d (length covers itself and body, not the type byte)", declared, len(got)-1)
	}
	if got[len(got)-1] != 0 {
		t.Errorf("packet must end with the field terminator")
	}
	for _, want := range []string{"FATAL", "3D000", "does not exist"} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("packet missing %q", want)
		}
	}
}

func TestReadyAcceptsServerThatAnswersSSLProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		probe := make([]byte, 8)
		if _, err := io.ReadFull(conn, probe); err != nil {
			return
		}
		if binary.BigEndian.Uint32(probe[4:8]) != sslRequestCode {
			return
		}
		conn.Write([]byte{'N'})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := (&Postgres{}).Ready(ctx, ln.Addr().String()); err != nil {
		t.Errorf("Ready: %v", err)
	}
}

func TestReadyRejectsServerThatSaysNothing(t *testing.T) {
	// A port that accepts TCP but never speaks Postgres must not read as ready:
	// this is exactly the state a container is in mid-boot.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Hold the connection open without replying.
		time.Sleep(2 * time.Second)
		conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := (&Postgres{}).Ready(ctx, ln.Addr().String()); err == nil {
		t.Error("Ready must fail when the server never answers the probe")
	}
}

func TestReadyFailsOnClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing is listening now

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (&Postgres{}).Ready(ctx, addr); err == nil {
		t.Error("Ready must fail when nothing is listening")
	}
}

func TestPostgresMetadata(t *testing.T) {
	pg := &Postgres{}
	if pg.Name() != "postgres" {
		t.Errorf("Name = %q", pg.Name())
	}
	if pg.InternalPort() != 5432 {
		t.Errorf("InternalPort = %d", pg.InternalPort())
	}
	if pg.Routing() != SharedPortStartupRouting {
		t.Errorf("Postgres is client-speaks-first, so it must share one port")
	}
	if pg.Image() != "postgres:latest" {
		t.Errorf("Image = %q", pg.Image())
	}
	if custom := (&Postgres{Img: "postgres:16"}).Image(); custom != "postgres:16" {
		t.Errorf("Image override = %q, want postgres:16", custom)
	}

	env := pg.Env("haki", "secret", "mydb")
	for _, want := range []string{"POSTGRES_USER=haki", "POSTGRES_PASSWORD=secret", "POSTGRES_DB=mydb"} {
		if !contains(env, want) {
			t.Errorf("Env missing %q, got %v", want, env)
		}
	}
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry(&Postgres{})
	if _, ok := reg.Get("postgres"); !ok {
		t.Error("postgres must be registered")
	}
	if _, ok := reg.Get("mysql"); ok {
		t.Error("mysql is not registered yet")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
