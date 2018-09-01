package events

import (
	"context"
	"log"
	"sync"

	"github.com/jeffrom/logd/config"
	"github.com/jeffrom/logd/internal"
	"github.com/jeffrom/logd/protocol"
	"github.com/jeffrom/logd/server"
	"github.com/jeffrom/logd/transport"
)

var blockingReqs = map[protocol.CmdType]bool{
	protocol.CmdBatch: true,
	protocol.CmdRead:  true,
	protocol.CmdTail:  true,
}

// Handlers is a map of event queues, one for each topic as well as one for
// non-blocking requests.
type Handlers struct {
	conf      *config.Config
	h         map[string]*eventQ
	asyncQ    *eventQ
	topics    *topics
	servers   []transport.Server
	shutdownC chan error
	mu        sync.Mutex
}

// NewHandlers returns a new instance of *Handlers.
func NewHandlers(conf *config.Config) *Handlers {
	log.Printf("starting options: %+v", conf)

	h := &Handlers{
		conf:      conf,
		h:         make(map[string]*eventQ),
		asyncQ:    newEventQ(conf),
		topics:    newTopics(conf),
		servers:   []transport.Server{},
		shutdownC: make(chan error, 1),
	}

	if conf.Hostport != "" {
		h.Register(server.NewSocket(conf.Hostport, conf))
	}

	return h
}

// Register adds a server to the event queue. The queue should be stopped when
// Register is called.
func (h *Handlers) Register(server transport.Server) {
	server.SetQPusher(h)
	h.servers = append(h.servers, server)
}

// GoStart begins handling messages
func (h *Handlers) GoStart() error {
	h.drainShutdownC()
	if err := h.topics.Setup(); err != nil {
		return err
	}

	if err := h.asyncQ.GoStart(); err != nil {
		return err
	}

	for name, topic := range h.topics.m {
		q := newEventQ(h.conf)
		q.setTopic(topic)
		if err := q.GoStart(); err != nil {
			return err
		}
		h.h[name] = q
	}

	for _, server := range h.servers {
		server.GoServe()
	}
	return nil
}

func (h *Handlers) drainShutdownC() {
	for {
		select {
		case <-h.shutdownC:
		default:
			return
		}
	}
}

func (h *Handlers) Start() error {
	if err := h.GoStart(); err != nil {
		return err
	}

	select {
	case err := <-h.shutdownC:
		if err != nil {
			return err
		}
	}
	return nil
}

// PushRequest implements transport.RequestHandler.
func (h *Handlers) PushRequest(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	if ok, _ := blockingReqs[req.Name]; ok {
		return h.pushBlockingRequest(ctx, req)
	} else {
		return h.asyncQ.PushRequest(ctx, req)
	}
	return nil, nil
}

func (h *Handlers) pushBlockingRequest(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	name := req.Topic()
	if name == "" {
		return h.asyncQ.PushRequest(ctx, req)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if q, ok := h.h[name]; ok {
		return q.PushRequest(ctx, req)
	}

	if req.Name == protocol.CmdBatch {
		q := newEventQ(h.conf)
		topic, err := h.topics.add(name)
		if err != nil {
			return nil, err
		}
		q.setTopic(topic)
		if err := q.GoStart(); err != nil {
			return nil, err
		}
		h.h[name] = q
		return q.PushRequest(ctx, req)
	}
	return h.asyncQ.PushRequest(ctx, req)
}

func (h *Handlers) Stop() error {
	defer func() {
		h.shutdownC <- nil
	}()
	internal.Debugf(h.conf, "shutting down")
	var firstErr error

	for _, server := range h.servers {
		if serr := internal.LogAndReturnError(server.Stop()); serr != nil {
			if firstErr == nil {
				firstErr = serr
			}
			log.Printf("shutdown error: %+v", serr)
		}
	}

	if err := internal.LogAndReturnError(h.asyncQ.Stop()); err != nil {
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, q := range h.h {
		if err := internal.LogAndReturnError(q.Stop()); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	if err := internal.LogAndReturnError(h.topics.Shutdown()); err != nil {
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}