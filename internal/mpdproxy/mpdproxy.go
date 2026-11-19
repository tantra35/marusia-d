// Package mpdproxy — MPD-протокольный прокси между upmpdcli и реальным mpd.
//
// Схема (2026-11-19, прозрачная пересылка idle; замена upmpdcli-volhook/volapi):
//
//	upmpdcli ──(MPD-протокол)──> marusia-d :6600 ──прокси──> mpd :6601
//	                                │
//	                                └─ перехват setvol/volume/status
//	                                   + mixer-инжект через noidle
//
// Реальный mpd слушает 127.0.0.1:6601 (mixer_type none — громкость mpd не
// регулирует). Архитектура — ЧЕСТНЫЙ ПРОКСИ: idle клиента форвардится в
// mpd как есть, mpd сам держит соединение в idle, буферизует свой поток
// событий и ничего не теряет; каждое подключение получает свою горутину-
// ридер бэкенда. Прокси перехватывает ровно три вещи:
//
//	setvol N / volume ±N → громкость sink через коллбэк Volume.Set
//	                       (upmpdcli шлёт mute как setvol 0); ответ OK;
//	status               → форвард ответа mpd со вставкой/переписыванием
//	                       строки «volume: N» (mpd 0.24 при mixer none
//	                       строки не отдаёт вовсе);
//	mixer-инжект         → при смене громкости (NotifyVolume) в
//	                       форварднутый idle бэкенду шлётся noidle,
//	                       к ответу добавляется «changed: mixer»
//	                       (стандартный трюк mixer-демона).
//
// Жизненный цикл честный: mpd закрыл бэкенд → прокси закрывает и
// клиентское соединение (клиент переподключается сам).
package mpdproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// debug — дамп всего трафика в stderr (env MARUSIA_MPDPROXY_DEBUG=1).
// Для живой отладки: демон пускается руками из /tmp, stdout смотрит ssh.
var debug atomic.Bool

func pdebug(id int, format string, args ...any) {
	if debug.Load() {
		log.Printf("mpdproxy[%d] %s", id, fmt.Sprintf(format, args...))
	}
}

// Volume — мост громкости в демон. Get — текущая ВИЗУАЛЬНАЯ громкость
// 0..100 (-1 = не узнали); Set — применить визуальные 0..100 (sink,
// отклик, репорт LVA — внутри реализации).
type Volume struct {
	Get func() int
	Set func(v int)
}

// Proxy — слушатель MPD-протокола. NotifyVolume безопасен на nil
// (прокси может быть выключен флагом).
type Proxy struct {
	addr    string // слушать (127.0.0.1:6600)
	backend string // реальный mpd (127.0.0.1:6601)
	vol     Volume

	mu     sync.Mutex
	conns  map[*pconn]struct{}
	ln     net.Listener
	nextID int32
}

// New — создать прокси; запуск — Serve.
func New(addr, backend string, vol Volume) *Proxy {
	return &Proxy{addr: addr, backend: backend, vol: vol, conns: make(map[*pconn]struct{})}
}

// Addr — фактический адрес слушателя (пусто до старта Serve).
func (p *Proxy) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}

// Serve — цикл accept до закрытия ctx.
func (p *Proxy) Serve(ctx context.Context) {
	debug.Store(os.Getenv("MARUSIA_MPDPROXY_DEBUG") == "1")
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		log.Printf("mpdproxy: listen %s: %v", p.addr, err)
		return
	}
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		<-ctx.Done()
		ln.Close()
		p.mu.Lock()
		conns := make([]*pconn, 0, len(p.conns))
		for c := range p.conns {
			conns = append(conns, c)
		}
		p.mu.Unlock()
		for _, c := range conns {
			c.shutdown()
		}
	}()
	log.Printf("mpdproxy: слушаю %s → %s", p.addr, p.backend)
	for {
		cli, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("mpdproxy: accept: %v", err)
			continue
		}
		go p.handle(cli)
	}
}

// NotifyVolume — громкость изменилась (любой источник): инжект
// «changed: mixer» во все соединения, находящиеся в форварднутом idle.
func (p *Proxy) NotifyVolume() {
	if p == nil {
		return
	}
	p.mu.Lock()
	conns := make([]*pconn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		c.injectMixer()
	}
}

// idleCount — соединений в форварднутом idle (тестам).
func (p *Proxy) idleCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for c := range p.conns {
		if c.idleForwarded.Load() {
			n++
		}
	}
	return n
}

