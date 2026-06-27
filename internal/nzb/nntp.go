package nzb

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
)

// ServerConfig holds the connection parameters for one NNTP server.
type ServerConfig struct {
	Host        string
	Port        int
	User        string
	Pass        string
	SSL         bool
	Connections int // informational; single-connection client ignores this
}

// Client is a sequential NNTP connection. A single connection is used; callers
// must not invoke methods concurrently.
type Client struct {
	tp   *textproto.Conn
	conn net.Conn
}

// Dial opens a TCP (or TLS) connection to the NNTP server described by cfg,
// performs the initial handshake, and authenticates when credentials are set.
// Default ports: 119 (plain), 563 (SSL) when cfg.Port == 0.
func Dial(cfg ServerConfig) (*Client, error) {
	port := cfg.Port
	if port == 0 {
		if cfg.SSL {
			port = 563
		} else {
			port = 119
		}
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))

	var conn net.Conn
	var err error
	if cfg.SSL {
		conn, err = tls.Dial("tcp", addr, &tls.Config{
			ServerName: cfg.Host,
		})
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("nntp: dial %s: %w", addr, err)
	}

	tp := textproto.NewConn(conn)

	// Read server greeting: 200 (post allowed) or 201 (read-only).
	code, _, err := tp.ReadResponse(0)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("nntp: greeting: %w", err)
	}
	if code != 200 && code != 201 {
		conn.Close()
		return nil, fmt.Errorf("nntp: unexpected greeting code %d", code)
	}

	c := &Client{tp: tp, conn: conn}

	if cfg.User != "" {
		if err := c.authinfo(cfg.User, cfg.Pass); err != nil {
			c.Close()
			return nil, err
		}
	}

	return c, nil
}

// authinfo performs AUTHINFO USER / PASS authentication.
func (c *Client) authinfo(user, pass string) error {
	if err := c.tp.PrintfLine("AUTHINFO USER %s", user); err != nil {
		return fmt.Errorf("nntp: AUTHINFO USER: %w", err)
	}
	code, _, err := c.tp.ReadResponse(0)
	if err != nil {
		return fmt.Errorf("nntp: AUTHINFO USER response: %w", err)
	}
	switch code {
	case 281:
		// accepted without password
		return nil
	case 381:
		// password required
	default:
		return fmt.Errorf("nntp: AUTHINFO USER: unexpected code %d", code)
	}

	if err := c.tp.PrintfLine("AUTHINFO PASS %s", pass); err != nil {
		return fmt.Errorf("nntp: AUTHINFO PASS: %w", err)
	}
	if _, _, err := c.tp.ReadResponse(281); err != nil {
		return fmt.Errorf("nntp: authentication failed: %w", err)
	}
	return nil
}

// Body fetches the body of the article identified by messageID, yEnc-decodes
// it, and writes the decoded bytes to w. The leading/trailing angle brackets
// on messageID are optional; Dial adds them if absent.
func (c *Client) Body(messageID string, w io.Writer) error {
	// Normalise message-id: ensure angle brackets.
	if len(messageID) == 0 || messageID[0] != '<' {
		messageID = "<" + messageID + ">"
	}

	if err := c.tp.PrintfLine("BODY %s", messageID); err != nil {
		return fmt.Errorf("nntp: BODY send: %w", err)
	}

	// Expect 222 (body follows).
	if _, _, err := c.tp.ReadResponse(222); err != nil {
		return fmt.Errorf("nntp: BODY %s: %w", messageID, err)
	}

	// DotReader handles dot-unstuffing of the dot-terminated article body.
	dr := c.tp.DotReader()
	return DecodeYenc(dr, w)
}

// Close sends QUIT and closes the underlying connection.
func (c *Client) Close() error {
	_ = c.tp.PrintfLine("QUIT")
	_, _, _ = c.tp.ReadResponse(205)
	return c.conn.Close()
}
