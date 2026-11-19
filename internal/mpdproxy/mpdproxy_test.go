package mpdproxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeBackend — минимальный mpd: баннер, status, эхо-OK. statusVolume:
// "" = строка volume НЕ отдаётся (mpd 0.24 при mixer none), "-1" = стиль
// mpd 0.23, прочее — конкретное значение (для теста переписи).
type fakeBackend struct {
	ln           net.Listener
	statusVolume string
	banner       string
}

func newFakeBackend(t *testing.T) *fakeBackend {
	return newFakeBackendV(t, "") // по умолчанию — поведение mpd 0.24 на плате
}

func newFakeBackendV(t *testing.T, statusVolume string) *fakeBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeBackend{ln: ln, statusVolume: statusVolume, banner: "OK MPD 0.24.0\n"}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go fb.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return fb
}

func (fb *fakeBackend) serve(c net.Conn) {
	defer c.Close()
	fmt.Fprint(c, fb.banner)
	br := bufio.NewReader(c)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		switch {
		case cmd == "status":
			if fb.statusVolume != "" {
				fmt.Fprintf(c, "volume: %s\n", fb.statusVolume)
			}
			fmt.Fprint(c, "repeat: 0\nstate: play\nOK\n")
		default:
			fmt.Fprint(c, "OK\n")
		}
	}
}

// dialProxy — подключиться к прокси и прочитать баннер.
func dialProxy(t *testing.T, addr string) (*bufio.Reader, net.Conn) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(c)
	greeting, err := br.ReadString('\n')
	if err != nil || greeting != "OK MPD 0.24.0\n" {
		t.Fatalf("баннер: %q err=%v", greeting, err)
	}
	return br, c
}

func send(t *testing.T, c net.Conn, cmd string) {
	t.Helper()
	if _, err := fmt.Fprintf(c, "%s\n", cmd); err != nil {
		t.Fatalf("send %q: %v", cmd, err)
	}
}

// readResp — читает ответ до OK/ACK.
func readResp(t *testing.T, br *bufio.Reader) []string {
	t.Helper()
	var lines []string
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("чтение ответа: %v (прочитано %q)", err, lines)
		}
		lines = append(lines, l)
		if l == "OK\n" || strings.HasPrefix(l, "ACK ") {
			return lines
		}
	}
}

// volrec — громкость с записью Set (для проверок перехвата).
type volrec struct {
	cur  int
	sets []int
}

func (v *volrec) volume() Volume {
	return Volume{
		Get: func() int { return v.cur },
		Set: func(n int) { v.cur = n; v.sets = append(v.sets, n) },
	}
}

