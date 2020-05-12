package dtls

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-ocf/go-coap/v2/blockwise"
	"github.com/go-ocf/go-coap/v2/message"

	"github.com/go-ocf/go-coap/v2/keepalive"

	"github.com/go-ocf/go-coap/v2/message/codes"
	"github.com/go-ocf/go-coap/v2/udp/client"
	"github.com/go-ocf/go-coap/v2/udp/message/pool"

	coapNet "github.com/go-ocf/go-coap/v2/net"
)

// A ServerOption sets options such as credentials, codec and keepalive parameters, etc.
type ServerOption interface {
	apply(*serverOptions)
}

// The HandlerFunc type is an adapter to allow the use of
// ordinary functions as COAP handlers.  If f is a function
// with the appropriate signature, HandlerFunc(f) is a
// Handler object that calls f.
type HandlerFunc = func(*client.ResponseWriter, *pool.Message)

type ErrorFunc = func(error)

type GoPoolFunc = func(func() error) error

type BlockwiseFactoryFunc = func(getSendedRequest func(token message.Token) (blockwise.Message, bool)) *blockwise.BlockWise

type OnNewClientConnFunc = func(cc *client.ClientConn)

var defaultServerOptions = serverOptions{
	ctx:            context.Background(),
	maxMessageSize: 64 * 1024,
	handler: func(w *client.ResponseWriter, r *pool.Message) {
		w.SetResponse(codes.NotFound, message.TextPlain, nil)
	},
	errors: func(err error) {
		fmt.Println(err)
	},
	goPool: func(f func() error) error {
		go func() {
			err := f()
			if err != nil {
				fmt.Println(err)
			}
		}()
		return nil
	},
	keepalive:                      keepalive.New(),
	blockwiseEnable:                true,
	blockwiseSZX:                   blockwise.SZX1024,
	blockwiseTransferTimeout:       time.Second * 5,
	onNewClientConn:                func(cc *client.ClientConn) {},
	heartBeat:                      time.Millisecond * 100,
	transmissionNStart:             time.Second,
	transmissionAcknowledgeTimeout: time.Second * 2,
	transmissionMaxRetransmit:      4,
}

type serverOptions struct {
	ctx                            context.Context
	maxMessageSize                 int
	handler                        HandlerFunc
	errors                         ErrorFunc
	goPool                         GoPoolFunc
	keepalive                      *keepalive.KeepAlive
	net                            string
	blockwiseSZX                   blockwise.SZX
	blockwiseEnable                bool
	blockwiseTransferTimeout       time.Duration
	onNewClientConn                OnNewClientConnFunc
	heartBeat                      time.Duration
	transmissionNStart             time.Duration
	transmissionAcknowledgeTimeout time.Duration
	transmissionMaxRetransmit      int
}

// Listener defined used by coap
type Listener interface {
	Close() error
	AcceptWithContext(ctx context.Context) (net.Conn, error)
}

type Server struct {
	maxMessageSize                 int
	handler                        HandlerFunc
	errors                         ErrorFunc
	goPool                         GoPoolFunc
	keepalive                      *keepalive.KeepAlive
	blockwiseSZX                   blockwise.SZX
	blockwiseEnable                bool
	blockwiseTransferTimeout       time.Duration
	onNewClientConn                OnNewClientConnFunc
	heartBeat                      time.Duration
	transmissionNStart             time.Duration
	transmissionAcknowledgeTimeout time.Duration
	transmissionMaxRetransmit      int

	ctx    context.Context
	cancel context.CancelFunc

	msgID uint32

	listen      Listener
	listenMutex sync.Mutex
}

func NewServer(opt ...ServerOption) *Server {
	opts := defaultServerOptions
	for _, o := range opt {
		o.apply(&opts)
	}

	ctx, cancel := context.WithCancel(opts.ctx)
	b := make([]byte, 4)
	rand.Read(b)
	msgID := binary.BigEndian.Uint32(b)

	return &Server{
		ctx:                            ctx,
		cancel:                         cancel,
		handler:                        opts.handler,
		maxMessageSize:                 opts.maxMessageSize,
		errors:                         opts.errors,
		goPool:                         opts.goPool,
		keepalive:                      opts.keepalive,
		blockwiseSZX:                   opts.blockwiseSZX,
		blockwiseEnable:                opts.blockwiseEnable,
		blockwiseTransferTimeout:       opts.blockwiseTransferTimeout,
		msgID:                          msgID,
		onNewClientConn:                opts.onNewClientConn,
		heartBeat:                      opts.heartBeat,
		transmissionNStart:             opts.transmissionNStart,
		transmissionAcknowledgeTimeout: opts.transmissionAcknowledgeTimeout,
		transmissionMaxRetransmit:      opts.transmissionMaxRetransmit,
	}
}

func (s *Server) Serve(l Listener) error {
	if s.blockwiseSZX > blockwise.SZX1024 {
		return fmt.Errorf("invalid blockwiseSZX")
	}

	s.listenMutex.Lock()
	if s.listen != nil {
		s.listenMutex.Unlock()
		return fmt.Errorf("server already serve listener")
	}
	s.listen = l
	s.listenMutex.Unlock()
	defer func() {
		s.listenMutex.Lock()
		defer s.listenMutex.Unlock()
		s.listen = nil
	}()

	var wg sync.WaitGroup
	for {
		rw, err := l.AcceptWithContext(s.ctx)
		if err != nil {
			switch err {
			case context.DeadlineExceeded, context.Canceled:
				select {
				case <-s.ctx.Done():
				default:
					s.errors(fmt.Errorf("cannot accept connection: %w", err))
					continue
				}
				wg.Wait()
				return nil
			default:
				continue
			}
		}
		if rw != nil {
			wg.Add(1)
			cc := s.createClientConn(coapNet.NewConn(rw, coapNet.WithHeartBeat(s.heartBeat)))
			if s.onNewClientConn != nil {
				s.onNewClientConn(cc)
			}
			go func() {
				defer wg.Done()
				err := cc.Run()
				if err != nil {
					s.errors(err)
				}
			}()
			if s.keepalive != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := s.keepalive.Run(cc)
					if err != nil {
						s.errors(err)
					}
				}()
			}
		}
	}
}

// Stop stops server without wait of ends Serve function.
func (s *Server) Stop() {
	s.cancel()
}

func (s *Server) createClientConn(connection *coapNet.Conn) *client.ClientConn {
	var blockWise *blockwise.BlockWise
	if s.blockwiseEnable {
		blockWise = blockwise.NewBlockWise(func(ctx context.Context) blockwise.Message {
			return pool.AcquireMessage(ctx)
		}, func(m blockwise.Message) {
			pool.ReleaseMessage(m.(*pool.Message))
		}, s.blockwiseTransferTimeout, s.errors, false, func(token message.Token) (blockwise.Message, bool) {
			return nil, false
		})
	}
	obsHandler := client.NewHandlerContainer()
	session := NewSession(
		s.ctx,
		connection,
		s.maxMessageSize,
	)
	cc := client.NewClientConn(
		session,
		obsHandler,
		nil,
		s.transmissionNStart,
		s.transmissionAcknowledgeTimeout,
		s.transmissionMaxRetransmit,
		client.NewObservationHandler(obsHandler, func(w *client.ResponseWriter, r *pool.Message) {
			s.handler(w, r)
		}),
		s.blockwiseSZX,
		blockWise,
		s.goPool,
	)

	return cc
}

// GetMID generates a message id for UDP-coap
func (s *Server) GetMID() uint16 {
	return uint16(atomic.AddUint32(&s.msgID, 1) % 0xffff)
}