// handle — жизнь одного клиентского соединения.
func (p *Proxy) handle(cli net.Conn) {
	id := int(atomic.AddInt32(&p.nextID, 1))
	be, err := net.Dial("tcp", p.backend)
	if err != nil {
		log.Printf("mpdproxy: бэкенд %s: %v", p.backend, err)
		cli.Close()
		return
	}
	c := &pconn{
		p:   p,
		id:  id,
		cli: cli,
		clr: bufio.NewReader(cli),
		clw: bufio.NewWriter(cli),
		be:  be,
		ber: bufio.NewReader(be),
		bew: bufio.NewWriter(be),
	}
	p.mu.Lock()
	p.conns[c] = struct{}{}
	p.mu.Unlock()
	pdebug(id, "open %s", cli.RemoteAddr())
	log.Printf("mpdproxy[%d] бэкенд-коннект: local=%v remote=%v", id, be.LocalAddr(), be.RemoteAddr())

	defer func() {
		pdebug(id, "close")
		c.shutdown()
	}()

	// приветствие mpd форвардим как есть — на версии сидят проверки
	// совместимости клиентов
	greeting, gerr := c.ber.ReadString('\n')
	if gerr != nil {
		return
	}
	if _, err := c.clw.WriteString(greeting); err != nil {
		return
	}
	c.clw.Flush()

	// ридер бэкенда — единственный читатель ber: пересылает ответы mpd
	// клиенту, кроме перехваченных (catch)
	go c.backendReader()

	for {
		line, err := c.clr.ReadString('\n')
		if err != nil {
			return
		}
		if !c.dispatch(strings.TrimRight(line, "\r\n")) {
			return
		}
	}
}

// pconn — одно клиентское соединение + его соединение к mpd.
type pconn struct {
	p   *Proxy
	id  int
	cli net.Conn
	clr *bufio.Reader // клиентские команды (dispatch-горутина)
	clw *bufio.Writer // клиенту (wmu)
	be  net.Conn
	ber *bufio.Reader // ответы mpd (только backendReader)
	bew *bufio.Writer // к mpd (bmu)

	wmu sync.Mutex // сериализация записей клиенту (ответы + инжекты)
	bmu sync.Mutex // сериализация записей к mpd (dispatch + инжект noidle)

	// catch — перехват СЛЕДУЮЩЕГО ответа бэкенда (dispatch или инжект
	// пишет сюда канал до отправки команды; ридер отдаёт строки в канал
	// вместо клиента)
	catchMu sync.Mutex
	catch   chan []string

	idleForwarded atomic.Bool  // бэкенд сидит в нашем форварднутом idle
	idleSubs      atomic.Value // маска последнего idle (string; "" = все)
	injectMu      sync.Mutex   // один инжект mixer на соединение
	shutdownOne   sync.Once
}

// registerCatch — зарегистрировать перехват следующего ответа бэкенда.
// Протокол mpd последовательный: незакрытый перехват может быть только
// один, поэтому при занятости отказываем (вызывающий не должен слать
// вторую команду поверх).
func (c *pconn) registerCatch(ch chan []string) bool {
	c.catchMu.Lock()
	defer c.catchMu.Unlock()
	if c.catch != nil {
		return false // уже ждут другой ответ
	}
	c.catch = ch
	return true
}

// takeCatch — забрать зарегистрированный перехват (nil если нет).
func (c *pconn) takeCatch() chan []string {
	c.catchMu.Lock()
	defer c.catchMu.Unlock()
	ch := c.catch
	c.catch = nil
	return ch
}

// backendReader — единственный читатель ber: пересылает ответы mpd
// клиенту, кроме перехваченных (catch). Живёт, пока соединение не закрыто
// (shutdown закрывает be — ридер разблокируется и выходит).
func (c *pconn) backendReader() {
	for {
		var lines []string
		for {
			l, err := c.ber.ReadString('\n')
			if err != nil {
				pdebug(c.id, "reader: конец потока бэкенда (%v)", err)
				c.shutdown()
				return
			}
			if strings.HasPrefix(l, "binary: ") {
				n, perr := strconv.Atoi(strings.TrimSpace(l[len("binary: "):]))
				if perr != nil {
					pdebug(c.id, "reader: binary parse: %v", perr)
					c.shutdown()
					return
				}
				buf := make([]byte, n+1) // payload + '\n'
				if _, err := io.ReadFull(c.ber, buf); err != nil {
					c.shutdown()
					return
				}
				lines = append(lines, l, string(buf))
				continue
			}
			lines = append(lines, l)
			if l == "OK\n" || strings.HasPrefix(l, "ACK ") {
				break
			}
		}
		// перехват? (dispatch/инжект ждут именно этот ответ)
		if ch := c.takeCatch(); ch != nil {
			ch <- lines
			continue
		}
		c.writeLines(lines...)
	}
}

