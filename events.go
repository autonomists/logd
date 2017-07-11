package logd

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// this file contains the core logic of the program. Commands come from the
// various inputs. They are handled and a response is given. For example, a
// message is received, it is written to a backend, and a log id is returned to
// the caller. Or, a tail command is received, and the caller receives a log
// stream.

// TODO use an array of send close <- struct{}{} functions to run on shutdown
// instead of doing each one manually

// eventQ manages the receiving, processing, and responding to events.
type eventQ struct {
	config        *Config
	currID        uint64
	in            chan *Command
	close         chan struct{}
	subscriptions map[chan *Response]*Subscription
	log           Logger
	client        *Client
}

func newEventQ(config *Config) *eventQ {
	if config.Logger == nil {
		config.Logger = newFileLogger(config)
	}

	q := &eventQ{
		config:        config,
		in:            make(chan *Command, 0),
		close:         make(chan struct{}),
		subscriptions: make(map[chan *Response]*Subscription),
		log:           config.Logger,
	}

	q.handleSignals()

	if manager, ok := q.log.(logManager); ok {
		if err := manager.Setup(); err != nil {
			panic(err)
		}
	}

	return q
}

func (q *eventQ) start() error {
	head, err := q.log.Head()
	if err != nil {
		return err
	}
	q.currID = head + 1

	log.Printf("Starting at log id %d", q.currID)

	go q.loop()
	return nil
}

func (q *eventQ) loop() {
	for {
		select {
		case cmd := <-q.in:
			debugf(q.config, "event: %s", cmd)

			switch cmd.name {
			case CmdMessage:
				q.handleMsg(cmd)
			case CmdReplicate:
				q.handleReplicate(cmd)
			case CmdRawMessage:
				q.handleRawMsg(cmd)
			case CmdRead:
				q.handleRead(cmd)
			case CmdHead:
				q.handleHead(cmd)
			case CmdPing:
				q.handlePing(cmd)
			case CmdClose:
				q.handleClose(cmd)
			case CmdSleep:
				q.handleSleep(cmd)
			case CmdShutdown:
				if err := q.handleShutdown(cmd); err != nil {
					cmd.respC <- newResponse(RespErr)
				} else {
					cmd.respC <- newResponse(RespOK)
					close(q.close)
					close(q.in)
				}
				return
			default:
				cmd.respC <- newResponse(RespErr)
			}
		case <-q.close:
			return
		}
	}
}

func (q *eventQ) stop() error {
	select {
	case q.close <- struct{}{}:
	case <-time.After(500 * time.Millisecond):
		log.Printf("event queue failed to stop properly")
	}
	return nil
}

func (q *eventQ) handleMsg(cmd *Command) {
	// TODO make the messages bytes once and reuse
	var msgs [][]byte
	id := q.currID - 1

	if len(cmd.args) == 0 {
		cmd.respond(NewClientErrResponse(errRespNoArguments))
		return
	}

	// TODO if any messages are invalid, throw out the whole bunch
	for _, msg := range cmd.args {
		if len(msg) == 0 {
			cmd.respond(NewClientErrResponse(errRespEmptyMessage))
			return
		}

		id++
		msgb := NewMessage(id, msg).bytes()
		msgs = append(msgs, msgb)
		_, err := q.log.Write(msgb)
		if err != nil {
			log.Printf("Error: %+v", err)
			cmd.respond(newResponse(RespErr))
			return
		}
	}
	q.currID = id + 1

	resp := newResponse(RespOK)
	resp.ID = id
	cmd.respond(resp)

	for _, sub := range q.subscriptions {
		go func(sub *Subscription) {
			for i := range cmd.args {
				sub.send(msgs[i])
			}
		}(sub)
	}
}

// handleReplicate basically does the same thing as handleRead now.
func (q *eventQ) handleReplicate(cmd *Command) {
	startID, err := q.parseReplicate(cmd)
	if err != nil {
		debugf(q.config, "invalid: %v", err)
		cmd.respond(newResponse(RespErr))
		return
	}

	q.doRead(cmd, startID, 0)
}

func (q *eventQ) parseReplicate(cmd *Command) (uint64, error) {
	if len(cmd.args) != 1 {
		return 0, errInvalidFormat
	}
	return parseNumber(cmd.args[0])
}

