package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// Postgres wire protocol constants.
const (
	// protocolVersion3 is the 3.0 protocol identifier (major 3, minor 0).
	protocolVersion3 = 196608

	// Magic codes sent in place of a protocol version.
	cancelRequestCode = 80877102
	sslRequestCode    = 80877103
	gssEncRequestCode = 80877104

	// A startup packet is small; anything larger is a client bug or an attack.
	maxStartupLen = 10000

	// minStartupLen covers the 4-byte length plus the 4-byte version/code.
	minStartupLen = 8
)

// Postgres implements Engine for PostgreSQL.
type Postgres struct {
	// Img overrides the default image when set.
	Img string
}

func (p *Postgres) Name() string { return "postgres" }

func (p *Postgres) Image() string {
	if p.Img != "" {
		return p.Img
	}
	return "postgres:latest"
}

func (p *Postgres) InternalPort() int { return 5432 }

func (p *Postgres) DataDir() string { return "/var/lib/postgresql/data" }

func (p *Postgres) Env(user, password, database string) []string {
	return []string{
		fmt.Sprintf("POSTGRES_USER=%s", user),
		fmt.Sprintf("POSTGRES_PASSWORD=%s", password),
		fmt.Sprintf("POSTGRES_DB=%s", database),
	}
}

// Routing: Postgres is client-speaks-first, so every database shares one port
// and the target is read from the StartupMessage.
func (p *Postgres) Routing() RoutingMode { return SharedPortStartupRouting }

// Resolve reads the client's startup handshake and returns the target database
// plus the bytes to replay to the backend.
//
// The client may first send an SSLRequest or GSSENCRequest. The proxy speaks
// plaintext, so those are declined with 'N' and we keep reading until the real
// StartupMessage arrives. Declined negotiation packets are NOT replayed: the
// backend connection is a fresh plaintext socket.
func (p *Postgres) Resolve(rw io.ReadWriter) (string, []byte, error) {
	for {
		var header [4]byte
		if _, err := io.ReadFull(rw, header[:]); err != nil {
			return "", nil, fmt.Errorf("read startup length: %w", err)
		}
		msgLen := binary.BigEndian.Uint32(header[:])
		if msgLen < minStartupLen || msgLen > maxStartupLen {
			return "", nil, fmt.Errorf("invalid startup packet length %d", msgLen)
		}

		// The length counts itself, so the remaining body is msgLen-4.
		body := make([]byte, msgLen-4)
		if _, err := io.ReadFull(rw, body); err != nil {
			return "", nil, fmt.Errorf("read startup body: %w", err)
		}

		code := binary.BigEndian.Uint32(body[:4])
		switch code {
		case sslRequestCode, gssEncRequestCode:
			// Decline encryption and wait for the plaintext StartupMessage.
			if _, err := rw.Write([]byte{'N'}); err != nil {
				return "", nil, fmt.Errorf("decline encryption: %w", err)
			}
			continue
		case cancelRequestCode:
			return "", nil, ErrCancelRequest
		}

		if code != protocolVersion3 {
			return "", nil, fmt.Errorf("unsupported postgres protocol version %d", code)
		}

		params := parseStartupParams(body[4:])
		database := params["database"]
		if database == "" {
			// Postgres defaults the database name to the user name.
			database = params["user"]
		}
		if database == "" {
			return "", nil, ErrNoDatabase
		}

		replay := make([]byte, 0, len(header)+len(body))
		replay = append(replay, header[:]...)
		replay = append(replay, body...)
		return database, replay, nil
	}
}

// parseStartupParams decodes the null-terminated key/value pairs that follow
// the protocol version, stopping at the terminating empty key.
func parseStartupParams(buf []byte) map[string]string {
	params := make(map[string]string)
	for {
		key, rest, ok := nextCString(buf)
		if !ok || key == "" {
			return params
		}
		value, rest2, ok := nextCString(rest)
		if !ok {
			return params
		}
		params[key] = value
		buf = rest2
	}
}

// nextCString splits off one null-terminated string from buf.
func nextCString(buf []byte) (string, []byte, bool) {
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i]), buf[i+1:], true
		}
	}
	return "", nil, false
}

// WriteError emits a Postgres ErrorResponse ('E') so psql prints a real
// message. Layout: type byte, Int32 length, then null-terminated fields keyed
// by a type byte, closed by a zero byte.
func (p *Postgres) WriteError(w io.Writer, code, message string) error {
	fields := []struct {
		key byte
		val string
	}{
		{'S', "FATAL"},
		{'V', "FATAL"},
		{'C', code},
		{'M', message},
	}

	body := make([]byte, 0, 64)
	for _, f := range fields {
		body = append(body, f.key)
		body = append(body, f.val...)
		body = append(body, 0)
	}
	body = append(body, 0) // terminator

	packet := make([]byte, 0, 5+len(body))
	packet = append(packet, 'E')
	// The length covers itself plus the body, but not the leading type byte.
	packet = binary.BigEndian.AppendUint32(packet, uint32(4+len(body)))
	packet = append(packet, body...)

	_, err := w.Write(packet)
	return err
}

// Ready reports whether Postgres at addr is accepting connections. A bare TCP
// dial is not conclusive during startup, so we send an SSLRequest and require
// the server to answer 'S' or 'N' — proof the postmaster is serving.
func (p *Postgres) Ready(ctx context.Context, addr string) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}

	probe := make([]byte, 8)
	binary.BigEndian.PutUint32(probe[0:4], 8)
	binary.BigEndian.PutUint32(probe[4:8], sslRequestCode)
	if _, err := conn.Write(probe); err != nil {
		return fmt.Errorf("probe write: %w", err)
	}

	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return fmt.Errorf("probe read: %w", err)
	}
	if reply[0] != 'S' && reply[0] != 'N' {
		return fmt.Errorf("unexpected probe reply %q", reply[0])
	}
	return nil
}
