package session

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/linxGnu/gosmpp"
	"github.com/linxGnu/gosmpp/pdu"
	"github.com/phuslu/log"
	"github.com/rixtrayker/demo-smpp/internal/config"
	"github.com/rixtrayker/demo-smpp/internal/dtos"
	"github.com/rixtrayker/demo-smpp/internal/metrics"
	"github.com/rixtrayker/demo-smpp/internal/queue"
	"github.com/rixtrayker/demo-smpp/internal/response"
	"golang.org/x/time/rate"
)

type SessionType string

const (
	Transceiver SessionType = "transceiver"
	Receiver    SessionType = "receiver"
	Transmitter SessionType = "transmitter"
)

type Session struct {
	ctx 			 context.Context
	logger 			 log.Logger
	gateway           string
	sessionType       SessionType
	startTime         time.Time
	limiter           *rate.Limiter
	rateLimit         int
	maxOutstanding    int
	hasOutstanding    bool
	outstandingCh     chan struct{}
	portedStream      *Stream
	resendStream 	  *Stream
	mu                sync.Mutex
	handler           *PDUHandler
	stop              chan struct{}
	concatenated      map[uint8][]string
	messagesStatus    map[int32]*MessageStatus
	maxRetries        int
	responseWriter    *response.Writer
	wg                sync.WaitGroup
	deliveryWg        sync.WaitGroup
	auth              gosmpp.Auth
	enquireLink       time.Duration
	readTimeout       time.Duration
	rebindingInterval time.Duration
	portGateways      []string
	smppSessions      SMPPSessions
    shutdown          CloseSignals
}

type CloseSignals struct {
    streamClose   chan struct{}
    closed        bool
	mu            sync.Mutex
}

type MessageStatus struct {
	startTime       time.Time
	MessageID       string
	SystemMessageID int64
	Sender          string
	Text            string
	Status          string
	Number          string
	GatewayHistory  []string
}

type SMPPSessions struct {
	transceiver *gosmpp.Session
	receiver    *gosmpp.Session
	transmitter *gosmpp.Session
}

type PDUHandler interface {
	HandlePDU(*interface{}) (pdu.PDU, bool)
}

type Option func(*Session)

func WithMaxOutstanding(maxOutstanding int) Option {
	return func(s *Session) {
		s.maxOutstanding = maxOutstanding
	}
}

func WithHasOutstanding(hasOutstanding bool) Option {
	return func(s *Session) {
		s.hasOutstanding = hasOutstanding
	}
}

func WithMaxRetries(maxRetries int) Option {
	return func(s *Session) {
		s.maxRetries = maxRetries
	}
}

func WithResponseWriter(responseWriter *response.Writer) Option {
	return func(s *Session) {
		s.responseWriter = responseWriter
	}
}

func NewSession(cfg config.Provider, h *PDUHandler, options ...Option) (*Session, error) {
	session := &Session{
		logger: 		  log.Logger{
			Level:      log.InfoLevel,
			TimeFormat: "15:04:05",
			Caller:     1,
			Writer: &log.FileWriter{
				Filename:   "logs/smpp/smpp.log",
				MaxBackups: 14,
				LocalTime:  false,
			},
		},
		gateway:           cfg.Name,
		startTime:         time.Now(),
		concatenated:      make(map[uint8][]string),
		handler:           h,
		limiter:           rate.NewLimiter(rate.Limit(cfg.RateLimit), cfg.BurstLimit),
		rateLimit:         cfg.RateLimit,
		maxOutstanding:    cfg.MaxOutStanding,
		hasOutstanding:    cfg.HasOutStanding,
		maxRetries:        cfg.MaxRetries,
		outstandingCh:     make(chan struct{}, cfg.MaxOutStanding),
		stop:              make(chan struct{}),
		messagesStatus:    make(map[int32]*MessageStatus),
		wg:                sync.WaitGroup{},
		deliveryWg:        sync.WaitGroup{},
		sessionType:       SessionType(cfg.SessionType),
		enquireLink:       5 * time.Second,
		readTimeout:       10 * time.Second,
		rebindingInterval: 600 * time.Second,
		portedStream: 		NewStream(50),
		resendStream:       NewStream(50),

		/// To be changed
		shutdown: CloseSignals{
			streamClose: make(chan struct{}),
			closed: false,
			mu: sync.Mutex{},
		},
		portGateways:      []string{"zain", "mobily", "stc"},
		smppSessions:      SMPPSessions{},
	}

	for _, option := range options {
		option(session)
	}

	session.auth = gosmpp.Auth{
		SMSC:       cfg.SMSC,
		SystemID:   cfg.SystemID,
		Password:   cfg.Password,
		SystemType: cfg.SystemType,
	}

	return session, nil
}

