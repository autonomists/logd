package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/jeffrom/logd/config"
	"github.com/jeffrom/logd/events"
	"github.com/jeffrom/logd/internal"
	"github.com/jeffrom/logd/protocol"
)

// Socket handles socket connections
type Socket struct {
	config *config.Config

	addr string
	ln   net.Listener
	mu   sync.Mutex

	conns  map[*Conn]bool
	connMu sync.Mutex
	connIn chan *Conn

	readyC       chan struct{}
	stopC        chan struct{}
	shutdownC    chan struct{}
	shuttingDown bool

	q *events.EventQ
}

// NewSocket will return a new instance of a log server
func NewSocket(addr string, config *config.Config) *Socket {
	log.Printf("starting options: %s", config)
	q := events.NewEventQ(config)
	if err := q.Start(); err != nil {
		panic(err)
	}

	return &Socket{
		config:    config,
		addr:      addr,
		readyC:    make(chan struct{}),
		conns:     make(map[*Conn]bool),
		connIn:    make(chan *Conn, 1000),
		stopC:     make(chan struct{}),
		shutdownC: make(chan struct{}),
		q:         q,
	}
}

// ListenAndServe starts serving requests
func (s *Socket) ListenAndServe() error {
	return s.listenAndServe(false)
}

// ListenAddress returns the listen address of the server.
func (s *Socket) ListenAddress() net.Addr {
	return s.ln.Addr()
}

func (s *Socket) listenAndServe(wait bool) error {
	var outerErr error

	s.mu.Lock()
	s.ln, outerErr = net.Listen("tcp", s.addr)
	s.mu.Unlock()
	if outerErr != nil {
		return outerErr
	}

	log.Printf("Serving at %s", s.ln.Addr())
	if wait {
		select {
		case s.readyC <- struct{}{}:
		}
	}

	go s.accept()

	for {
		select {
		case <-s.stopC:
			log.Printf("Shutting down server at %s", s.ln.Addr())
			s.logConns()
			return s.shutdown()
		case conn := <-s.connIn:
			go s.handleConnection(conn)
		}
	}
}

// Respond satisfies Server interface
// func (s *SocketServer) Respond(cmd *Command, resp *Response) error {
// 	return nil
// }

// Send satisfies Server interface
// func (s *SocketServer) Send(sub *Subscription, msg *Message) error {
// 	return nil
// }

// ready signals that the application is ready to serve on this host:port
func (s *Socket) ready() {
	select {
	case <-s.readyC:
	}
}

func (s *Socket) isShuttingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.shuttingDown
}

func (s *Socket) accept() {
	for {
		if s.isShuttingDown() {
			break
		}

		rawConn, err := s.ln.Accept()
		if err != nil {
			break
		}
		if s.isShuttingDown() {
			log.Printf("Closed new connection from %s because shutting down", rawConn.RemoteAddr())
			rawConn.Close()
			break
		}

		s.q.Stats.Incr("total_connections")
		internal.Debugf(s.config, "accept: %s", rawConn.RemoteAddr())

		conn := newServerConn(rawConn, s.config)
		s.addConn(conn)

		s.connIn <- conn
	}
}

// GoServe starts a server without blocking the current goroutine
func (s *Socket) GoServe() {
	go func() {
		if err := s.listenAndServe(true); err != nil {
			log.Printf("error serving: %v", err)
		}
	}()
	s.ready()
}

// shutdown shuts down the server
func (s *Socket) shutdown() error {
	defer func() {
		select {
		case s.shutdownC <- struct{}{}:
		}
	}()

	s.mu.Lock()
	s.shuttingDown = true
	s.mu.Unlock()

	err := s.q.Stop()

	wg := sync.WaitGroup{}
	s.connMu.Lock()
	for c := range s.conns {
		wg.Add(1)
		go func(c *Conn) {
			defer wg.Done()

			if c.isActive() {
				select {
				case <-c.done:
					internal.Debugf(s.config, "%s closed gracefully", c.RemoteAddr())
				case <-time.After(time.Duration(s.config.GracefulShutdownTimeout) * time.Millisecond):
					log.Printf("%s timed out", c.RemoteAddr())
				}
			}

			s.removeConn(c)
		}(c)
	}
	s.connMu.Unlock()
	wg.Wait()

	return err
}

// Conns returns a list of current connections. For debugging.
func (s *Socket) Conns() []*Conn {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	conns := make([]*Conn, len(s.conns))
	i := 0
	for conn := range s.conns {
		conns[i] = conn
		i++
	}
	return conns
}

func (s *Socket) logConns() {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	var states []string
	for c := range s.conns {
		state := fmt.Sprintf("%s(%s)", c.Conn.RemoteAddr(), c.getState())
		states = append(states, state)
	}

	log.Printf("connection states (%d): %s", len(states), strings.Join(states, ", "))
}

// Stop can be called to shut down the server
func (s *Socket) Stop() error {
	select {
	case s.stopC <- struct{}{}:
	}

	select {
	case <-s.shutdownC:
	}

	return nil
}

