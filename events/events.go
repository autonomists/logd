package events

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/jeffrom/logd/config"
	"github.com/jeffrom/logd/internal"
	"github.com/jeffrom/logd/logger"
	"github.com/jeffrom/logd/protocol"
)

// this file contains the core logic of the program. Commands come from the
// various inputs. They are handled and a response is given. For example, a
// message is received, it is written to a backend, and a log id is returned to
// the caller. Or, a tail command is received, and the caller receives a log
// stream.

// TODO use an array of send close <- struct{}{} functions to run on shutdown
// instead of doing each one manually

// EventQ manages the receiving, processing, and responding to events.
type EventQ struct {
	config        *config.Config
	currID        uint64
	in            chan *protocol.Command
	close         chan struct{}
	subscriptions map[string]*Subscription
	log           logger.Logger
	Stats         *internal.Stats
}

// NewEventQ creates a new instance of an EventQ
func NewEventQ(conf *config.Config) *EventQ {
	log := logger.NewFileLogger(conf)

	q := &EventQ{
		config:        conf,
		in:            make(chan *protocol.Command, 1000),
		close:         make(chan struct{}),
		subscriptions: make(map[string]*Subscription),
		log:           log,
		Stats:         internal.NewStats(),
	}

	return q
}

func (q *EventQ) Start() error {
	if manager, ok := q.log.(logger.LogManager); ok {
		if err := manager.Setup(); err != nil {
			panic(err)
		}
	}

	head, err := q.log.Head()
	if err != nil {
		return err
	}
	q.currID = head + 1

	go q.loop()
	return nil
}

func (q *EventQ) loop() {
	for {
		select {
		case cmd := <-q.in:
			internal.Debugf(q.config, "event: %s(%q)", cmd, cmd.Args)

			switch cmd.Name {
			case protocol.CmdMessage:
				q.handleMsg(cmd)
			case protocol.CmdReplicate:
				q.handleReplicate(cmd)
			// TODO maybe remove rawmessage and change replicate? It would be
			// best if both readers and replicas got the same optimizations.
			// For example, stream messages as they come in, but if partitions
			// are being written fast enough, wait until a partition has been
			// written and then just sendfile it.
			case protocol.CmdRawMessage:
				q.handleRawMsg(cmd)
			case protocol.CmdRead:
				q.handleRead(cmd)
			case protocol.CmdHead:
				q.handleHead(cmd)
			case protocol.CmdStats:
				q.handleStats(cmd)
			case protocol.CmdPing:
				q.handlePing(cmd)
			case protocol.CmdClose:
				q.handleClose(cmd)
			case protocol.CmdSleep:
				q.handleSleep(cmd)
			case protocol.CmdShutdown:
				if err := q.HandleShutdown(cmd); err != nil {
					cmd.Respond(protocol.NewResponse(q.config, protocol.RespErr))
				} else {
					cmd.Respond(protocol.NewResponse(q.config, protocol.RespOK))
					// close(q.close)
					// close(q.in)
				}
			default:
				cmd.Respond(protocol.NewResponse(q.config, protocol.RespErr))
			}
		case <-q.close:
			return
		}
	}
}

func (q *EventQ) Stop() error {
	select {
	case q.close <- struct{}{}:
	case <-time.After(500 * time.Millisecond):
		log.Printf("event queue failed to stop properly")
	}
	return nil
}

func (q *EventQ) handleMsg(cmd *protocol.Command) {
	// TODO make the messages bytes once and reuse
	var msgs [][]byte
	id := q.currID - 1

	if len(cmd.Args) == 0 {
		cmd.Respond(protocol.NewClientErrResponse(q.config, protocol.ErrRespNoArguments))
		return
	}

	// TODO if any messages are invalid, throw out the whole bunch
	for _, msg := range cmd.Args {
		if len(msg) == 0 {
			cmd.Respond(protocol.NewClientErrResponse(q.config, protocol.ErrRespEmptyMessage))
			return
		}

		id++
		msgb := protocol.NewProtocolWriter().WriteLogLine(protocol.NewMessage(id, msg))
		msgs = append(msgs, msgb)

		q.log.SetID(id)
		_, err := q.log.Write(msgb)
		if err != nil {
			log.Printf("Error: %+v", err)
			cmd.Respond(protocol.NewResponse(q.config, protocol.RespErr))
			return
		}
	}
	q.currID = id + 1

	q.Stats.Incr("total_writes")

	resp := protocol.NewResponse(q.config, protocol.RespOK)
	resp.ID = id
	cmd.Respond(resp)

	q.publishMessages(cmd, msgs)
}