func (s *Session) Start(ctx context.Context) error {
	initialDelay := 100 * time.Millisecond
	maxDelay := 10 * time.Second
	factor := 2.0

	s.ctx = ctx
	for retries := 0; retries <= s.maxRetries; retries++ {
		select {
		case <-ctx.Done():
			return errors.New("session creation stopped")
		default:
			if err := s.connectSessions(); err != nil {
				delay := calculateBackoff(initialDelay, maxDelay, factor, retries)
				s.logger.Error().Err(err).Str("provider", s.gateway).Msg("Failed to create session")
				s.logger.Info().Msgf("Retrying in (Backoff) : %v", delay)
				time.Sleep(delay)
			} else {
				return nil
			}
		}
	}

	return errors.New("failed to create session after maximum retries")
}

func (s *Session) connectSessions() error {
	var err error
	switch s.sessionType {
	case Transceiver:
		s.smppSessions.transceiver, err = gosmpp.NewSession(
			gosmpp.TRXConnector(gosmpp.NonTLSDialer, s.auth),
			s.getSettings(),
			s.rebindingInterval,
		)
		if err != nil {
			return err
		}
	default:
		s.smppSessions.receiver, err = gosmpp.NewSession(
			gosmpp.RXConnector(gosmpp.NonTLSDialer, s.auth),
			s.getSettings(),
			s.rebindingInterval,
		)
		if err != nil {
			return err
		}

		s.smppSessions.transmitter, err = gosmpp.NewSession(
			gosmpp.TXConnector(gosmpp.NonTLSDialer, s.auth),
			s.getSettings(),
			s.rebindingInterval,
		)
		if err != nil {
			return err
		}
	}

	metrics.ActiveSessions.Inc()
	return nil
}

func (s *Session) getSettings() gosmpp.Settings {
	return gosmpp.Settings{
		EnquireLink:      s.enquireLink,
		ReadTimeout:      s.readTimeout,
		OnAllPDU:         handlePDU(s),
		OnSubmitError:    s.handleSubmitError,
		OnReceivingError: s.handleReceivingError,
		OnRebindingError: s.handleRebindingError,
		OnClosed:         s.handleClosed,
	}
}

func (s *Session) handleSubmitError(p pdu.PDU, err error) {
	s.logger.Error().Err(err).Msg("SubmitPDU error")
}

func (s *Session) handleReceivingError(err error) {
	s.logger.Error().Err(err).Msg("Receiving PDU/Network error")
}

func (s *Session) handleRebindingError(err error) {
	metrics.SessionDuration.Observe(time.Since(s.startTime).Seconds())
	s.logger.Error().Err(err).Msg("Rebinding error")
}

func (s *Session) handleClosed(state gosmpp.State) {
	metrics.ActiveSessions.Dec()
	s.logger.Info().Msg("Session closed")	
}

func handlePDU(s *Session) func(pdu.PDU) (pdu.PDU, bool) {
	return func(p pdu.PDU) (pdu.PDU, bool) {
		switch pd := p.(type) {
		case *pdu.BindResp:

			// Handle BindResp if needed
		case *pdu.Unbind:
			s.logger.Info().Msg("Unbind Received")
			metrics.ActiveSessions.Dec()
			return pd.GetResponse(), true
		case *pdu.UnbindResp:
			metrics.SessionDuration.Observe(time.Since(s.startTime).Seconds())
			// Handle UnbindResp if needed
		case *pdu.SubmitSMResp:
			s.handleSubmitSMResp(pd)
			return pd.GetResponse(), false
		case *pdu.GenericNack:
			// Handle GenericNack if needed
		case *pdu.EnquireLinkResp:
			// Handle EnquireLinkResp if needed
		case *pdu.EnquireLink:
			return pd.GetResponse(), false
		case *pdu.DataSM:
			return pd.GetResponse(), false
		case *pdu.DeliverSM:
			s.deliveryWg.Add(1)
			s.HandleDeliverSM(pd)
			return pd.GetResponse(), false
		}
		return nil, false
	}
}

func (s *Session) Write(log *dtos.ReceiveLog) {
	if s.responseWriter != nil {
		(*s.responseWriter).WriteResponse(log)
	}
}

type Handler struct {
}

func (h *Handler) HandlePDU(pd *interface{}) (pdu.PDU, bool) {
    return (*pd).(pdu.PDU).GetResponse(), true
}

func (s *Session) StreamResend() <-chan queue.MessageData {
	return s.resendStream.stream
}

func (s *Session) Stop() {
	<-s.shutdown.streamClose
	
	s.wg.Wait()

	if s.smppSessions.transceiver != nil {
		s.smppSessions.transceiver.Close()
	}
	if s.smppSessions.receiver != nil {
		s.smppSessions.receiver.Close()
	}
	if s.smppSessions.transmitter != nil {
		s.smppSessions.transmitter.Close()
	}

	time.Sleep(1 * time.Second)

	// close(s.portedChannel)
	s.portedStream.Close()
	s.resendStream.Close()
	
    s.shutdown.mu.Lock()
	if !s.shutdown.closed {
		
		s.shutdown.closed = true
	}
	s.shutdown.mu.Unlock()

	s.deliveryWg.Wait()

	metrics.SessionDuration.Observe(time.Since(s.startTime).Seconds())

	if s.responseWriter != nil {
		time.Sleep(1 * time.Second)
		s.responseWriter.Close()
	}
}