package binance

import (
	"context"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/c9s/bbgo/pkg/depth"
	"github.com/c9s/bbgo/pkg/util"

	"github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"

	"github.com/gorilla/websocket"

	"github.com/c9s/bbgo/pkg/types"
)

var debugBinanceDepth bool

var defaultDialer = &websocket.Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: 10 * time.Second,
	ReadBufferSize:   4096,
}

// from Binance document:
// The websocket server will send a ping frame every 3 minutes.
// If the websocket server does not receive a pong frame back from the connection within a 10 minute period, the connection will be disconnected.
// Unsolicited pong frames are allowed.

// WebSocket connections have a limit of 5 incoming messages per second. A message is considered:
// A PING frame
// A PONG frame
// A JSON controlled message (e.g. subscribe, unsubscribe)

// The connect() method dials and creates the connection object, then it starts 2 go-routine, 1 for reading message, 2 for writing ping messages.
// The re-connector uses the ReconnectC signal channel to start a new websocket connection.
// When ReconnectC is triggered
//   - The context created for the connection must be canceled
//   - The read goroutine must close the connection and exit
//   - The ping goroutine must stop the ticker and exit
//   - the re-connector calls connect() to create the new connection object, go to the 1 step.
// When stream.Close() is called, a close message must be written to the websocket connection.

const readTimeout = 3 * time.Minute
const writeTimeout = 10 * time.Second
const listenKeyKeepAliveInterval = 10 * time.Minute

func init() {

	debugBinanceDepth, _ = strconv.ParseBool(os.Getenv("DEBUG_BINANCE_DEPTH"))
	if debugBinanceDepth {
		log.Info("binance depth debugging is enabled")
	}
}

type StreamRequest struct {
	// request ID is required
	ID     int      `json:"id"`
	Method string   `json:"method"`
	Params []string `json:"params"`
}

//go:generate callbackgen -type Stream -interface
type Stream struct {
	types.MarginSettings
	types.FuturesSettings
	types.StandardStream

	Client        *binance.Client
	futuresClient *futures.Client

	Conn     *websocket.Conn
	ConnLock sync.Mutex

	connCtx    context.Context
	connCancel context.CancelFunc

	// custom callbacks
	depthEventCallbacks       []func(e *DepthEvent)
	kLineEventCallbacks       []func(e *KLineEvent)
	kLineClosedEventCallbacks []func(e *KLineEvent)

	markPriceUpdateEventCallbacks []func(e *MarkPriceUpdateEvent)

	continuousKLineEventCallbacks       []func(e *ContinuousKLineEvent)
	continuousKLineClosedEventCallbacks []func(e *ContinuousKLineEvent)

	balanceUpdateEventCallbacks           []func(event *BalanceUpdateEvent)
	outboundAccountInfoEventCallbacks     []func(event *OutboundAccountInfoEvent)
	outboundAccountPositionEventCallbacks []func(event *OutboundAccountPositionEvent)
	executionReportEventCallbacks         []func(event *ExecutionReportEvent)
	bookTickerEventCallbacks              []func(event *BookTickerEvent)

	orderTradeUpdateEventCallbacks []func(e *OrderTradeUpdateEvent)

	depthBuffers map[string]*depth.Buffer
}