func (q *EventQ) publishMessages(cmd *protocol.Command, msgs [][]byte) {
	internal.Debugf(q.config, "publishing to %d subscribers", len(q.subscriptions))
	for _, sub := range q.subscriptions {
		// go func(sub *Subscription) {
		for i := range msgs {
			sub.send(msgs[i])
		}
		// }(sub)
	}
}

// handleReplicate basically does the same thing as handleRead now.
func (q *EventQ) handleReplicate(cmd *protocol.Command) {
	startID, err := q.parseReplicate(cmd)
	if err != nil {
		internal.Debugf(q.config, "invalid: %v", err)
		cmd.Respond(protocol.NewResponse(q.config, protocol.RespErr))
		return
	}

	q.doRead(cmd, startID, 0)
}

func (q *EventQ) parseReplicate(cmd *protocol.Command) (uint64, error) {
	if len(cmd.Args) != 1 {
		return 0, errInvalidFormat
	}
	return protocol.ParseNumber(cmd.Args[0])
}

// handleRawMsg receives a chunk of data from a master and writes it to the log
func (q *EventQ) handleRawMsg(cmd *protocol.Command) {

	resp := protocol.NewResponse(q.config, protocol.RespOK)
	cmd.Respond(resp)
}

func (q *EventQ) handleRead(cmd *protocol.Command) {
	startID, limit, err := q.parseRead(cmd)
	if err != nil {
		internal.Debugf(q.config, "invalid: %v", err)
		cmd.Respond(protocol.NewClientErrResponse(q.config, protocol.ErrRespInvalid))
		return
	}

	q.Stats.Incr("total_reads")
	q.doRead(cmd, startID, limit)
}

func (q *EventQ) doRead(cmd *protocol.Command, startID uint64, limit uint64) {
	resp := protocol.NewResponse(q.config, protocol.RespOK)
	resp.ReaderC = make(chan io.Reader, 1000)

	cmd.Respond(resp)
	cmd.WaitForReady()

	end := startID + limit
	if limit == 0 {
		head, err := q.log.Head()
		internal.PanicOnError(err)
		end = head
	}

	internal.Debugf(q.config, "adding subscription for %s", cmd.ConnID)
	q.subscriptions[cmd.ConnID] = newSubscription(q.config, resp.ReaderC, cmd.Done)

	iterator, err := q.log.Range(startID, end)
	if err != nil {
		log.Printf("failed to handle read command: %+v", err)
		resp.SendEOF()
		cmd.Finish()
		return
	}

	for iterator.Next() {
		if err := iterator.Error(); err != nil {
			log.Printf("failed to read log range iterator: %+v", err)
			resp.SendEOF()
			cmd.Finish()
		}
		q.sendChunk(iterator.LogFile(), resp.ReaderC)
	}

	if limit != 0 { // not reading forever
		resp.SendEOF()
		cmd.Finish()
	}
}

func (q *EventQ) sendChunk(lf logger.LogReadableFile, readerC chan io.Reader) {
	size, limit := lf.SizeLimit()
	buflen := size
	if limit > 0 {
		buflen = limit
	}
	// buflen does not take seek position into account

	f := lf.AsFile()
	internal.Debugf(q.config, "<-%s: %d bytes", f.Name(), buflen)
	reader := bytes.NewReader([]byte(fmt.Sprintf("+%d\r\n", buflen)))
	readerC <- reader
	readerC <- io.LimitReader(f, buflen)
}

var errInvalidFormat = errors.New("Invalid command format")