func startProxy(t *testing.T, backend string, vol Volume) *Proxy {
	t.Helper()
	p := New("127.0.0.1:0", backend, vol)
	go p.Serve(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for p.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("прокси не стартовал")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return p
}

func TestSetvolIntercepted(t *testing.T) {
	fb := newFakeBackend(t)
	v := &volrec{cur: 50}
	p := startProxy(t, fb.ln.Addr().String(), v.volume())
	br, c := dialProxy(t, p.Addr())

	send(t, c, "setvol 55")
	if got := readResp(t, br); len(got) != 1 || got[0] != "OK\n" {
		t.Fatalf("setvol: %q", got)
	}
	if len(v.sets) != 1 || v.sets[0] != 55 {
		t.Fatalf("Set не вызван: %v", v.sets)
	}
}

func TestSetvolClamp(t *testing.T) {
	fb := newFakeBackend(t)
	v := &volrec{cur: 50}
	p := startProxy(t, fb.ln.Addr().String(), v.volume())
	br, c := dialProxy(t, p.Addr())

	send(t, c, "setvol 130")
	if got := readResp(t, br); len(got) != 1 || got[0] != "OK\n" {
		t.Fatalf("setvol 130: %q", got)
	}
	if len(v.sets) != 1 || v.sets[0] != 100 {
		t.Fatalf("кламп: %v", v.sets)
	}
	send(t, c, "setvol -3")
	if got := readResp(t, br); len(got) != 1 || !strings.HasPrefix(got[0], "ACK ") {
		t.Fatalf("setvol -3 должен дать ACK: %q", got)
	}
}

func TestSetvolQuoted(t *testing.T) {
	// libmpdclient цитирует число: «setvol "85"» — это валидный mpd-синтаксис
	fb := newFakeBackend(t)
	v := &volrec{cur: 50}
	p := startProxy(t, fb.ln.Addr().String(), v.volume())
	br, c := dialProxy(t, p.Addr())

	send(t, c, `setvol "85"`)
	if got := readResp(t, br); len(got) != 1 || got[0] != "OK\n" {
		t.Fatalf("setvol quoted: %q", got)
	}
	if len(v.sets) != 1 || v.sets[0] != 85 {
		t.Fatalf("Set: %v", v.sets)
	}
	send(t, c, `volume "+5"`)
	readResp(t, br)
	if v.cur != 90 {
		t.Fatalf("volume quoted relative: cur=%d", v.cur)
	}
}

func TestVolumeRelative(t *testing.T) {
	fb := newFakeBackend(t)
	v := &volrec{cur: 50}
	p := startProxy(t, fb.ln.Addr().String(), v.volume())
	br, c := dialProxy(t, p.Addr())

	send(t, c, "volume +10")
	readResp(t, br)
	if v.cur != 60 {
		t.Fatalf("volume +10: cur=%d", v.cur)
	}
	send(t, c, "volume -70")
	readResp(t, br)
	if v.cur != 0 {
		t.Fatalf("volume -70 (кламп пола): cur=%d", v.cur)
	}
}

func TestStatusRewritesVolume(t *testing.T) {
	// mpd 0.24 на плате: строки volume НЕТ — прокси вставляет её первой
	fb := newFakeBackend(t) // statusVolume=""
	v := &volrec{cur: 77}
	p := startProxy(t, fb.ln.Addr().String(), v.volume())
	br, c := dialProxy(t, p.Addr())

	send(t, c, "status")
	got := readResp(t, br)
	want := []string{"volume: 77\n", "repeat: 0\n", "state: play\n", "OK\n"}
	if strings.Join(got, "") != strings.Join(want, "") {
		t.Fatalf("status: %q, хотим %q", got, want)
	}
}

func TestStatusRewritesLegacyVolumeLine(t *testing.T) {
	// mpd 0.23-стиль: «volume: -1» переписывается на месте
	fb := newFakeBackendV(t, "-1")
	v := &volrec{cur: 77}
	p := startProxy(t, fb.ln.Addr().String(), v.volume())
	br, c := dialProxy(t, p.Addr())

	send(t, c, "status")
	got := readResp(t, br)
	want := []string{"volume: 77\n", "repeat: 0\n", "state: play\n", "OK\n"}
	if strings.Join(got, "") != strings.Join(want, "") {
		t.Fatalf("status: %q, хотим %q", got, want)
	}
}

func TestIdleNotify(t *testing.T) {
	fb := newFakeBackend(t)
	v := &volrec{cur: 50}
	p := startProxy(t, fb.ln.Addr().String(), v.volume())
	br, c := dialProxy(t, p.Addr())

	send(t, c, "idle mixer player")
	// idle регистрируется в dispatch-горутине — дождаться, иначе
	// нотификация уйдёт в пустоту
	deadline := time.Now().Add(2 * time.Second)
	for p.idleCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle не зарегистрирован")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// громкость меняется → соединение получает changed: mixer
	p.NotifyVolume()
	got := readResp(t, br)
	if len(got) != 2 || got[0] != "changed: mixer\n" || got[1] != "OK\n" {
		t.Fatalf("idle notify: %q", got)
	}
}

func TestIdleFilterMismatchKeepsHolding(t *testing.T) {
	fb := newFakeBackend(t)
	v := &volrec{cur: 50}
	p := startProxy(t, fb.ln.Addr().String(), v.volume())
	br, c := dialProxy(t, p.Addr())

	send(t, c, "idle player") // mixer не запрошен
	p.NotifyVolume()
	// ответа быть не должно: соединение продолжает висеть в idle.
	// noidle завершает ожидание пустым OK.
	send(t, c, "noidle")
	got := readResp(t, br)
	if len(got) != 1 || got[0] != "OK\n" {
		t.Fatalf("noidle: %q", got)
	}
}

func TestIdleThenCommandCancelsIdle(t *testing.T) {
	fb := newFakeBackend(t)
	v := &volrec{cur: 50}
	p := startProxy(t, fb.ln.Addr().String(), v.volume())
	br, c := dialProxy(t, p.Addr())

	send(t, c, "idle")
	// команда на idle-соединении: idle снимается молча, приходит только
	// ответ команды (семантика mpd)
	send(t, c, "status")
	got := readResp(t, br)
	if !strings.HasPrefix(got[0], "volume:") {
		t.Fatalf("status после отмены idle: %q", got)
	}
}

func TestPassthrough(t *testing.T) {
	fb := newFakeBackend(t)
	v := &volrec{cur: 50}
	p := startProxy(t, fb.ln.Addr().String(), v.volume())
	br, c := dialProxy(t, p.Addr())

	send(t, c, "ping")
	if got := readResp(t, br); len(got) != 1 || got[0] != "OK\n" {
		t.Fatalf("ping: %q", got)
	}
	// после pass-through перехват продолжает работать
	send(t, c, "setvol 20")
	readResp(t, br)
	if len(v.sets) != 1 || v.sets[0] != 20 {
		t.Fatalf("setvol после pass-through: %v", v.sets)
	}
}

func TestBackendDown(t *testing.T) {
	// порт, где никто не слушает
	p := startProxy(t, "127.0.0.1:1", (&volrec{cur: 50}).volume())
	c, err := net.DialTimeout("tcp", p.Addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	// соединение должно закрыться без баннера
	buf := make([]byte, 64)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("ожидалось закрытие при мёртвом бэкенде")
	}
}

func TestNotifyVolumeNilSafe(t *testing.T) {
	var p *Proxy
	p.NotifyVolume() // не должно паниковать
}