func (s *Socket) addConn(conn *Conn) {
	conn.setState(connStateInactive)
	s.connMu.Lock()
	s.conns[conn] = true
	s.connMu.Unlock()
}

func (s *Socket) removeConn(conn *Conn) {
	if err := conn.Close(); err != nil {
		log.Printf("error removing connection: %+v", err)
	}

	s.connMu.Lock()
	delete(s.conns, conn)
	s.connMu.Unlock()
}

func handleConnErr(config *config.Config, err error, conn *Conn) error {
	if err == nil {
		return nil
	}
	if err == io.EOF {
		internal.Debugf(config, "%s closed the connection", conn.RemoteAddr())
	} else if err, ok := err.(net.Error); ok && err.Timeout() {
		internal.Logf("%s timed out: %s", conn.RemoteAddr(), debug.Stack())
	} else if err != nil {
		conn.setState(connStateFailed)

		log.Printf("error handling connection: %+v", err)
	}
	return err
}

func (s *Socket) handleConnection(conn *Conn) {
	s.q.Stats.Incr("connections")

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	ctx, cancel = context.WithCancel(context.Background())

	defer func() {
		cancel()
		s.q.Stats.Decr("connections")

		s.removeConn(conn)
		if err := conn.close(); err != nil {
			internal.Debugf(s.config, "error closing connection: %+v", err)
		}
	}()

	for {
		if s.isShuttingDown() {
			internal.Debugf(s.config, "closing connection to %s due to shutdown", conn.RemoteAddr())
			break
		}

		if err := conn.setWaitForCmdDeadline(); err != nil {
			log.Printf("%s error: %+v", conn.RemoteAddr(), err)
			return
		}

		// wait for a command
		cmd, err := s.readCommand(conn)
		if cerr := handleConnErr(s.config, err, conn); cerr != nil {
			return
		}
		internal.Debugf(s.config, "%s<-%s: %s", conn.LocalAddr(), conn.RemoteAddr(), cmd)

		// just waited for io, so check if we're in shutdown
		if s.isShuttingDown() {
			internal.Debugf(s.config, "closing connection to %s due to shutdown", conn.RemoteAddr())
			break
		}

		resp, exerr := s.executeCommand(ctx, conn, cmd)
		if exerr != nil {
			s.q.Stats.Incr("command_errors")
			return
		}

		// after sending some more io, check for shutdown again
		if s.isShuttingDown() {
			internal.Debugf(s.config, "closing connection to %s due to shutdown", conn.RemoteAddr())
			break
		}

		// finish the loop if the command was CLOSE/0
		if s.handleClose(ctx, conn, cmd, resp) {
			internal.Debugf(s.config, "closing %s", conn.RemoteAddr())
			break
		}

		// handle a single read or go into subscriber state
		if !resp.Failed() {
			s.handleRead(ctx, conn, cmd, resp)
		}

		// clean up the request in preparation for the next, or go into
		// subscriber mode
		s.finishCommand(ctx, conn, cmd, resp)
	}
}

func (s *Socket) readCommand(conn *Conn) (*protocol.Command, error) {
	internal.Debugf(s.config, "waiting to read a new command")
	cmd, err := conn.pr.ReadCommand(conn)
	if cmd != nil && conn != nil {
		cmd.ConnID = conn.id
	}
	return cmd, err
}

func (s *Socket) executeCommand(ctx context.Context, conn *Conn, cmd *protocol.Command) (*protocol.Response, error) {
	timeout := time.Duration(s.config.ServerTimeout) * time.Millisecond
	cmdCtx, cmdCancel := context.WithTimeout(ctx, timeout)
	defer cmdCancel()

	// push to event queue and wait for a result
	resp, err := s.q.PushCommand(cmdCtx, cmd)

	s.q.Stats.Incr("total_commands")
	if cerr := handleConnErr(s.config, err, conn); cerr != nil {
		s.q.Stats.Incr("command_errors")
		return resp, cerr
	}

	// now we've waited to hear back from the event queue, so check if we're in shutdown again
	if s.isShuttingDown() {
		internal.Debugf(s.config, "closing connection to %s due to shutdown", conn.RemoteAddr())
		return resp, nil
	}

	if s.handleShutdownRequest(conn, cmd, resp) {
		internal.Debugf(s.config, "shutdown request from %s", conn.RemoteAddr())
		return resp, nil
	}

	respBytes, err := resp.SprintBytes()
	if err != nil {
		log.Printf("error formatting response bytes: %+v", err)
		return resp, err
	}

	// create a new reader channel now if we're reading. if a previous readerC
	// was here, we don't care about it anymore
	if cmd.IsRead() {
		resp.ReaderC = make(chan io.Reader, 1000)
	}

	// send a response
	if werr := conn.setWriteDeadline(); werr != nil {
		s.q.Stats.Incr("connection_errors")
		log.Printf("error setting %s write deadline: %+v", conn.RemoteAddr(), werr)
		return resp, werr
	}

	n, err := s.readPending(ctx, conn, resp)
	s.q.Stats.Add("total_bytes_written", int64(n))
	if err != nil {
		log.Printf("%s: error reading pending buffers: %+v", conn.RemoteAddr(), err)
		return resp, err
	}

	n, err = conn.write(respBytes)
	s.q.Stats.Add("total_bytes_written", int64(n))

	if herr := handleConnErr(s.config, err, conn); herr != nil {
		s.q.Stats.Incr("connection_errors")
		log.Printf("error writing to %s: %+v", conn.RemoteAddr(), err)
		return resp, herr
	}

	return resp, nil
}