func (q *EventQ) parseRead(cmd *protocol.Command) (uint64, uint64, error) {
	if len(cmd.Args) != 2 {
		// cmd.Respond(protocol.NewResponse(respErr))
		return 0, 0, errInvalidFormat
	}

	startID, err := protocol.ParseNumber(cmd.Args[0])
	if err != nil {
		return 0, 0, err
	}

	limit, err := protocol.ParseNumber(cmd.Args[1])
	if err != nil {
		return 0, 0, err
	}
	return startID, limit, nil
}

func (q *EventQ) handleHead(cmd *protocol.Command) {
	if len(cmd.Args) != 0 {
		cmd.Respond(protocol.NewClientErrResponse(q.config, protocol.ErrRespInvalid))
		return
	}

	if id, err := q.log.Head(); err != nil {
		cmd.Respond(protocol.NewResponse(q.config, protocol.RespErr))
	} else {
		resp := protocol.NewResponse(q.config, protocol.RespOK)
		resp.ID = id
		cmd.Respond(resp)
	}
}

func (q *EventQ) handleStats(cmd *protocol.Command) {
	if len(cmd.Args) != 0 {
		cmd.Respond(protocol.NewClientErrResponse(q.config, protocol.ErrRespInvalid))
		return
	}

	resp := protocol.NewResponse(q.config, protocol.RespOK)
	resp.Body = q.Stats.Bytes()

	cmd.Respond(resp)
}

func (q *EventQ) handlePing(cmd *protocol.Command) {
	if len(cmd.Args) != 0 {
		cmd.Respond(protocol.NewClientErrResponse(q.config, protocol.ErrRespInvalid))
		return
	}

	cmd.Respond(protocol.NewResponse(q.config, protocol.RespOK))
}

func (q *EventQ) handleClose(cmd *protocol.Command) {
	if len(cmd.Args) != 0 {
		cmd.Respond(protocol.NewClientErrResponse(q.config, protocol.ErrRespInvalid))
		return
	}

	q.removeSubscription(cmd)
	cmd.Respond(protocol.NewResponse(q.config, protocol.RespOK))
}

func (q *EventQ) removeSubscription(cmd *protocol.Command) {
	if sub, ok := q.subscriptions[cmd.ConnID]; ok {
		sub.finish()
	}
	delete(q.subscriptions, cmd.ConnID)
}

func (q *EventQ) handleSleep(cmd *protocol.Command) {
	if len(cmd.Args) != 1 {
		cmd.Respond(protocol.NewClientErrResponse(q.config, protocol.ErrRespInvalid))
		return
	}

	var msecs int
	_, err := fmt.Sscanf(string(cmd.Args[0]), "%d", &msecs)
	if err != nil {
		cmd.Respond(protocol.NewClientErrResponse(q.config, protocol.ErrRespInvalid))
		return
	}

	select {
	case <-time.After(time.Duration(msecs) * time.Millisecond):
	case <-cmd.Wake:
	}

	cmd.Respond(protocol.NewResponse(q.config, protocol.RespOK))
}

func (q *EventQ) HandleShutdown(cmd *protocol.Command) error {
	// check if shutdown command is allowed and wait to finish any outstanding
	// work here
	if manager, ok := q.log.(logger.LogManager); ok {
		if err := manager.Shutdown(); err != nil {
			return err
		}
	}
	return nil
}

func (q *EventQ) PushCommand(cmd *protocol.Command) (*protocol.Response, error) {
	select {
	case q.in <- cmd:
	}

	select {
	case resp := <-cmd.RespC:
		return resp, nil
	}
}

// func (q *EventQ) handleHup() {
// }

// Subscription is used to tail logs
type Subscription struct {
	config  *config.Config
	readerC chan io.Reader
	done    chan struct{}
}

func newSubscription(config *config.Config, readerC chan io.Reader, done chan struct{}) *Subscription {
	return &Subscription{
		config:  config,
		readerC: readerC,
		done:    done,
	}
}

func (subs *Subscription) send(msg []byte) {
	// fmt.Printf("<-bytes %q (subscription)\n", prettybuf(msg))
	subs.readerC <- bytes.NewReader(msg)
}

func (subs *Subscription) finish() {
	select {
	case subs.done <- struct{}{}:
		internal.Debugf(subs.config, "subscription <-done")
	default:
		internal.Debugf(subs.config, "tried but failed to close subscription")
	}
}
