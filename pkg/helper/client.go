package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Client forwards privileged operations to the helper daemon over its unix
// socket. The daemon verifies the caller's UID with SO_PEERCRED on each
// connection, so only the panel account's requests are honored.
type Client struct {
	Socket string
}

const (
	requestLimit  = 64 << 10
	responseLimit = 8 << 20 // journal-style output can be large; kill/service output stays small
)

// Call performs one request on a fresh connection. A nil receiver fails
// closed so callers can keep a single code path.
func (c *Client) Call(ctx context.Context, req Request) (string, error) {
	resp, err := c.send(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "rejected"
		}
		if resp.Output != "" {
			msg += "\n" + TrimOutput(resp.Output)
		}
		if hint := versionMismatchHint(resp); hint != "" {
			msg += "\n" + hint
		}
		return resp.Output, fmt.Errorf("%w: %s", ErrHelper, msg)
	}
	return resp.Output, nil
}

// Hello performs the protocol handshake and returns the helper's protocol
// version. An old helper answers "unknown operation" — that error is itself
// the signal that the running helper process predates the panel binary and
// needs a restart. Use it at panel startup to warn about a stale helper
// before individual operations start failing.
func (c *Client) Hello(ctx context.Context) (int, error) {
	resp, err := c.send(ctx, Request{Op: OpHello})
	if err != nil {
		return 0, err
	}
	return resp.Version, nil
}

// versionMismatchHint explains a failed call when the responding helper
// speaks a different protocol version than the panel: the shared binary was
// upgraded but the helper unit was not restarted, so new operations hit its
// old dispatch table.
func versionMismatchHint(resp *Response) string {
	if resp.Version == ProtocolVersion {
		return ""
	}
	return fmt.Sprintf("helper protocol version %d does not match the panel's %d; the lightpanel-helper service is running an outdated binary and must be restarted (systemctl restart lightpanel-helper)", resp.Version, ProtocolVersion)
}

func (c *Client) send(ctx context.Context, req Request) (*Response, error) {
	if c == nil || c.Socket == "" {
		return nil, fmt.Errorf("%w: helper not configured", ErrHelper)
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, fmt.Errorf("%w: helper socket %s: %v (is the lightpanel-helper service running?)", ErrHelper, c.Socket, err)
	}
	defer conn.Close()
	// Baseline 60s; a caller-provided context deadline may extend it (bounded
	// at 10 minutes) so long ACME issuances and package installs survive the
	// round-trip.
	deadline := time.Now().Add(60 * time.Second)
	if d2, ok := ctx.Deadline(); ok && d2.After(deadline) {
		deadline = d2
	}
	if max := time.Now().Add(10 * time.Minute); deadline.After(max) {
		deadline = max
	}
	_ = conn.SetDeadline(deadline)
	if err = json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("%w: send request: %v", ErrHelper, err)
	}
	var resp Response
	if err = json.NewDecoder(bufio.NewReader(io.LimitReader(conn, responseLimit))).Decode(&resp); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: helper closed the connection without a response", ErrHelper)
		}
		return nil, fmt.Errorf("%w: read response: %v", ErrHelper, err)
	}
	return &resp, nil
}