func NewStream(ex *Exchange, client *binance.Client, futuresClient *futures.Client) *Stream {
	stream := &Stream{
		StandardStream: types.StandardStream{
			ReconnectC: make(chan struct{}, 1),
			CloseC: make(chan struct{}),
		},
		Client:        client,
		futuresClient: futuresClient,
		depthBuffers:  make(map[string]*depth.Buffer),
	}

	stream.OnDepthEvent(func(e *DepthEvent) {
		if debugBinanceDepth {
			log.Infof("received %s depth event updateID %d ~ %d (len %d)", e.Symbol, e.FirstUpdateID, e.FinalUpdateID, e.FinalUpdateID-e.FirstUpdateID)
		}

		f, ok := stream.depthBuffers[e.Symbol]
		if ok {
			f.AddUpdate(types.SliceOrderBook{
				Symbol: e.Symbol,
				Bids:   e.Bids,
				Asks:   e.Asks,
			}, e.FirstUpdateID, e.FinalUpdateID)
		} else {
			f = depth.NewBuffer(func() (types.SliceOrderBook, int64, error) {
				return ex.QueryDepth(context.Background(), e.Symbol)
			})
			stream.depthBuffers[e.Symbol] = f

			f.SetBufferingPeriod(time.Second)
			f.OnReady(func(snapshot types.SliceOrderBook, updates []depth.Update) {
				if valid, err := snapshot.IsValid(); !valid {
					log.Errorf("depth snapshot is invalid, error: %v", err)
					return
				}

				stream.EmitBookSnapshot(snapshot)
				for _, u := range updates {
					stream.EmitBookUpdate(u.Object)
				}
			})
			f.OnPush(func(update depth.Update) {
				stream.EmitBookUpdate(update.Object)
			})
		}
	})

	stream.OnOutboundAccountPositionEvent(stream.handleOutboundAccountPositionEvent)
	stream.OnKLineEvent(stream.handleKLineEvent)
	stream.OnBookTickerEvent(stream.handleBookTickerEvent)
	stream.OnExecutionReportEvent(stream.handleExecutionReportEvent)
	stream.OnContinuousKLineEvent(stream.handleContinuousKLineEvent)

	stream.OnOrderTradeUpdateEvent(func(e *OrderTradeUpdateEvent) {
		switch e.OrderTrade.CurrentExecutionType {

		case "NEW", "CANCELED", "EXPIRED":
			order, err := e.OrderFutures()
			if err != nil {
				log.WithError(err).Error("order convert error")
				return
			}

			stream.EmitOrderUpdate(*order)

		case "TRADE":
			// TODO

			// trade, err := e.Trade()
			// if err != nil {
			// 	log.WithError(err).Error("trade convert error")
			// 	return
			// }

			// stream.EmitTradeUpdate(*trade)

			// order, err := e.OrderFutures()
			// if err != nil {
			// 	log.WithError(err).Error("order convert error")
			// 	return
			// }

			// Update Order with FILLED event
			// if order.Status == types.OrderStatusFilled {
			// 	stream.EmitOrderUpdate(*order)
			// }
		case "CALCULATED - Liquidation Execution":
			log.Infof("CALCULATED - Liquidation Execution not support yet.")
		}
	})

	stream.OnDisconnect(func() {
		log.Infof("resetting depth snapshots...")
		for _, f := range stream.depthBuffers {
			f.Reset()
		}
	})

	stream.OnConnect(func() {
		var params []string
		for _, subscription := range stream.Subscriptions {
			params = append(params, convertSubscription(subscription))
		}

		if len(params) == 0 {
			return
		}

		log.Infof("subscribing channels: %+v", params)
		err := stream.Conn.WriteJSON(StreamRequest{
			Method: "SUBSCRIBE",
			Params: params,
			ID:     1,
		})

		if err != nil {
			log.WithError(err).Error("subscribe error")
		}
	})

	return stream
}

func (s *Stream) handleContinuousKLineEvent(e *ContinuousKLineEvent) {
	kline := e.KLine.KLine()
	if e.KLine.Closed {
		s.EmitContinuousKLineClosedEvent(e)
		s.EmitKLineClosed(kline)
	} else {
		s.EmitKLine(kline)
	}
}

func (s *Stream) handleExecutionReportEvent(e *ExecutionReportEvent) {
	switch e.CurrentExecutionType {

	case "NEW", "CANCELED", "REJECTED", "EXPIRED", "REPLACED":
		order, err := e.Order()
		if err != nil {
			log.WithError(err).Error("order convert error")
			return
		}

		s.EmitOrderUpdate(*order)

	case "TRADE":
		trade, err := e.Trade()
		if err != nil {
			log.WithError(err).Error("trade convert error")
			return
		}

		s.EmitTradeUpdate(*trade)

		order, err := e.Order()
		if err != nil {
			log.WithError(err).Error("order convert error")
			return
		}

		// Update Order with FILLED event
		if order.Status == types.OrderStatusFilled {
			s.EmitOrderUpdate(*order)
		}
	}
}