// handleRawMsg receives a chunk of data from a master and writes it to the log
func (q *eventQ) handleRawMsg(cmd *Command) {

	resp := newResponse(RespOK)
	cmd.respond(resp)
}

func (q *eventQ) handleRead(cmd *Command) {
	startID, limit, err := q.parseRead(cmd)
	if err != nil {
		debugf(q.config, "invalid: %v", err)
		cmd.respond(NewClientErrResponse(errRespInvalid))
		return
	}

	q.doRead(cmd, startID, limit)
}

func (q *eventQ) doRead(cmd *Command, startID uint64, limit uint64) {
	resp := newResponse(RespOK)
	resp.msgC = make(chan []byte)
	cmd.respond(resp)

	if err := q.log.SeekToID(startID); err != nil {
		panic(err)
	}

	// b := make([]byte, q.config.MaxChunkSize)
	var b []byte
	numMsg := 0
	scanner := newLogScanner(q.config, q.log)
	for scanner.Scan() {
		msg := scanner.Msg()
		b = append(b, msg.bytes()...)

		numMsg++
		if limit > 0 && uint64(numMsg) >= limit {
			break
		}
	}
	if err := scanner.Error(); err != nil {
		panic(err)
	}

	if len(b) > 0 {
		resp.msgC <- b
	}

	if limit == 0 { // read forever
		q.subscriptions[cmd.respC] = newSubscription(resp.msgC, cmd.done)
	} else {
		cmd.finish()
	}
}

var errInvalidFormat = errors.New("Invalid command format")

func (q *eventQ) parseRead(cmd *Command) (uint64, uint64, error) {
	if len(cmd.args) != 2 {
		// cmd.respond(newResponse(respErr))
		return 0, 0, errInvalidFormat
	}

	startID, err := parseNumber(cmd.args[0])
	if err != nil {
		return 0, 0, err
	}

	limit, err := parseNumber(cmd.args[1])
	if err != nil {
		return 0, 0, err
	}
	return startID, limit, nil
}

func (q *eventQ) handleHead(cmd *Command) {
	if len(cmd.args) != 0 {
		cmd.respond(NewClientErrResponse(errRespInvalid))
		return
	}

	if id, err := q.log.Head(); err != nil {
		cmd.respond(newResponse(RespErr))
	} else {
		resp := newResponse(RespOK)
		resp.ID = id
		cmd.respond(resp)
	}
}

func (q *eventQ) handlePing(cmd *Command) {
	if len(cmd.args) != 0 {
		cmd.respond(NewClientErrResponse(errRespInvalid))
		return
	}

	cmd.respond(newResponse(RespOK))
}

func (q *eventQ) handleClose(cmd *Command) {
	if len(cmd.args) != 0 {
		cmd.respond(NewClientErrResponse(errRespInvalid))
		return
	}

	if sub, ok := q.subscriptions[cmd.respC]; ok {
		sub.finish()
	}

	delete(q.subscriptions, cmd.respC)
	cmd.respond(newResponse(RespOK))
	// cmd.finish()
}

func (q *eventQ) handleSleep(cmd *Command) {
	if len(cmd.args) != 1 {
		cmd.respond(NewClientErrResponse(errRespInvalid))
		return
	}

	var msecs int
	_, err := fmt.Sscanf(string(cmd.args[0]), "%d", &msecs)
	if err != nil {
		cmd.respond(NewClientErrResponse(errRespInvalid))
		return
	}

	select {
	case <-time.After(time.Duration(msecs) * time.Millisecond):
	case <-cmd.wake:
	}

	cmd.respond(newResponse(RespOK))
}

func (q *eventQ) handleShutdown(cmd *Command) error {
	// check if shutdown command is allowed and wait to finish any outstanding
	// work here
	if manager, ok := q.log.(logManager); ok {
		if err := manager.Shutdown(); err != nil {
			return err
		}
	}
	return nil
}

func (q *eventQ) pushCommand(cmd *Command) (*Response, error) {
	q.in <- cmd
	resp := <-cmd.respC
	return resp, nil
}

func (q *eventQ) handleSignals() {
	go q.handleKill()
}

func (q *eventQ) handleKill() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	for range c {
		log.Print("Caught signal. Exiting...")
		q.handleShutdown(nil)
		os.Exit(0)
	}
}

// func (q *eventQ) handleHup() {
// }
