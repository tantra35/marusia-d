// Package mpdproxy — MPD-протокольный прокси между upmpdcli и реальным mpd.
//
// Схема (2026-11-19, замена upmpdcli-volhook/volapi):
//
//	upmpdcli ──(MPD-протокол)──> marusia-d :6600 ──прокси──> mpd :6601
//	                                │
//	                                └─ перехват setvol/volume/status/idle
//
// Реальный mpd слушает 127.0.0.1:6601 (mixer_type none — громкость mpd не
// регулирует). Прокси перехватывает ровно четыре вещи, всё остальное —
// прозрачный построчный форвард с сохранением границ ответа (OK/ACK):
//
//	setvol N / volume ±N → громкость sink через коллбэк Volume.Set
//	                       (upmpdcli шлёт mute как setvol 0), ответ OK;
//	status               → форвард ответа mpd с переписыванием строки
//	                       «volume: -1» (при mixer none) на реальную
//	                       визуальную громкость — без этого upmpdcli
//	                       раздаёт контроллерам мусор;
//	idle [subs...]       → соединение переводится в ожидание; при смене
//	                       громкости (NotifyVolume) получает синтетический
//	                       «changed: mixer» — штатный путь, которым
//	                       upmpdcli публикует громкость в RenderingControl.
//
// Стейт-машина пер-соединение: «кто в idle» — атрибут коннекта (mpd-клиенты
// многосоединённые, upmpdcli держит командное и idle-соединение отдельно).
// Команда на idle-соединении (кроме noidle) молча снимает idle — как в mpd.
// Для соединений в idle прокси держит связь с mpd живой периодическим ping
// (иначе connection_timeout mpd закрыл бы «молчащее» соединение).
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

// Volume — мост громкости в демон. Get — текущая ВИЗУАЛЬНАЯ громкость
// 0..100 (-1 = не узнали); Set — применить визуальные 0..100 (sink,
// отклик, репорт LVA — внутри реализации).
type Volume struct {
	Get func() int
	Set func(v int)
}

// pingInterval — держак idle-соединений через mpd: connection_timeout mpd
// (дефолт 30 с) закрывает соединения без команд; ping каждые 15 с.
const pingInterval = 15 * time.Second

// debug — дамп всего трафика в stderr (env MARUSIA_MPDPROXY_DEBUG=1).
// Для живой отладки: демон пускается руками из /tmp, stdout смотрит ssh.
var debug bool

func pdebug(id int, format string, args ...any) {
	if debug {
		log.Printf("mpdproxy[%d] %s", id, fmt.Sprintf(format, args...))
	}
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
	debug = os.Getenv("MARUSIA_MPDPROXY_DEBUG") == "1"
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

// NotifyVolume — громкость изменилась (любой источник): все соединения,
// сидящие в idle с интересом к mixer, получают «changed: mixer».
func (p *Proxy) NotifyVolume() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.notifyMixer()
	}
}

// idleCount — соединений в состоянии idle (тестам: дождаться регистрации).
func (p *Proxy) idleCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for c := range p.conns {
		c.mu.Lock()
		if c.idle {
			n++
		}
		c.mu.Unlock()
	}
	return n
}

// handle — жизнь одного клиентского соединения: баннер от mpd, затем
// построчный разбор команд. Любая ошибка любой стороны — рвём оба
// (клиент переподключится сам).
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

	defer func() {
		pdebug(id, "close")
		c.shutdown()
	}()

	// приветствие mpd форвардим как есть — на версии могут сидеть
	// проверки совместимости клиентов
	c.bmu.Lock()
	greeting, err := c.ber.ReadString('\n')
	c.bmu.Unlock()
	if err != nil {
		return
	}
	if _, err := c.clw.WriteString(greeting); err != nil {
		return
	}
	c.clw.Flush()

	go c.pingLoop()

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
	clr *bufio.Reader // читает клиентские команды (только dispatch-горутина)
	clw *bufio.Writer // пишет клиенту (wmu)
	be  net.Conn
	ber *bufio.Reader // читает ответы mpd (bmu)
	bew *bufio.Writer // пишет mpd (bmu)

	wmu sync.Mutex // сериализация записей клиенту (ответы + idle-нотификации)
	bmu sync.Mutex // сериализация пар «команда→ответ» к mpd (ping ходит параллельно)

	mu          sync.Mutex
	idle        bool            // клиент сидит в idle (ждёт changed:...)
	idleFilter  map[string]bool // nil = все подсистемы
	shutdownOne sync.Once
}

