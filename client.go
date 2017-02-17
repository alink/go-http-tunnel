package tunnel

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"github.com/mmatczuk/go-http-tunnel/log"
	"github.com/mmatczuk/go-http-tunnel/proto"
)

var (
	// DefaultTimeout specifies general purpose timeout.
	DefaultTimeout = 10 * time.Second
)

// ClientConfig is configuration of the Client.
type ClientConfig struct {
	// ServerAddr specifies TCP address of the tunnel server.
	ServerAddr string
	// TLSClientConfig specifies the tls configuration to use with
	// tls.Client.
	TLSClientConfig *tls.Config
	// DialTLS specifies an optional dial function that creates a tls
	// connection to the server. If DialTLS is nil, tls.Dial is used.
	DialTLS func(network, addr string, config *tls.Config) (net.Conn, error)
	// Backoff specifies backoff policy on server connection retry. If nil
	// when dial fails it will not be retried.
	Backoff Backoff
	// Tunnels specifies the tunnels client requests to be opened on server.
	Tunnels map[string]*proto.Tunnel
	// Proxy is ProxyFunc responsible for transferring data between server
	// and local services.
	Proxy ProxyFunc
	// Logger is optional logger. If nil logging is disabled.
	Logger log.Logger
}

// Client is responsible for creating connection to the server, handling control
// messages. It uses ProxyFunc for transferring data between server and local
// services.
type Client struct {
	config     *ClientConfig
	conn       net.Conn
	connMu     sync.Mutex
	httpServer *http2.Server
	serverErr  error
	logger     log.Logger
}

// NewClient creates a new unconnected Client based on configuration. Caller
// must invoke Start() on returned instance in order to connect server.
func NewClient(config *ClientConfig) *Client {
	if config.ServerAddr == "" {
		panic("Missing ServerAddr")
	}
	if config.TLSClientConfig == nil {
		panic("Missing TLSClientConfig")
	}
	if config.Tunnels == nil || len(config.Tunnels) == 0 {
		panic("Missing Tunnels")
	}
	if config.Proxy == nil {
		panic("Missing Proxy")
	}

	logger := config.Logger
	if logger == nil {
		logger = log.NewNopLogger()
	}

	c := &Client{
		config:     config,
		httpServer: &http2.Server{},
		logger:     logger,
	}

	return c
}

// Start connects client to the server, it returns error if there is a
// connection error, or server cannot open requested tunnels. On connection
// error a backoff policy is used to reestablish the connection. When connected
// HTTP/2 server is started to handle ControlMessages.
func (c *Client) Start() error {
	c.logger.Log(
		"level", 1,
		"action", "start",
	)

	for {
		conn, err := c.connect()
		if err != nil {
			return err
		}

		c.httpServer.ServeConn(conn, &http2.ServeConnOpts{
			Handler: http.HandlerFunc(c.serveHTTP),
		})

		c.logger.Log(
			"level", 1,
			"action", "disconnected",
		)

		c.connMu.Lock()
		err = c.serverErr
		c.conn = nil
		c.serverErr = nil
		c.connMu.Unlock()

		if err != nil {
			return fmt.Errorf("server error: %s", err)
		}
	}
}

func (c *Client) connect() (net.Conn, error) {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn != nil {
		return nil, fmt.Errorf("already connected")
	}

	conn, err := c.dial()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to server: %s", err)
	}
	c.conn = conn

	return conn, nil
}

func (c *Client) dial() (net.Conn, error) {
	var (
		network   = "tcp"
		addr      = c.config.ServerAddr
		tlsConfig = c.config.TLSClientConfig
	)

	doDial := func() (conn net.Conn, err error) {
		c.logger.Log(
			"level", 1,
			"action", "dial",
			"network", network,
			"addr", addr,
		)

		if c.config.DialTLS != nil {
			conn, err = c.config.DialTLS(network, addr, tlsConfig)
		} else {
			conn, err = tls.DialWithDialer(
				&net.Dialer{Timeout: DefaultTimeout},
				network, addr, tlsConfig,
			)
		}

		if err != nil {
			c.logger.Log(
				"level", 0,
				"msg", "dial failed",
				"network", network,
				"addr", addr,
				"err", err,
			)
		}

		return
	}

	b := c.config.Backoff
	if b == nil {
		return doDial()
	}

	for {
		conn, err := doDial()

		// success
		if err == nil {
			b.Reset()
			return conn, err
		}

		// failure
		d := b.NextBackOff()
		if d < 0 {
			return conn, fmt.Errorf("backoff limit exeded: %s", err)
		}

		// backoff
		c.logger.Log(
			"level", 1,
			"action", "backoff",
			"sleep", d,
		)
		time.Sleep(d)
	}
}

func (c *Client) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		if r.Header.Get(proto.ErrorHeader) != "" {
			c.handleHandshakeError(w, r)
		} else {
			c.handleHandshake(w, r)
		}
		return
	}

	msg, err := proto.ParseControlMessage(r.Header)
	if err != nil {
		c.logger.Log(
			"level", 1,
			"err", err,
		)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	c.logger.Log(
		"level", 2,
		"action", "handle",
		"ctrlMsg", msg,
	)
	switch msg.Action {
	case proto.Proxy:
		c.config.Proxy(w, r.Body, msg)
	default:
		c.logger.Log(
			"level", 0,
			"msg", "unknown action",
			"ctrlMsg", msg,
		)
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
	c.logger.Log(
		"level", 2,
		"action", "done",
		"ctrlMsg", msg,
	)
}

func (c *Client) handleHandshakeError(w http.ResponseWriter, r *http.Request) {
	err := fmt.Errorf(r.Header.Get(proto.ErrorHeader))

	c.logger.Log(
		"level", 1,
		"action", "handshake error",
		"addr", r.RemoteAddr,
		"err", err,
	)

	c.connMu.Lock()
	c.serverErr = err
	c.connMu.Unlock()
}

func (c *Client) handleHandshake(w http.ResponseWriter, r *http.Request) {
	c.logger.Log(
		"level", 1,
		"action", "handshake",
		"addr", r.RemoteAddr,
	)

	w.WriteHeader(http.StatusOK)

	b, err := json.Marshal(c.config.Tunnels)
	if err != nil {
		c.logger.Log(
			"level", 0,
			"msg", "handshake failed",
			"err", err,
		)
		return
	}
	w.Write(b)
}

// Stop disconnects client from server.
func (c *Client) Stop() {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	c.logger.Log(
		"level", 1,
		"action", "stop",
	)

	if c.conn != nil {
		c.conn.Close()
	}
	c.conn = nil
}
