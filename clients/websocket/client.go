package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	websocketmodels "github.com/joaquinbejar/deribit-api/clients/websocket/models"
	"github.com/joaquinbejar/deribit-api/pkg/deribit"
	"github.com/joaquinbejar/deribit-api/pkg/models"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chuckpreslar/emission"
	"github.com/sourcegraph/jsonrpc2"
	"nhooyr.io/websocket"
)

var (
	ErrAuthenticationIsRequired = errors.New("authentication is required")
)

type Client struct {
	ctx           context.Context
	addr          string
	apiKey        string
	secretKey     string
	autoReconnect bool
	debugMode     bool

	conn        *websocket.Conn
	rpcConn     *jsonrpc2.Conn
	mu          sync.RWMutex
	heartCancel chan struct{}
	isConnected bool

	auth struct {
		token   string
		refresh string
	}

	subscriptions    []string
	subscriptionsMap map[string]struct{}

	emitter *emission.Emitter
}

func NewDeribitWsClient(cfg *deribit.Configuration) *Client {
	ctx := cfg.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	client := &Client{
		ctx:              ctx,
		addr:             cfg.WsAddr,
		apiKey:           cfg.ApiKey,
		secretKey:        cfg.SecretKey,
		autoReconnect:    cfg.AutoReconnect,
		debugMode:        cfg.DebugMode,
		subscriptionsMap: make(map[string]struct{}),
		emitter:          emission.NewEmitter(),
	}
	err := client.start()
	if err != nil {
		log.Fatal(err)
	}
	return client
}

// setIsConnected sets state for isConnected
func (c *Client) setIsConnected(state bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.isConnected = state
}

// IsConnected returns the WebSocket connection state
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.isConnected
}

func (c *Client) Subscribe(channels []string) {
	c.subscriptions = append(c.subscriptions, channels...)
	c.subscribe(channels)
}

func (c *Client) subscribe(channels []string) {
	var publicChannels []string
	var privateChannels []string

	for _, v := range c.subscriptions {
		if _, ok := c.subscriptionsMap[v]; ok {
			continue
		}
		if strings.HasPrefix(v, "user.") {
			privateChannels = append(privateChannels, v)
		} else {
			publicChannels = append(publicChannels, v)
		}
	}

	if len(publicChannels) > 0 {
		_, err := c.PublicSubscribe(&models.SubscribeParams{
			Channels: publicChannels,
		})
		if err != nil {
			return
		}
	}
	if len(privateChannels) > 0 {
		_, err := c.PrivateSubscribe(&models.SubscribeParams{
			Channels: privateChannels,
		})
		if err != nil {
			return
		}
	}

	allChannels := append(publicChannels, privateChannels...)
	for _, v := range allChannels {
		c.subscriptionsMap[v] = struct{}{}
	}
}

func (c *Client) start() error {
	c.setIsConnected(false)
	c.subscriptionsMap = make(map[string]struct{})
	c.conn = nil
	c.rpcConn = nil
	c.heartCancel = make(chan struct{})

	for i := 0; i < deribit.MaxTryTimes; i++ {
		conn, _, err := c.connect()
		if err != nil {
			log.Println(err)
			tm := (i + 1) * 5
			log.Printf("Sleep %vs", tm)
			time.Sleep(time.Duration(tm) * time.Second)
			continue
		}
		c.conn = conn
		break
	}
	if c.conn == nil {
		return errors.New("connect fail")
	}

	c.rpcConn = jsonrpc2.NewConn(context.Background(), websocketmodels.NewObjectStream(c.conn), c)

	c.setIsConnected(true)

	// auth
	if c.apiKey != "" && c.secretKey != "" {
		if err := c.Auth(c.apiKey, c.secretKey); err != nil {
			log.Printf("auth error: %v", err)
		}
	}

	// subscribe
	c.subscribe(c.subscriptions)

	_, err := c.SetHeartbeat(&models.SetHeartbeatParams{Interval: 30})
	if err != nil {
		return err
	}

	if c.autoReconnect {
		go c.reconnect()
	}

	go c.heartbeat()

	return nil
}

// Call issues JSONRPC v2 calls
func (c *Client) Call(method string, params interface{}, result interface{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New(fmt.Sprintf("%v", r))
		}
	}()

	if !c.IsConnected() {
		return errors.New("not connected")
	}
	if params == nil {
		params = websocketmodels.EmptyParams
	}

	if token, ok := params.(websocketmodels.PrivateParams); ok {
		if c.auth.token == "" {
			return ErrAuthenticationIsRequired
		}
		token.SetToken(c.auth.token)
	}

	return c.rpcConn.Call(c.ctx, method, params, result)
}

// Handle implements jsonrpc2.Handler
func (c *Client) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	if req.Method == "subscription" {
		// update events
		if req.Params != nil && len(*req.Params) > 0 {
			var event websocketmodels.Event
			if err := json.Unmarshal(*req.Params, &event); err != nil {
				//c.setError(err)
				return
			}
			c.subscriptionsProcess(&event)
		}
	}
}

func (c *Client) heartbeat() {
	t := time.NewTicker(3 * time.Second)
	for {
		select {
		case <-t.C:
			_, err := c.Test()
			if err != nil {
				return
			}
		case <-c.heartCancel:
			return
		}
	}
}

func (c *Client) reconnect() {
	notify := c.rpcConn.DisconnectNotify()
	<-notify
	c.setIsConnected(false)

	log.Println("disconnect, reconnect...")

	close(c.heartCancel)

	time.Sleep(1 * time.Second)

	err := c.start()
	if err != nil {
		return
	}
}

func (c *Client) connect() (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, c.addr, &websocket.DialOptions{})
	if err == nil {
		conn.SetReadLimit(32768 * 64)
	}
	return conn, resp, err
}