// dispatch — одна команда клиента. false = закрыть соединение.
func (c *pconn) dispatch(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return true
	}
	cmd := strings.ToLower(fields[0])
	pdebug(c.id, "cmd %q (idle-forwarded: %v)", line, c.idleForwarded.Load())

	switch cmd {
	case "setvol":
		// libmpdclient цитирует аргумент: на проводе «setvol "85"»
		// (mpd-парсер кавычки легально снимает; у нас было источником
		// ACK-ошибки, отравлявшей соединение upmpdcli)
		if len(fields) < 2 {
			return c.ack(cmd, "нужен аргумент 0..100")
		}
		n, err := strconv.Atoi(strings.Trim(fields[1], `"`))
		if err != nil || n < 0 {
			return c.ack(cmd, "некорректная громкость "+fields[1])
		}
		if n > 100 {
			n = 100 // mpd-диапазон; upmpdcli больше и не шлёт
		}
		c.p.vol.Set(n)
		c.writeLines("OK\n")

	case "volume":
		// deprecated-команда относительной громкости (в кавычках —
		// см. setvol). Не перехватывать нельзя — mixer none у mpd
		// ответил бы ACK.
		if len(fields) < 2 {
			return c.ack(cmd, "нужен аргумент")
		}
		arg := strings.Trim(fields[1], `"`)
		cur := c.p.vol.Get()
		if cur < 0 {
			c.writeLines("OK\n") // громкость не узнали — молча no-op
			return true
		}
		var v int
		switch {
		case strings.HasPrefix(arg, "+"):
			d, err := strconv.Atoi(arg[1:])
			if err != nil {
				return c.ack(cmd, "некорректный аргумент "+arg)
			}
			v = cur + d
		case strings.HasPrefix(arg, "-"):
			d, err := strconv.Atoi(arg[1:])
			if err != nil {
				return c.ack(cmd, "некорректный аргумент "+arg)
			}
			v = cur - d
		default:
			d, err := strconv.Atoi(arg)
			if err != nil {
				return c.ack(cmd, "некорректный аргумент "+arg)
			}
			v = d
		}
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		c.p.vol.Set(v)
		c.writeLines("OK\n")

	case "status":
		// форвард с перехватом ответа: mixer none у mpd 0.24 — строки
		// volume нет вовсе (0.23 отдавал «volume: -1»); вставляем/переписываем
		// на реальную визуальную громкость — без этого upmpdcli не видит
		// громкости и контроллерам раздаётся пустота
		ch := make(chan []string) // небуферизованный
		if !c.registerCatch(ch) {
			// занято (не должно случаться при последовательном mpd):
			// шлём без перехвата — сырой ответ вернёт ридер
			pdebug(c.id, "status: catch занят — форвард без вставки volume")
			if err := c.backendWrite(line); err != nil {
				return false
			}
			return true
		}
		if err := c.backendWrite(line); err != nil {
			c.registerCatch(nil)
			return false
		}
		select {
		case lines := <-ch:
			if v := c.p.vol.Get(); v >= 0 {
				inserted := false
				for i, l := range lines {
					if strings.HasPrefix(l, "volume:") {
						lines[i] = fmt.Sprintf("volume: %d\n", v)
						inserted = true
						break
					}
				}
				if !inserted {
					lines = append([]string{fmt.Sprintf("volume: %d\n", v)}, lines...)
				}
				pdebug(c.id, "status volume → %d", v)
			}
			c.writeLines(lines...)
		case <-time.After(10 * time.Second):
			// владелец сдался: запоздавший ответ уходит клиенту
			// (синхрон потока), затем соединение закрываем
			pdebug(c.id, "status: таймаут ответа бэкенда")
			if ch := c.takeCatch(); ch != nil {
				select {
				case lines := <-ch:
					c.writeLines(lines...)
				default:
				}
			}
			return false
		}

	case "idle":
		// форвард в mpd КАК ЕСТЬ: mpd сам держит соединение в idle,
		// буферизует события (ничего не теряется в окнах обработки) и
		// exempt от connection_timeout. Ответ (буферизованные события)
		// уйдёт клиенту через ридер. idleForwarded/idleSubs нужны для
		// инжекта changed: mixer при смене громкости (инжект — только
		// если клиент запрашивал mixer).
		if err := c.backendWrite(line); err != nil {
			return false
		}
		subs := ""
		if len(fields) > 1 {
			subs = strings.Join(fields[1:], " ")
		}
		c.idleSubs.Store(subs)
		c.idleForwarded.Store(true)

	case "noidle":
		// форвард; ответ (буферизованные события или пустой OK) уйдёт
		// клиенту через ридер
		if err := c.backendWrite(line); err != nil {
			return false
		}

	case "close":
		if err := c.backendWrite(line); err != nil {
			pdebug(c.id, "close: backend write err: %v", err)
		}
		return false

	default:
		// прозрачный форвард; command_list_* тоже летит насквозь —
		// upmpdcli списков не шлёт, а перехват внутри списка сломал бы
		// соответствие команды и list_OK
		if cmd == "command_list_begin" {
			pdebug(c.id, "command_list от клиента — форвард без перехвата")
		}
		if err := c.backendWrite(line); err != nil {
			return false
		}
	}
	return true
}