// dispatch — одна команда клиента. false = закрыть соединение.
func (c *pconn) dispatch(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return true
	}
	cmd := strings.ToLower(fields[0])
	// Любая команда кроме noidle на idle-соединении молча снимает idle
	// (семантика mpd: ответ idle теряется, идёт ответ команды).
	c.mu.Lock()
	wasIdle := c.idle
	if c.idle && cmd != "noidle" {
		c.idle = false
		pdebug(c.id, "idle отменён командой %q", cmd)
	}
	c.mu.Unlock()
	pdebug(c.id, "cmd %q (idle был: %v)", line, wasIdle)

	switch cmd {
	case "setvol":
		// libmpdclient цитирует аргумент: на проводе «setvol "85"»
		// (mpd-парсер кавычки легально снимает; у нас было источником
		// ACK-ошибки, отравлявшей соединение upmpdcli)
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
		// deprecated-команда относительной громкости: «+10»/«-10»/«55»
		// (возможно в кавычках — см. setvol). Не перехватывать нельзя —
		// mixer none у mpd ответил бы ACK.
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
	case "idle":
		// Не блокируем чтение: idle «висит» просто потому, что мы не
		// отвечаем, — до NotifyVolume/noidle/чужой команды.
		c.mu.Lock()
		c.idle = true
		if len(fields) > 1 {
			c.idleFilter = make(map[string]bool, len(fields)-1)
			for _, sub := range fields[1:] {
				c.idleFilter[sub] = true
			}
		} else {
			c.idleFilter = nil
		}
		c.mu.Unlock()
		pdebug(c.id, "idle ждёт, фильтр %v", fields[1:])
	case "noidle":
		// отмена ожидающего idle — пустой OK (иначе eventloop клиента висит)
		c.mu.Lock()
		c.idle = false
		c.mu.Unlock()
		c.writeLines("OK\n")
	case "status":
		lines, err := c.backendExchange(line)
		if err != nil {
			return false
		}
		// mixer none: mpd 0.23 отдавал «volume: -1», 0.24 строку ВООБЩЕ
		// опускает. Переписываем найденную, при отсутствии — вставляем
		// первой (volume — первая строка статуса mpd): без неё upmpdcli
		// не видит громкости и контроллерам раздаётся пустота
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
	case "close":
		c.backendWrite(line) // mpd закрывает соединение без ответа
		return false
	default:
		// прозрачный форвард; command_list_* тоже летит насквозь —
		// upmpdcli списков не шлёт, а перехват внутри списка сломал бы
		// соответствие команды и list_OK
		if cmd == "command_list_begin" {
			log.Printf("mpdproxy: command_list от клиента — форвард без перехвата (setvol внутри списка с mixer none даст ACK)")
		}
		lines, err := c.backendExchange(line)
		if err != nil {
			return false
		}
		c.writeLines(lines...)
	}
	return true
}

// notifyMixer — синтетический «changed: mixer» ожидающему idle-соединению
// (если оно подписано на mixer). Вызывается под p.mu.
func (c *pconn) notifyMixer() {
	c.mu.Lock()
	if !c.idle || (c.idleFilter != nil && !c.idleFilter["mixer"]) {
		c.mu.Unlock()
		return
	}
	c.idle = false
	c.mu.Unlock()
	pdebug(c.id, "notify changed: mixer")
	c.writeLines("changed: mixer\n", "OK\n")
}

// backendExchange — команда к mpd и сбор ответа до OK/ACK. Вызывается
// только из dispatch-горутины; bmu защищает от параллельного ping.
func (c *pconn) backendExchange(line string) ([]string, error) {
	c.bmu.Lock()
	defer c.bmu.Unlock()
	if err := c.backendWriteLocked(line); err != nil {
		pdebug(c.id, "backend write err: %v", err)
		return nil, err
	}
	lines, err := c.readResponseLocked()
	if err != nil {
		pdebug(c.id, "backend read err: %v", err)
	} else if debug {
		joined := strings.TrimSuffix(strings.Join(lines, ""), "\n")
		pdebug(c.id, "backend ответ: %q", strings.ReplaceAll(joined, "\n", " | "))
	}
	return lines, err
}

// backendWrite — отправка без чтения ответа (close).
func (c *pconn) backendWrite(line string) {
	c.bmu.Lock()
	c.backendWriteLocked(line)
	c.bmu.Unlock()
}

func (c *pconn) backendWriteLocked(line string) error {
	c.be.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer c.be.SetWriteDeadline(time.Time{})
	if _, err := c.bew.WriteString(line); err != nil {
		return err
	}
	if err := c.bew.WriteByte('\n'); err != nil {
		return err
	}
	if err := c.bew.Flush(); err != nil {
		return err
	}
	return nil
}

// readResponseLocked — строки ответа mpd до «OK»/«ACK …». Встреченный
// «binary: N» протаскивает N байт payload + перевод строки как есть
// (binary-ответы — albumart/readcomments; построчный разбор там сломался бы).
func (c *pconn) readResponseLocked() ([]string, error) {
	var lines []string
	for {
		c.be.SetReadDeadline(time.Now().Add(10 * time.Second))
		l, err := c.ber.ReadString('\n')
		if err != nil {
			return nil, err
		}
		c.be.SetReadDeadline(time.Time{})
		if strings.HasPrefix(l, "binary: ") {
			n, perr := strconv.Atoi(strings.TrimSpace(l[len("binary: "):]))
			if perr != nil {
				return nil, fmt.Errorf("binary: %v", perr)
			}
			buf := make([]byte, n+1) // payload + '\n'
			if _, err := io.ReadFull(c.ber, buf); err != nil {
				return nil, err
			}
			lines = append(lines, l, string(buf))
			continue
		}
		lines = append(lines, l)
		if l == "OK\n" || strings.HasPrefix(l, "ACK ") {
			return lines, nil
		}
	}
}

// pingLoop — keepalive бэкенда для соединений в idle: пока клиент сидит
// в idle, к mpd не уходит ни одной команды, и connection_timeout mpd
// закрыл бы бэкенд-соединение. Пропускает такты, когда клиент уже не в
// idle (там команды ходят сами).
func (c *pconn) pingLoop() {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for range t.C {
		c.mu.Lock()
		idle := c.idle
		c.mu.Unlock()
		if !idle {
			continue
		}
		pdebug(c.id, "ping бэкенду (клиент в idle)")
		c.bmu.Lock()
		if err := c.backendWriteLocked("ping"); err != nil {
			c.bmu.Unlock()
			c.shutdown()
			return
		}
		// съесть ответ («OK»), иначе он встанет в очередь перед
		// ответом следующей команды
		c.be.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			l, err := c.ber.ReadString('\n')
			if err != nil {
				c.be.SetReadDeadline(time.Time{})
				c.bmu.Unlock()
				c.shutdown()
				return
			}
			if l == "OK\n" || strings.HasPrefix(l, "ACK ") {
				break
			}
		}
		c.be.SetReadDeadline(time.Time{})
		c.bmu.Unlock()
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