func (s *Stream) handleBookTickerEvent(e *BookTickerEvent) {
	s.EmitBookTickerUpdate(e.BookTicker())
}

func (s *Stream) handleKLineEvent(e *KLineEvent) {
	kline := e.KLine.KLine()
	if e.KLine.Closed {
		s.EmitKLineClosedEvent(e)
		s.EmitKLineClosed(kline)
	} else {
		s.EmitKLine(kline)
	}
}

func (s *Stream) handleOutboundAccountPositionEvent(e *OutboundAccountPositionEvent) {
	snapshot := types.BalanceMap{}
	for _, balance := range e.Balances {
		snapshot[balance.Asset] = types.Balance{
			Currency:  balance.Asset,
			Available: balance.Free,
			Locked:    balance.Locked,
		}
	}
	s.EmitBalanceSnapshot(snapshot)
}

func (s *Stream) dial(listenKey string) (*websocket.Conn, error) {
	var url string
	if s.PublicOnly {
		if s.IsFutures {
			url = "wss://fstream.binance.com/ws/"
		} else {
			url = "wss://stream.binance.com:9443/ws"
		}
	} else {
		if s.IsFutures {
			url = "wss://fstream.binance.com/ws/" + listenKey
		} else {
			url = "wss://stream.binance.com:9443/ws/" + listenKey
		}
	}

	conn, _, err := defaultDialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}

	// use the default ping handler
	// The websocket server will send a ping frame every 3 minutes.
	// If the websocket server does not receive a pong frame back from the connection within a 10 minutes period,
	// the connection will be disconnected.
	// Unsolicited pong frames are allowed.
	conn.SetPingHandler(nil)
	conn.SetPongHandler(func(string) error {
		log.Debugf("websocket <- received pong")
		if err := conn.SetReadDeadline(time.Now().Add(readTimeout * 2)); err != nil {
			log.WithError(err).Error("pong handler can not set read deadline")
		}
		return nil
	})

	return conn, nil
}

// Connect starts the stream and create the websocket connection
func (s *Stream) Connect(ctx context.Context) error {
	err := s.connect(ctx)
	if err != nil {
		return err
	}

	// start one re-connector goroutine with the base context
	go s.reconnector(ctx)

	s.EmitStart()
	return nil
}

func (s *Stream) reconnector(ctx context.Context) {
	for {
		select {

		case <-ctx.Done():
			return

		case <-s.CloseC:
			return

		case <-s.ReconnectC:
			log.Warnf("received reconnect signal")
			time.Sleep(3 * time.Second)

			log.Warnf("re-connecting...")
			if err := s.connect(ctx); err != nil {
				log.WithError(err).Errorf("connect error, try to reconnect again...")
				s.Reconnect()
			}
		}
	}
}

func (s *Stream) connect(ctx context.Context) error {
	var err error
	var listenKey string
	if s.PublicOnly {
		log.Infof("stream is set to public only mode")
	} else {

		listenKey, err = s.fetchListenKey(ctx)
		if err != nil {
			return err
		}

		log.Infof("listen key is created: %s", util.MaskKey(listenKey))
	}

	// when in public mode, the listen key is an empty string
	conn, err := s.dial(listenKey)
	if err != nil {
		return err
	}

	log.Infof("websocket connected")

	// should only start one connection one time, so we lock the mutex
	s.ConnLock.Lock()

	// ensure the previous context is cancelled
	if s.connCancel != nil {
		s.connCancel()
	}

	// create a new context for this connection
	s.connCtx, s.connCancel = context.WithCancel(ctx)
	s.Conn = conn
	s.ConnLock.Unlock()

	s.EmitConnect()

	if !s.PublicOnly {
		go s.listenKeyKeepAlive(s.connCtx, listenKey)
	}

	go s.read(s.connCtx, conn)
	go s.ping(s.connCtx, conn, readTimeout / 3)
	return nil
}

