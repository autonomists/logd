package client

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"time"

	"github.com/jeffrom/logd/config"
	"github.com/jeffrom/logd/internal"
	"github.com/jeffrom/logd/protocol"
)

// Client represents a connection to the database
type Client struct { // nolint: golint
	net.Conn
	conf  *Config
	gconf *config.Config

	readTimeout  time.Duration
	writeTimeout time.Duration

	bw      *bufio.Writer
	br      *bufio.Reader
	cr      *protocol.ClientResponse
	readreq *protocol.Read
	tailreq *protocol.Tail
	bs      *protocol.BatchScanner
}

// New returns a new instance of Client without a net.Conn
func New(conf *Config) *Client {
	// timeout := time.Duration(conf.ClientTimeout) * time.Millisecond
	gconf := conf.toGeneralConfig()
	c := &Client{
		conf:         conf,
		gconf:        gconf,
		cr:           protocol.NewClientResponse(gconf),
		bs:           protocol.NewBatchScanner(gconf, nil),
		readreq:      protocol.NewRead(gconf),
		tailreq:      protocol.NewTail(gconf),
		readTimeout:  conf.getReadTimeout(),
		writeTimeout: conf.getWriteTimeout(),
	}

	return c
}

// Dial returns a new instance of Conn
func Dial(addr string) (*Client, error) {
	return DialConfig(addr, DefaultConfig)
}

// DialConfig returns a configured Conn
func DialConfig(addr string, conf *Config) (*Client, error) {
	// internal.Debugf(conf, "starting options: %s", conf)
	var conn net.Conn
	var err error

	conn, err = net.Dial("tcp", addr)
	if err != nil {
		if conn != nil {
			internal.LogError(conn.Close())
		}

		return nil, err
	}

	c := New(conf)
	c.Conn = conn
	c.bw = bufio.NewWriterSize(conn, conf.BatchSize)
	c.br = bufio.NewReaderSize(conn, conf.BatchSize)
	return c, nil
}

func (c *Client) reset() {
	c.cr.Reset()
	c.readreq.Reset()
	c.tailreq.Reset()
}

// SetConn sets net.Conn for a client.
func (c *Client) SetConn(conn net.Conn) *Client {
	c.Conn = conn
	if c.bw == nil {
		c.bw = bufio.NewWriterSize(conn, c.conf.BatchSize)
	} else {
		c.bw.Reset(conn)
	}
	if c.br == nil {
		c.br = bufio.NewReaderSize(conn, c.conf.BatchSize)
	} else {
		c.br.Reset(conn)
	}
	return c
}

// Batch sends a batch request and returns the response.
func (c *Client) Batch(batch *protocol.Batch) (uint64, error) {
	internal.Debugf(c.gconf, "%v -> %s", batch, c.Conn.RemoteAddr())
	if _, err := c.send(batch); err != nil {
		return 0, err
	}
	off, _, err := c.readBatchResponse()
	return off, err
}

// ReadOffset sends a READ request, returning a scanner that can be used to
// iterate over the messages in the response.
func (c *Client) ReadOffset(offset uint64, limit int) (int, *protocol.BatchScanner, error) {
	internal.Debugf(c.gconf, "READ %d %d -> %s", offset, limit, c.Conn.RemoteAddr())
	req := c.readreq
	req.Reset()
	req.Offset = offset
	req.Messages = limit

	if _, err := c.send(req); err != nil {
		return 0, nil, err
	}

	respOff, nbatches, err := c.readBatchResponse()
	if err != nil {
		return 0, nil, err
	}
	if respOff != offset {
		log.Printf("response offset (%d) did not match request (%d)", respOff, offset)
		return 0, nil, protocol.ErrInternal
	}

	c.bs.Reset(c.br)
	internal.LogError(c.SetReadDeadline(time.Now().Add(c.readTimeout)))
	return nbatches, c.bs, nil
}

// Tail sends a TAIL request, returning the initial offset and a scanner
// starting from the first available batch.
func (c *Client) Tail(limit int) (uint64, int, *protocol.BatchScanner, error) {
	internal.Debugf(c.gconf, "TAIL %d -> %s", limit, c.Conn.RemoteAddr())
	req := c.tailreq
	req.Reset()
	req.Messages = limit

	if _, err := c.send(req); err != nil {
		return 0, 0, nil, err
	}

	respOff, nbatches, err := c.readBatchResponse()
	if err != nil {
		return 0, 0, nil, err
	}

	c.bs.Reset(c.br)
	internal.LogError(c.SetReadDeadline(time.Now().Add(c.readTimeout)))
	return respOff, nbatches, c.bs, nil
}

// Close sends a CLOSE request and then closes the connection
func (c *Client) Close() error {
	defer func() {
		internal.LogError(c.Conn.Close())
	}()

	closereq := protocol.NewCloseRequest(c.gconf)
	if _, err := c.send(closereq); err != nil {
		return err
	}

	if err := c.readCloseResponse(); err != nil {
		return err
	}

	return nil
}

func (c *Client) send(wt io.WriterTo) (int64, error) {
	internal.LogError(c.SetWriteDeadline(time.Now().Add(c.writeTimeout)))
	n, err := wt.WriteTo(c.bw)
	if c.handleErr(err) != nil {
		return 0, err
	}
	internal.Debugf(c.gconf, "wrote %d bytes to %s", n, c.Conn.RemoteAddr())

	err = c.flush()
	if c.handleErr(err) != nil {
		return n, err
	}

	internal.LogError(c.SetWriteDeadline(time.Time{}))
	return n, err
}

// flush flushes all pending data to the server
func (c *Client) flush() error {
	if c.bw.Buffered() > 0 {
		internal.Debugf(c.gconf, "client.Flush() (%d bytes)", c.bw.Buffered())
		internal.LogError(c.SetWriteDeadline(time.Now().Add(c.writeTimeout)))
		err := c.bw.Flush()
		internal.Debugf(c.gconf, "client.Flush() complete")
		internal.LogError(c.SetWriteDeadline(time.Time{}))
		return err
	}
	return nil
}

func (c *Client) readBatchResponse() (uint64, int, error) {
	c.cr.Reset()
	internal.LogError(c.SetReadDeadline(time.Now().Add(c.readTimeout)))
	n, err := c.cr.ReadFrom(c.br)
	internal.LogError(c.SetReadDeadline(time.Time{}))
	internal.Debugf(c.gconf, "read %d bytes from %s: %+v", n, c.Conn.RemoteAddr(), c.cr)
	c.handleErr(err)
	if err != nil {
		return 0, 0, err
	}
	return c.cr.Offset(), c.cr.Batches(), c.cr.Error()
}

func (c *Client) readCloseResponse() error {
	c.cr.Reset()
	internal.LogError(c.SetReadDeadline(time.Now().Add(c.readTimeout)))
	n, err := c.cr.ReadFrom(c.br)
	internal.LogError(c.SetReadDeadline(time.Time{}))
	internal.Debugf(c.gconf, "read %d bytes from %s: %+v", n, c.Conn.RemoteAddr(), c.cr)
	c.handleErr(err)
	if err != nil {
		return err
	}
	if !c.cr.Ok() {
		return errors.New("close failed")
	}
	return nil
}

func (c *Client) handleErr(err error) error {
	if err == nil {
		return err
	}
	if err == io.EOF {
		internal.DebugfDepth(c.gconf, 1, "%s closed the connection", c.Conn.RemoteAddr())
	} else {
		internal.DebugfDepth(c.gconf, 1, "%+v", err)
	}
	return err
}