func (s *Socket) finishCommand(ctx context.Context, conn *Conn, cmd *protocol.Command, resp *protocol.Response) {
	if cmd.IsRead() {
		return
	}

	conn.setState(connStateInactive)
}

func (s *Socket) handleRead(ctx context.Context, conn *Conn, cmd *protocol.Command, resp *protocol.Response) {
	if !cmd.IsRead() {
		return
	}

	cmd.SignalReady()
	internal.Debugf(s.config, "reading log messages to %s", conn.RemoteAddr())
	conn.setState(connStateReading)

	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		log.Printf("error setting write deadline to %s: %+v", conn.RemoteAddr(), err)
		return
	}

	go s.handleSubscriber(ctx, conn, cmd, resp)
}

func (s *Socket) handleClose(ctx context.Context, conn *Conn, cmd *protocol.Command, resp *protocol.Response) bool {
	if cmd.Name != protocol.CmdClose {
		return false
	}

	return true
}

func (s *Socket) handleShutdownRequest(conn *Conn, cmd *protocol.Command, resp *protocol.Response) bool {
	if cmd.Name == protocol.CmdShutdown && resp.Status == protocol.RespOK {
		s.removeConn(conn)
		if err := s.Stop(); err != nil {
			log.Print(err)
			return false
		}
		return true
	}
	return false
}

func (s *Socket) handleSubscriber(ctx context.Context, conn *Conn, cmd *protocol.Command, resp *protocol.Response) {
	s.q.Stats.Incr("subscriptions")
	s.q.Stats.Incr("total_subscriptions")

	subCtx, subCancel := context.WithCancel(ctx)

	defer func() {
		s.q.Stats.Decr("subscriptions")
		if _, err := s.readPending(subCtx, conn, resp); err != nil {
			log.Printf("%s: error reading pending buffers: %+v", conn.RemoteAddr(), err)
		}

		conn.mu.Lock()
		if err := conn.Flush(); err != nil {
			log.Printf("failed to flush connection while closing (connection was probably closed by the client): %+v", err)
		}
		conn.mu.Unlock()

		subCancel()
	}()

	for {
		internal.Debugf(s.config, "%s: waiting for subscription event", conn.RemoteAddr())

		select {
		case r := <-resp.ReaderC:
			internal.Debugf(s.config, "%s <-reader: %+v", conn.RemoteAddr(), r)

			if err := s.sendReader(subCtx, r, conn); err != nil {
				log.Printf("%s: error sending reader: %+v", conn.RemoteAddr(), err)
				return
			}

		case <-ctx.Done():
			internal.Debugf(s.config, "%s: subscriber context received <-Done", conn.RemoteAddr())

			close(resp.ReaderC)
			return
		}
	}
}

func (s *Socket) sendReader(ctx context.Context, r io.Reader, conn *Conn) error {
	n, err := conn.readFrom(r)
	s.q.Stats.Add("total_bytes_written", int64(n))
	if err != nil {
		log.Printf("%s error: %+v", conn.RemoteAddr(), err)
		return err
	}

	// unwrap the os.file, same as go stdlib sendfile optimization logic
	lr, ok := r.(*io.LimitedReader)
	if ok {
		r = lr.R
	}

	f, ok := r.(*os.File)
	if ok {
		internal.Debugf(s.config, "%s: closing %s", conn.RemoteAddr(), f.Name())
		if cerr := f.Close(); err != nil {
			log.Printf("error closing reader to %s: %+v", conn.RemoteAddr(), cerr)
			return cerr
		}
	}
	return nil
}

func (s *Socket) readPending(ctx context.Context, c *Conn, resp *protocol.Response) (int64, error) {
	// prevState := c.getState()
	// c.setState(connStateReading)
	// defer c.setState(prevState)
	var read int64
	numRead := 0

	if s.isShuttingDown() {
		return 0, nil
	}

Loop:
	for {
		select {
		case r := <-resp.ReaderC:
			if r == nil {
				continue
			}
			n, rerr := c.readFrom(r)
			numRead++
			read += n

			if closer, ok := r.(io.Closer); ok {
				if err := closer.Close(); err != nil {
					log.Printf("error closing %s: %+v", c.RemoteAddr(), err)
				}
			}
			if rerr != nil {
				return read, rerr
			}
		case <-ctx.Done():
			break Loop
		default:
			break Loop
		}
	}
	internal.Debugf(c.config, "%s: read %d pending readers", c.RemoteAddr(), numRead)
	return read, nil
}