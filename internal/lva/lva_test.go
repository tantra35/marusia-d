package lva

// Тесты серверной стороны: коннект клиента-«LVA», события в обе стороны,
// EvDown при обрыве, режим «последний побеждает».

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testAddr = "127.0.0.1:60599"

func dialLVA(t *testing.T) *websocket.Conn {
	t.Helper()
	var ws *websocket.Conn
	var err error
	for i := 0; i < 50; i++ { // слушатель поднимается асинхронно
		ws, _, err = websocket.DefaultDialer.Dial("ws://"+testAddr, nil)
		if err == nil {
			return ws
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dial: %v", err)
	return nil
}

// ждёт событие из канала не дольше d
func waitEvent(t *testing.T, ch <-chan Event, d time.Duration) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(d):
		t.Fatal("событие не пришло")
		return Event{}
	}
}

func TestServe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(testAddr, nil)
	go srv.Serve(ctx)
	ch, c := srv.Events(), srv.Client()

	ws := dialLVA(t)

	// событие от LVA доходит с разобранным data
	if err := ws.WriteJSON(map[string]any{"event": "snapshot", "data": map[string]any{"muted": true}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	e := waitEvent(t, ch, 2*time.Second)
	if e.Event != "snapshot" || !e.Bool("muted") {
		t.Fatalf("snapshot: got %+v muted=%v", e.Event, e.Bool("muted"))
	}

	// команда от демона доходит до LVA
	c.Send("start_listening", nil)
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read cmd: %v", err)
	}
	var got map[string]any
	if json.Unmarshal(raw, &got) != nil || got["command"] != "start_listening" {
		t.Fatalf("cmd: %s", raw)
	}

	// обрыв активного клиента → EvDown
	ws.Close()
	if e := waitEvent(t, ch, 2*time.Second); e.Event != EvDown {
		t.Fatalf("want EvDown, got %s", e.Event)
	}

	// повторный коннект: «последний побеждает», сервер жив
	ws2 := dialLVA(t)
	defer ws2.Close()
	if err := ws2.WriteJSON(map[string]any{"event": "wake_word_detected"}); err != nil {
		t.Fatalf("write2: %v", err)
	}
	if e := waitEvent(t, ch, 2*time.Second); e.Event != "wake_word_detected" {
		t.Fatalf("wake: got %s", e.Event)
	}
}

// nil-клиент (при -lva "" lvaClient не создаётся) — Send/Connected no-op
func TestNilClient(t *testing.T) {
	var c *Client
	c.Send("volume_changed", map[string]any{"volume": 0.5})
	c.Send("media_player_state", nil)
	if c.Connected() {
		t.Fatal("nil-клиент не может быть подключён")
	}
}

// StrOrList — media_url приходит строкой или списком строк
func TestStrOrList(t *testing.T) {
	ev := func(data string) Event {
		return Event{Data: json.RawMessage(data)}
	}
	if got := ev(`{"media_url":"http://radio"}`).StrOrList("media_url"); len(got) != 1 || got[0] != "http://radio" {
		t.Fatalf("строка: %q", got)
	}
	if got := ev(`{"media_url":["a","b"]}`).StrOrList("media_url"); len(got) != 2 || got[1] != "b" {
		t.Fatalf("список: %q", got)
	}
	if got := ev(`{}`).StrOrList("media_url"); got != nil {
		t.Fatalf("нет поля: %q", got)
	}
	if got := ev(`{"media_url":5}`).StrOrList("media_url"); got != nil {
		t.Fatalf("чужой тип: %q", got)
	}
}