func (s *Stream) ping(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	defer log.Debug("ping worker stopped")

	var pingTicker = time.NewTicker(interval)
	defer pingTicker.Stop()

	for {
		select {

		case <-ctx.Done():
			return

		case <-s.CloseC:
			return

		case <-pingTicker.C:
			log.Debugf("websocket -> ping")
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				log.WithError(err).Error("ping error", err)
				s.Reconnect()
			}
		}
	}
}

// From Binance
// Keepalive a user data stream to prevent a time out. User data streams will close after 60 minutes.
// It's recommended to send a ping about every 30 minutes.
func (s *Stream) listenKeyKeepAlive(ctx context.Context, listenKey string) {
	keepAliveTicker := time.NewTicker(listenKeyKeepAliveInterval)
	defer keepAliveTicker.Stop()

	// if we exit, we should invalidate the existing listen key
	defer func() {
		log.Debugf("keepalive worker stopped")
		if err := s.invalidateListenKey(context.Background(), listenKey); err != nil {
			log.WithError(err).Errorf("invalidate listen key error: %v key: %s", err, util.MaskKey(listenKey))
		}
	}()

	for {
		select {

		case <-ctx.Done():
			return

		case <-keepAliveTicker.C:
			for i := 0; i < 5; i++ {
				err := s.keepaliveListenKey(ctx, listenKey)
				if err == nil {
					break
				} else {
					switch err.(type) {
					case net.Error:
						log.WithError(err).Errorf("listen key keep-alive network error: %v key: %s", err, util.MaskKey(listenKey))
						time.Sleep(5 * time.Second)
						continue

					default:
						log.WithError(err).Errorf("listen key keep-alive unexpected error: %v key: %s", err, util.MaskKey(listenKey))
						s.Reconnect()
						return

					}
				}
			}

		}
	}
}

func (s *Stream) read(ctx context.Context, conn *websocket.Conn) {
	defer func() {
		// if we failed to read, we need to cancel the context
		if s.connCancel != nil {
			s.connCancel()
		}
		_ = conn.Close()
		s.EmitDisconnect()
	}()

	for {
		select {

		case <-ctx.Done():
			return

		case <-s.CloseC:
			return

		default:
			if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
				log.WithError(err).Errorf("set read deadline error: %s", err.Error())
			}

			mt, message, err := conn.ReadMessage()
			if err != nil {
				// if it's a network timeout error, we should re-connect
				switch err := err.(type) {

				// if it's a websocket related error
				case *websocket.CloseError:
					if err.Code == websocket.CloseNormalClosure {
						return
					}

					log.WithError(err).Errorf("websocket error abnormal close: %+v", err)

					_ = conn.Close()
					// for unexpected close error, we should re-connect
					// emit reconnect to start a new connection
					s.Reconnect()
					return

				case net.Error:
					log.WithError(err).Error("websocket read network error")
					_ = conn.Close()
					s.Reconnect()
					return

				default:
					log.WithError(err).Error("unexpected websocket error")
					_ = conn.Close()
					s.Reconnect()
					return
				}
			}

			// skip non-text messages
			if mt != websocket.TextMessage {
				continue
			}

			log.Debug(string(message))

			e, err := ParseEvent(string(message))
			if err != nil {
				log.WithError(err).Errorf("websocket event parse error")
				continue
			}

			switch e := e.(type) {

			case *OutboundAccountPositionEvent:
				s.EmitOutboundAccountPositionEvent(e)

			case *OutboundAccountInfoEvent:
				s.EmitOutboundAccountInfoEvent(e)

			case *BalanceUpdateEvent:
				s.EmitBalanceUpdateEvent(e)

			case *KLineEvent:
				s.EmitKLineEvent(e)

			case *BookTickerEvent:
				s.EmitBookTickerEvent(e)

			case *DepthEvent:
				s.EmitDepthEvent(e)

			case *ExecutionReportEvent:
				s.EmitExecutionReportEvent(e)

			case *MarkPriceUpdateEvent:
				s.EmitMarkPriceUpdateEvent(e)

			case *ContinuousKLineEvent:
				s.EmitContinuousKLineEvent(e)

			case *OrderTradeUpdateEvent:
				s.EmitOrderTradeUpdateEvent(e)
			}
		}
	}
}

