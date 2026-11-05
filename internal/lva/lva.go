// Package lva — серверная сторона Peripheral WebSocket API Linux Voice
// Assistant (docs/peripheral_api.md). Роли перевёрнуты относительно upstream:
// marusia-d слушает (по умолчанию :6055), LVA подключается сам — колонка не
// знает адреса LVA и молча работает изолированно, пока мозг не поднялся.
// Слушает события диалога (wake/listening/thinking/speaking/idle...) и шлёт
// команды (start_listening, mute_mic...).
package lva

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Event — сообщение LVA: {"event": "...", "data": {...}}.
type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// Внутренние синтетические события (не от LVA).
const (
	EvDown = "_down" // соединение потеряно / не установлено
)

// Client — потокобезопасный отправитель команд.
type Client struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

// Send шлёт команду; молча пропускает, если связи нет.
func (c *Client) Send(cmd string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return
	}
	m := map[string]any{"command": cmd}
	if data != nil {
		m["data"] = data
	}
	if err := c.conn.WriteJSON(m); err != nil {
		log.Printf("lva: send %s: %v", cmd, err)
	}
}

// Connected — есть ли живой клиент LVA. lvaClient != nil всегда истина
// (объект создаётся в New), проверять надо именно соединение.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// RegisterLight — объявить кольцо как light-сущность HA (идемпотентно).
func (c *Client) RegisterLight(name, objectID string, effects []string) {
	c.Send("register_light", map[string]any{
		"name": name, "object_id": objectID, "effects": effects,
		"supports_rgb": true, "supports_brightness": true,
	})
}

// Server — WS-сервер периферии: слушает addr (напр. ":6055"), LVA — клиент.
// Пока клиентов нет, демон тихо живёт без LVA; первый коннект кладёт в канал
// события (включая snapshot), обрыв активного клиента — EvDown. Если LVA
// подключился повторно, прежний клиент выкидывается («последний побеждает»).
type Server struct {
	addr      string
	onConnect func(*Client)
	ch        chan Event
	client    *Client
}

// New — конструктор сервера: адрес прослушивания и обработчик подключения
// (вызывается после каждого коннекта — удобно для регистрации сущностей).
func New(addr string, onConnect func(*Client)) *Server {
	return &Server{
		addr:      addr,
		onConnect: onConnect,
		ch:        make(chan Event, 16),
		client:    &Client{},
	}
}

// Events — канал событий; обрыв активного клиента приходит как EvDown.
func (s *Server) Events() <-chan Event { return s.ch }

// Client — отправитель команд в LVA.
func (s *Server) Client() *Client { return s.client }

// Serve — запустить ws-сервер; блокирует, запускать в горутине.
func (s *Server) Serve(ctx context.Context) {
	up := websocket.Upgrader{}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("lva: upgrade: %v", err)
			return
		}
		s.client.mu.Lock()
		old := s.client.conn
		s.client.conn = ws
		s.client.mu.Unlock()
		if old != nil {
			old.Close() // его read-loop увидит замену и уйдёт молча
		}
		log.Printf("lva: подключено с %s", r.RemoteAddr)
		if s.onConnect != nil {
			s.onConnect(s.client)
		}
		for {
			_, raw, err := ws.ReadMessage()
			if err != nil {
				s.client.mu.Lock()
				active := s.client.conn == ws
				if active {
					s.client.conn = nil
				}
				s.client.mu.Unlock()
				ws.Close()
				if active {
					log.Printf("lva: клиент отвалился: %v", err)
					notify(ctx, s.ch, Event{Event: EvDown})
				}
				return
			}
			var e Event
			if json.Unmarshal(raw, &e) == nil && e.Event != "" {
				notify(ctx, s.ch, e)
			}
		}
	})

	go func() {
		for ctx.Err() == nil {
			l, err := net.Listen("tcp", s.addr)
			if err != nil {
				log.Printf("lva: listen %s: %v (повтор через 5с)", s.addr, err)
				sleepCtx(ctx, 5*time.Second)
				continue
			}
			srv := &http.Server{Handler: mux}
			go func() { <-ctx.Done(); srv.Close() }()
			log.Printf("lva: жду LVA на %s", s.addr)
			if err := srv.Serve(l); err != nil && ctx.Err() == nil {
				log.Printf("lva: serve: %v (повтор через 5с)", err)
				sleepCtx(ctx, 5*time.Second)
			}
		}
		s.client.mu.Lock()
		if s.client.conn != nil {
			s.client.conn.Close()
		}
		s.client.mu.Unlock()
	}()
}

func notify(ctx context.Context, eventCh chan<- Event, e Event) {
	select {
	case eventCh <- e:
	case <-ctx.Done():
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// --- парсеры data для известных событий ---

func (e Event) Bool(key string) bool {
	var d map[string]bool
	json.Unmarshal(e.Data, &d)
	return d[key]
}

func (e Event) Float(key string) float64 {
	var d map[string]float64
	json.Unmarshal(e.Data, &d)
	return d[key]
}

func (e Event) Str(key string) string {
	var d map[string]string
	json.Unmarshal(e.Data, &d)
	return d[key]
}

// Has — есть ли ключ в data (отличить «нет данных» от нуля).
func (e Event) Has(key string) bool {
	var d map[string]json.RawMessage
	json.Unmarshal(e.Data, &d)
	_, ok := d[key]
	return ok
}

// LightCommand — data события light_command.
type LightCommand struct {
	ObjectID   string  `json:"object_id"`
	State      bool    `json:"state"`
	Brightness float64 `json:"brightness"`
	Red        float64 `json:"red"`
	Green      float64 `json:"green"`
	Blue       float64 `json:"blue"`
	Effect     string  `json:"effect"`
}

func (e Event) LightCommand() LightCommand {
	var lc LightCommand
	json.Unmarshal(e.Data, &lc)
	return lc
}