// backendWrite — отправка строки к mpd (bmu).
func (c *pconn) backendWrite(line string) error {
	c.bmu.Lock()
	defer c.bmu.Unlock()
	c.be.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer c.be.SetWriteDeadline(time.Time{})
	if _, err := c.bew.WriteString(line); err != nil {
		return err
	}
	if err := c.bew.WriteByte('\n'); err != nil {
		return err
	}
	if err := c.bew.Flush(); err != nil {
		log.Printf("mpdproxy[?] ЗАПИСАНО err=%v", err)
	}
	log.Printf("mpdproxy[?] ЗАПИСАНО ok: %q", line)
	return nil
}

// injectMixer — инжект «changed: mixer» в форварднутый idle: бэкенду
// шлётся noidle (ответ перехватывается), к ответу добавляется mixer,
// если его там не было, и всё уходит клиенту.
func (c *pconn) injectMixer() {
	if !c.idleForwarded.Load() {
		return // не в idle — upmpdcli узнает громкость на ближайшем status
	}
	// клиент не запрашивал mixer в idle — инжект был бы неожиданным
	// изменением протокола (upmpdcli запрашивает, но подстраховка)
	if subs, _ := c.idleSubs.Load().(string); subs != "" && !strings.Contains(" "+subs+" ", " mixer ") {
		return
	}
	c.injectMu.Lock()
	defer c.injectMu.Unlock()
	// клиент мог выйти из idle, пока ждали мьютекс
	if !c.idleForwarded.Load() {
		return
	}
	ch := make(chan []string) // небуферизованный
	if !c.registerCatch(ch) {
		return // занято — пропускаем инжект (upmpdcli узнает из status)
	}
	if err := c.backendWrite("noidle"); err != nil {
		c.registerCatch(nil)
		return
	}
	select {
	case lines := <-ch:
		hasMixer := false
		for _, l := range lines {
			if strings.HasPrefix(l, "changed: mixer") {
				hasMixer = true
				break
			}
		}
		if !hasMixer {
			// mixer вставляется ПЕРЕД завершающим OK (ответ noidle
			// обычно — просто "OK"); строки после OK были бы
			// неожиданными для клиента
			trimmed := lines[:0]
			for _, l := range lines {
				if l != "OK\n" && !strings.HasPrefix(l, "ACK ") {
					trimmed = append(trimmed, l)
				}
			}
			lines = append(trimmed, "changed: mixer\n", "OK\n")
		}
		pdebug(c.id, "inject changed: mixer")
		c.writeLines(lines...)
		c.idleForwarded.Store(false)
	case <-time.After(5 * time.Second):
		// запоздавший ответ уходит клиенту (синхрон), соединение закрываем
		if ch := c.takeCatch(); ch != nil {
			select {
			case lines := <-ch:
				c.writeLines(lines...)
			default:
			}
		}
		pdebug(c.id, "inject mixer: таймаут ответа на noidle")
	}
}

// ack — ответ в формате ошибки mpd, чтобы клиент отработал её штатно.
func (c *pconn) ack(cmd, msg string) bool {
	c.writeLines(fmt.Sprintf("ACK [2@0] {%s} %s\n", cmd, msg))
	return true
}

// writeLines — сериализованная запись клиенту.
func (c *pconn) writeLines(lines ...string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	for _, l := range lines {
		c.clw.WriteString(l)
	}
	c.clw.Flush()
}

// shutdown — закрыть оба соединения ровно один раз и вычеркнуть себя.
func (c *pconn) shutdown() {
	c.shutdownOne.Do(func() {
		c.cli.Close()
		c.be.Close()
		c.p.mu.Lock()
		delete(c.p.conns, c)
		c.p.mu.Unlock()
	})
}
