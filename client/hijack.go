package client // import "github.com/docker/docker/client"

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/go-connections/sockets"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

// postHijacked sends a POST request and hijacks the connection.
func (cli *Client) postHijacked(ctx context.Context, path string, query url.Values, body interface{}, headers map[string][]string) (types.HijackedResponse, error) {
	bodyEncoded, err := encodeData(body)
	if err != nil {
		return types.HijackedResponse{}, err
	}

	apiPath := cli.getAPIPath(path, query)
	req, err := http.NewRequest("POST", apiPath, bodyEncoded)
	if err != nil {
		return types.HijackedResponse{}, err
	}
	req = cli.addHeaders(req, headers)

	conn, err := cli.setupHijackConn(req, "tcp")
	if err != nil {
		return types.HijackedResponse{}, err
	}

	return types.HijackedResponse{Conn: conn, Reader: bufio.NewReader(conn)}, err
}

func (cli *Client) dial() (net.Conn, error) {
	if cli.proto == "npipe" {
		return sockets.DialPipe(cli.addr, 32*time.Second)
	}
	return cli.client.Transport.(*http.Transport).Dial(cli.proto, cli.addr)
}

func (cli *Client) setupHijackConn(req *http.Request, proto string) (net.Conn, error) {
	req.Host = cli.addr
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", proto)

	conn, err := cli.dial()
	if err != nil {
		return nil, errors.Wrap(err, "cannot connect to the Docker daemon. Is 'docker daemon' running on this host?")
	}

	// When we set up a TCP connection for hijack, there could be long periods
	// of inactivity (a long running command with no output) that in certain
	// network setups may cause ECONNTIMEOUT, leaving the client in an unknown
	// state. Setting TCP KeepAlive on the socket connection will prohibit
	// ECONNTIMEOUT unless the socket connection truly is broken
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	clientconn := httputil.NewClientConn(conn, nil)
	defer clientconn.Close()

	// Server hijacks the connection, error 'connection closed' expected
	resp, err := clientconn.Do(req)
	if err != httputil.ErrPersistEOF {
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusSwitchingProtocols {
			resp.Body.Close()
			return nil, fmt.Errorf("unable to upgrade to %s, received %d", proto, resp.StatusCode)
		}
	}

	c, br := clientconn.Hijack()
	if br.Buffered() > 0 {
		// If there is buffered content, wrap the connection
		c = &hijackedConn{c, br}
	} else {
		br.Reset(nil)
	}

	return c, nil
}

type hijackedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *hijackedConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}