func (s *Stream) Close() error {
	log.Infof("closing stream...")

	// close the close signal channel
	close(s.CloseC)

	// get the connection object before call the context cancel function
	s.ConnLock.Lock()
	conn := s.Conn
	s.ConnLock.Unlock()

	// cancel the context so that the ticker loop and listen key updater will be stopped.
	if s.connCancel != nil {
		s.connCancel()
	}

	// gracefully write the close message to the connection
	err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	if err != nil {
		return errors.Wrap(err, "websocket write close message error")
	}

	err = s.Conn.Close()
	return errors.Wrap(err, "websocket connection close error")
}

func (s *Stream) fetchListenKey(ctx context.Context) (string, error) {
	if s.IsMargin {
		if s.IsIsolatedMargin {
			log.Debugf("isolated margin %s is enabled, requesting margin user stream listen key...", s.IsolatedMarginSymbol)
			req := s.Client.NewStartIsolatedMarginUserStreamService()
			req.Symbol(s.IsolatedMarginSymbol)
			return req.Do(ctx)
		}

		log.Debugf("margin mode is enabled, requesting margin user stream listen key...")
		req := s.Client.NewStartMarginUserStreamService()
		return req.Do(ctx)
	} else if s.IsFutures {
		log.Debugf("futures mode is enabled, requesting futures user stream listen key...")
		req := s.futuresClient.NewStartUserStreamService()
		return req.Do(ctx)
	}

	log.Debugf("spot mode is enabled, requesting user stream listen key...")
	return s.Client.NewStartUserStreamService().Do(ctx)
}

func (s *Stream) keepaliveListenKey(ctx context.Context, listenKey string) error {
	log.Debugf("keepalive listen key: %s", util.MaskKey(listenKey))
	if s.IsMargin {
		if s.IsIsolatedMargin {
			req := s.Client.NewKeepaliveIsolatedMarginUserStreamService().ListenKey(listenKey)
			req.Symbol(s.IsolatedMarginSymbol)
			return req.Do(ctx)
		}
		req := s.Client.NewKeepaliveMarginUserStreamService().ListenKey(listenKey)
		return req.Do(ctx)
	} else if s.IsFutures {
		req := s.futuresClient.NewKeepaliveUserStreamService().ListenKey(listenKey)
		return req.Do(ctx)
	}

	return s.Client.NewKeepaliveUserStreamService().ListenKey(listenKey).Do(ctx)
}

func (s *Stream) invalidateListenKey(ctx context.Context, listenKey string) (err error) {
	// should use background context to invalidate the user stream
	log.Debugf("closing listen key: %s", util.MaskKey(listenKey))

	if s.IsMargin {
		if s.IsIsolatedMargin {
			req := s.Client.NewCloseIsolatedMarginUserStreamService().ListenKey(listenKey)
			req.Symbol(s.IsolatedMarginSymbol)
			err = req.Do(ctx)
		} else {
			req := s.Client.NewCloseMarginUserStreamService().ListenKey(listenKey)
			err = req.Do(ctx)
		}

	} else if s.IsFutures {
		req := s.futuresClient.NewCloseUserStreamService().ListenKey(listenKey)
		err = req.Do(ctx)
	} else {
		err = s.Client.NewCloseUserStreamService().ListenKey(listenKey).Do(ctx)
	}

	if err != nil {
		log.WithError(err).Errorf("error deleting listen key: %s", util.MaskKey(listenKey))
		return err
	}

	return nil
}
