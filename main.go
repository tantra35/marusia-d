// marusia-d — демон периферии колонки VK Mini (Маруся):
// световое кольцо (SPI), сенсорная панель (evdev), панель-часы (seg-led),
// датчик света (IIO), состояние музыки — от MPD, диалог ассистента — от LVA.
//
// Источники событий → state machine → рендер:
//
//	mpd watcher (player)      → play/pause кнопкой, лица на панели; кольцо от mpd не светится
//	LVA peripheral API (ws)   → диалог: wake/listening/thinking/speaking/muted/error/noconn
//	touch /dev/input/event0   → громкость ±10, play/pause, start_listening, mute
//	light bh1730 (2с)         → глобальная яркость кольца
//	clock                     → время ЧЧ:ММ, лица на события
//
// Приоритет сцен кольца:
//
//	solid (HA) > error > noconn > muted > диалог LVA > сцена MPD
//
// Ни один из источников не фатален: mpd и LVA переподключаются сами.
//
// Сборка: CGO_ENABLED=0 GOARCH=arm64 go build -o marusia-d .
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"marusia-d/internal/clock"
	"marusia-d/internal/light"
	"marusia-d/internal/lva"
	"marusia-d/internal/ring"
	"marusia-d/internal/touch"

	mpd "github.com/fhs/gompd/v2/mpd"
)

// keycode'ы из DT keys@15: <0x72 0xF8 0x73 0x94 0x246>
// Назначения сверены на устройстве (evtest), кроме 582.
const (
	KEY_VOLDOWN = 114 // 0x72  KEY_VOLUMEDOWN
	KEY_MUTE    = 248 // 0xF8  mute микрофона (сверено)
	KEY_VOLUP   = 115 // 0x73  KEY_VOLUMEUP
	KEY_CENTER  = 148 // 0x94  центр сенсорного круга → play/pause (сверено)
	KEY_582     = 582 // 0x246 — предположительно кнопка ассистента — TBD
)

// звук отклика на кнопки громкости (кладка в rootfs: /etc/sounds)
const (
	SOUND_VOLUME      = "/etc/sounds/13_volume_voice.flac"
	FLASH_MS          = 800 * time.Millisecond  // вспышка громкости: дыхание 0.8 с
	ERROR_FLASH_MS    = 1600 * time.Millisecond // красная вспышка ошибки пайплайна
	WAKE_FLASH_MS     = 800 * time.Millisecond  // вспышка wake: синий импульс, как у громкости
	VOLUME_MAX_PCT    = 150                     // физическая громкость sink, больше нельзя
	VOLUME_STEP_PCT   = 10                      // инкремент клавиш громкости — сетка громкости кратна ему
	VOLUME_VISUAL_MAX = 100                   // визуальная шкала (кольцо, панель): 0..100
	SINK_TTS          = "tts"                 // роль-сосед master'а в lva.pa: громкость LVA живёт здесь
)

// mpdLink — переподключающийся клиент MPD. conn и watcher живут в своей
// горутине; наружу — Serve, togglePause и statesCh.
type mpdLink struct {
	addr     string
	mu       sync.Mutex
	conn     *mpd.Client
	statesCh chan string
}

// Serve — цикл переподключения к mpd: коннект → статус/watch → при обрыве
// повтор через 5 с.
func (m *mpdLink) Serve(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		c, err := mpd.Dial("tcp", m.addr)
		if err != nil {
			log.Printf("mpd: %v (повтор через 5с)", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		m.mu.Lock()
		m.conn = c
		m.mu.Unlock()
		log.Printf("mpd: подключено к %s", m.addr)
		if st, err := c.Status(); err == nil {
			m.statesCh <- st["state"]
		}
		m.watch(ctx, c)
		m.mu.Lock()
		m.conn = nil
		m.mu.Unlock()
		c.Close()
		m.statesCh <- "stop" // нет связи с mpd — кольцо гасим
		log.Printf("mpd: связь потеряна (повтор через 5с)")
		sleepCtx(ctx, 5*time.Second)
	}
}

// togglePause — play/pause: играет → пауза, на паузе → продолжить.
// Соединение копируем под локом (его может закрыть Serve), сетевые вызовы — без лока.
func (m *mpdLink) togglePause() {
	m.mu.Lock()
	c := m.conn
	m.mu.Unlock()
	if c == nil {
		return
	}
	st, err := c.Status()
	if err != nil {
		return
	}
	c.Pause(st["state"] == "play")
}

func (m *mpdLink) watch(ctx context.Context, c *mpd.Client) {
	w, err := mpd.NewWatcher("tcp", m.addr, "", "player")
	if err != nil {
		log.Printf("mpd watcher: %v", err)
		return
	}
	defer w.Close()
	ping := time.NewTicker(2 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-w.Error:
			log.Printf("mpd watcher: %v", err)
			return
		case ev := <-w.Event:
			switch ev {
			case "player":
				if st, err := c.Status(); err == nil {
					m.statesCh <- st["state"]
				}
			}
		case <-ping.C:
			if err := c.Ping(); err != nil {
				return
			}
		}
	}
}

func main() {
	mpdAddr := flag.String("mpd", "127.0.0.1:6600", "адрес MPD")
	lvaAddr := flag.String("lva", ":6055", "слушать LVA peripheral API на этом адресе; пусто = выключено")
	lvaLight := flag.Bool("lva-light", true, "регистрировать кольцо как light-сущность HA")
	volapiAddr := flag.String("volapi", "127.0.0.1:6056", "слушать volume-API (upmpdcli externalvolumecontrol) на этом адресе; пусто = выключено")
	flag.Parse()

	log.SetFlags(log.Ltime)
	log.Printf("marusia-d старт: mpd=%s lva=%q volapi=%q", *mpdAddr, *lvaAddr, *volapiAddr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()

	// ---------- кольцо ----------
	rg := ring.New(ring.DefaultDevice, 12)
	go rg.Serve(ctx)

	// ---------- панель-часы ----------
	var clk *clock.Clock
	c, err := clock.Open()
	if err != nil {
		log.Printf("clock: %v — панель-часы выключена", err)
	} else {
		clk = c
		go clk.Serve(ctx) // время и возврат после лиц — целиком внутри Clock
	}

	// ---------- датчик освещенности ----------
	go func() {
		var ema float64 = -1
		bright, cur := -1, -1
		for lux := range light.Start(ctx, 2*time.Second) {
			if ema < 0 {
				ema = lux
			}
			ema = 0.7*ema + 0.3*lux
			rg.SetScale(light.Scale(ema))
			if clk != nil {
				// яркость панели — от того же датчика; пишем только при изменении
				if b := light.ClockPWM(ema); b != bright {
					bright = b
					clk.SetBrightness(byte(b))
				}
				if c := light.ClockCurrent(ema); c != cur {
					cur = c
					clk.SetCurrent(c)
				}
			}
		}
	}()

	// ---------- mpd ----------
	mpdLink := &mpdLink{addr: *mpdAddr, statesCh: make(chan string, 8)}
	go mpdLink.Serve(ctx)

	// ---------- LVA (периферийный API) ----------
	var lvaClient *lva.Client
	var lvaEventsCh <-chan lva.Event
	if *lvaAddr != "" {
		onConnect := func(c *lva.Client) {
			if *lvaLight {
				c.RegisterLight("Кольцо", "ring", []string{"Voice Assistant"})
			}
		}
		srv := lva.New(*lvaAddr, onConnect)
		lvaEventsCh, lvaClient = srv.Events(), srv.Client()
		go srv.Serve(ctx)
	}

	// ---------- сенсорная панель ----------
	// громкость — СИСТЕМНАЯ (sink PulseAudio), а не mpd: через sink проходит
	// всё — музыка mpd, служебные звуки, голос ассистента.
	// getSinkPct/setSinkPct — статические, рядом с pactl-хелперами внизу.
	// volFeedback — отклик на смену громкости: звук, вспышка всего кольца
	// одним цветом уровня, проценты на панели-часах. Звук ПЕРВЫМ: Flash
	// синхронен до первого кадра (rebind), спад дорисовывается в рендере —
	// paplay после Flash отставал бы от старта вспышки. pct — ВИЗУАЛЬНАЯ
	// громкость 0..100;
	volFeedback := func(pct int) {
		playSound(SOUND_VOLUME)
		rg.Flash(volumeColor(pct), FLASH_MS)
		if clk != nil && pct >= 0 {
			clk.ShowVolume(pct)
		}
	}

	// ---------- volume API для upmpdcli (externalvolumecontrol) ----------
	// mpd при mixer_type none громкость не регулирует: DLNA-слайдер доезжает
	// через хуки upmpdcli (getexternalvolume/onvolumechange — dumb-пайпы
	// printf | nc на этот listener). Протокол построчный:
	//   GET          → визуальные 0..100 (текущий sink);
	//   SET <0..100> → sink = round(v*1.5), отклик кольцо/панель/звук.
	// Шкала: визуальная 0..VOLUME_VISUAL_MAX ↔ физическая 0..VOLUME_MAX_PCT.
	go func(addr string) {
		if addr == "" {
			return
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Printf("volapi: %v — DLNA-громкость через upmpdcli не работает", err)
			return
		}
		go func() {
			<-ctx.Done()
			ln.Close()
		}()
		log.Printf("volapi: слушаю %s", addr)
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("volapi: accept: %v", err)
				continue
			}
			go func(conn net.Conn) {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(2 * time.Second))
				sc := bufio.NewScanner(conn)
				if !sc.Scan() {
					return
				}
				fields := strings.Fields(strings.TrimSpace(sc.Text()))
				if len(fields) == 0 {
					return
				}
				switch fields[0] {
				case "GET":
					p := getSinkPct()
					if p < 0 {
						p = 0
					}
					if p > VOLUME_MAX_PCT {
						p = VOLUME_MAX_PCT
					}
					fmt.Fprintf(conn, "%d\n", toVisual(p))
				case "SET":
					if len(fields) < 2 {
						return
					}
					v, err := strconv.Atoi(fields[1])
					if err != nil || v < 0 {
						log.Printf("volapi: SET %q: не число", fields[1])
						return
					}
					// v∈0..100: fromVisual — на сетку инкремента клавиш
					// setSinkPct сам режет в 0..VOLUME_MAX_PCT
					// (кламп внутри), отклик — от итогового значения
					p := fromVisual(v)
					setSinkPct("@DEFAULT_SINK@", p)
					volFeedback(toVisual(p))
				default:
					log.Printf("volapi: неизвестная команда %q", fields[0])
				}
			}(conn)
		}
	}(*volapiAddr)
	// up=true → KEY_VOLUP, up=false → KEY_VOLDOWN.
	// LVA подключен: команды volume_up/volume_down — шаг знает только LVA
	// (5% по умолчанию, --peripheral-volume-step), результат прилетит
	// volume_changed'ом. Автономно: локальный pactl, шаг наш собственный.
	volStep := func(up bool) {
		if lvaClient.Connected() {
			if up {
				lvaClient.Send("volume_up", nil)
			} else {
				lvaClient.Send("volume_down", nil)
			}
			return
		}
		// локальная ветка: шаг ±VOLUME_STEP_PCT считает сам демон — один get +
		// один set (setSinkPct сам режет в 0..VOLUME_MAX_PCT). get вернул -1 =
		// PA не ответил: и set упадёт так же (тот же pactl, тот же сокет),
		// не шагаем.
		pct := getSinkPct()
		if pct < 0 {
			return
		}
		pct = snapPct(pct) // чужая установка могла оставить значение мимо сетки
		if up {
			pct += VOLUME_STEP_PCT
		} else {
			pct -= VOLUME_STEP_PCT
		}
		if pct > VOLUME_MAX_PCT { // 145+10=155 → потолок
			pct = VOLUME_MAX_PCT
		} else if pct < 0 { // 5-10=-5 → пол
			pct = 0
		}
		setSinkPct("@DEFAULT_SINK@", pct)
		volFeedback(toVisual(pct))
	}
	// keyCh — ЕДИНЫЙ небуферизированный канал действий с клавиатуры.
	// Производитель — touch-горутина; потребитель — ровно один (ниже):
	// вся обработка нажатий сериализована, параллельных volStep/pactl/
	// команд в LVA не бывает. Если потребитель занят предыдущим нажатием,
	// новое дропается (select/default) — очередь не копится.
	keyCh := make(chan uint16)

	// потребитель: единственная точка исполнения действий с клавиатуры
	go func() {
		for code := range keyCh {
			switch code {
			case KEY_VOLUP:
				log.Printf("touch: KEY_VOLUP")
				volStep(true)
			case KEY_VOLDOWN:
				log.Printf("touch: KEY_VOLDOWN")
				volStep(false)
			case KEY_CENTER: // центр сенсорного круга — play/pause
				mpdLink.togglePause()
			case KEY_582: // кнопка ассистента — старт диалога в LVA
				if lvaClient.Connected() {
					lvaClient.Send("start_listening", nil)
				}
			case KEY_MUTE: // mute микрофона — команда в LVA
				if lvaClient.Connected() {
					if rg.Muted() {
						lvaClient.Send("unmute_mic", nil)
					} else {
						lvaClient.Send("mute_mic", nil)
					}
				}
			default:
				log.Printf("touch: keycode %d (без действия)", code)
			}
		}
	}()

	// производитель: тач-события → в канал, лишние — дроп
	go func() {
		defer close(keyCh)
		for code := range touch.Start(ctx, "/dev/input/event0") {
			select {
			case keyCh <- code:
			default:
				log.Printf("touch: keycode %d дропнут — потребитель занят", code)
			}
		}
	}()

	// ---------- главный цикл ----------
	for {
		select {
		case <-ctx.Done():
			log.Printf("marusia-d стоп") // панель и кольцо гасят свои акторы
			return
		case state := <-mpdLink.statesCh:
			// Кольцо от mpd не светится (по просьбе: радуга мешала).
			// Состояние mpd нужно только для play/pause кнопкой и лиц на панели.
			if clk != nil {
				switch state {
				case "play":
					clk.PlayFaces([]string{"sleepy", "curious", "surprised"})
				case "pause":
					clk.PlayFaces([]string{"surprised", "curious", "sleepy"})
				}
			}
			log.Printf("mpd: %s", state)
		case e, ok := <-lvaEventsCh:
			if !ok {
				lvaEventsCh = nil // поток закрыт
				continue
			}
			switch e.Event {
			case lva.EvDown:
				rg.SetLive(false)
			case "snapshot":
				rg.SetSnapshot(e.Bool("muted"))
				if e.Has("volume") {
					// volume — доля 0.0..1.0 от максимума колонки (150%);
					// громкость LVA — это роль tts (звук ассистента),
					// sync при коннекте идёт В неё, master (музыка)
					// не трогается; fromVisual — сразу физическая шкала
					// на сетке инкремента клавиш
					setSinkPct(SINK_TTS, fromVisual(int(math.Round(e.Float("volume")*VOLUME_VISUAL_MAX))))
				}
			case "zeroconf":
				// upstream: status "getting_started" | "connected"
				// (peripheral_api.md:127) — connected снимает noconn
				rg.SetLive(e.Str("status") == "connected")

			case "muted":
				// звук мьюта играет LVA сам (satellite.py:557-572: _set_muted
				// — общая воронка и для кнопки, и для тумблера HA), демону
				// остаётся сцена кольца
				newMuted := e.Bool("muted")
				rg.SetMuted(newMuted)

			case "wake_word_detected":
				// API: chime уже играет, у нас нет wake-анимации — короткая
				// синяя вспышка (peripheral_api.md:111 предлагает анимацию)
				rg.Flash(ring.LISTEN_COLOR, WAKE_FLASH_MS)

			case "listening":
				rg.SetDialog("listening")

			case "thinking":
				rg.SetDialog("thinking")

			case "tts_speaking":
				rg.SetDialog("speaking")

			case "tts_finished", "idle":
				rg.SetDialog("")

			case "pipeline_error":
				// красная вспышка на 1.6 с (overlay поверх любых сцен,
				// спад дорисовывается в рендере); «голос» ошибки играет LVA
				rg.Flash(ring.ERROR_COLOR, ERROR_FLASH_MS)

			case "volume_changed":
				// upstream: {"volume": float} 0.0..1.0 — доля от максимума
				// колонки (150%). Громкость LVA — роль tts (звук ассистента
				// по сети): пишем В неё, master (музыка) живёт своей
				// жизнью; fromVisual — физическая на сетке инкремента
				// клавиш (кламп внутри), в отклик — визуальные 0..100
				pct := fromVisual(int(math.Round(e.Float("volume") * VOLUME_VISUAL_MAX)))
				setSinkPct(SINK_TTS, pct)
				volFeedback(toVisual(pct))

			case "light_command":
				lc := e.LightCommand()
				if lc.ObjectID != "ring" {
					continue
				}
				if lc.Effect == "Voice Assistant" {
					// HA вернул управление демону
					rg.SetHA(false)
				} else {
					k := lc.Brightness
					rg.SetSolid(ring.Color{
						R: uint8(lc.Red * 255 * k),
						G: uint8(lc.Green * 255 * k),
						B: uint8(lc.Blue * 255 * k),
					})
					if !lc.State {
						rg.SetSolid(ring.Off)
					}
					rg.SetHA(true)
				}
			default:
				log.Printf("lva: событие %s (без обработки)", e.Event)
			}
		}
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

// pactl — управление системной громкостью PulseAudio. Сокет задаём явно,
// как и в playSound (в template нет client.conf с default-server).
func pactl(args ...string) {
	cmd := exec.Command("pactl", args...)
	cmd.Env = append(os.Environ(), "PULSE_SERVER=unix:/var/run/pulse/native")
	if err := cmd.Run(); err != nil {
		log.Printf("pactl %v: %v", args, err)
	}
}

// pactlOutput — как pactl, но возвращает stdout (для чтения громкости).
func pactlOutput(args ...string) (string, error) {
	cmd := exec.Command("pactl", args...)
	cmd.Env = append(os.Environ(), "PULSE_SERVER=unix:/var/run/pulse/native")
	out, err := cmd.Output()
	if err != nil {
		log.Printf("pactl %v: %v", args, err)
		return "", err
	}
	return string(out), nil
}

// getSinkPct — текущая физическая громкость default sink в процентах
// 0..VOLUME_MAX_PCT, -1 = не узнали.
func getSinkPct() int {
	out, err := pactlOutput("get-sink-volume", "@DEFAULT_SINK@")
	if err != nil {
		return -1
	}
	for _, f := range strings.Fields(out) {
		if strings.HasSuffix(f, "%") {
			if v, err := strconv.Atoi(strings.TrimSuffix(f, "%")); err == nil {
				return v
			}
		}
	}
	return -1
}

// setSinkPct — абсолютная физическая громкость УКАЗАННОГО sink'а
// 0..VOLUME_MAX_PCT (не доля 0..1!), чужой вход режет сама. Писатели:
// "@DEFAULT_SINK@" (мастер: кнопки/DLNA) и SINK_TTS (громкость LVA).
func setSinkPct(sink string, p int) {
	if p < 0 {
		p = 0
	}
	if p > VOLUME_MAX_PCT {
		p = VOLUME_MAX_PCT
	}
	pactl("set-sink-volume", sink, fmt.Sprintf("%d%%", p))
}

// snapPct — округление громкости к ближайшей ступени сетки VOLUME_STEP_PCT
// (инкремент клавиш) с клампом 0..VOLUME_MAX_PCT. Все писатели sink
// подставляют его: иначе чужая абсолютная установка (слайдер DLNA, volume
// от LVA) оставляет значение мимо сетки, и шаг ±VOLUME_STEP_PCT с клавиш
// уезжает с неё (73+10=83 — дальше кратности 10 уже не будет).
func snapPct(p int) int {
	if p < 0 {
		p = 0
	}
	if p > VOLUME_MAX_PCT {
		p = VOLUME_MAX_PCT
	}
	return (p + VOLUME_STEP_PCT/2) / VOLUME_STEP_PCT * VOLUME_STEP_PCT
}

// fromVisual — громкость в ВИЗУАЛЬНОЙ шкале 0..VOLUME_VISUAL_MAX →
// физическая на сетке клавиш. Округление к ближайшему целочисленным +50
// перед делением (int-вход, деление в Go дробь отбрасывает:
// 47*150/100 = 70, а с +50 честные 70.5 → 71).
func fromVisual(v int) int {
	return snapPct((v*VOLUME_MAX_PCT + 50) / VOLUME_VISUAL_MAX)
}

// toVisual — физическая громкость sink (0..VOLUME_MAX_PCT) → визуальная
// шкала 0..VOLUME_VISUAL_MAX: на кольце и панели громкость всегда 0..100,
// физические 150% показываются как 100. Округление к ближайшему
// (+VOLUME_MAX_PCT/2 — половина знаменателя), НЕ отсечение: иначе
// визуальные значения системно занижаются (56.67 → 56). Отрицательное
// (-1 = «не узнали») проходит насквозь без перевода.
func toVisual(pct int) int {
	if pct < 0 {
		return -1
	}
	return (pct*VOLUME_VISUAL_MAX + VOLUME_MAX_PCT/2) / VOLUME_MAX_PCT
}

// volumeColor — цвет вспышки громкости, двухсегментный градиент по
// ВИЗУАЛЬНОЙ шкале 0..VOLUME_VISUAL_MAX:
//
//	0..половины — синий → зелёный (B убывает, G растёт);
//	половина..максимума — зелёный → оранжевый (R растёт, G спадает до 140).
//
// pct=-1 — уровень не узнали: белая.
func volumeColor(pct int) ring.Color {
	if pct < 0 {
		return ring.White
	}
	t := float64(pct) / float64(VOLUME_VISUAL_MAX)
	if t > 1 {
		t = 1
	}
	var r, g, b float64
	if t <= 0.5 {
		s := t / 0.5
		g = 255 * s
		b = 255 * (1 - s)
	} else {
		s := (t - 0.5) / 0.5
		r = 255 * s
		g = 255 - (255-140)*s // на максимуме оранжевый (255,140,0)
	}
	return ring.Color{R: byte(r), G: byte(g), B: byte(b)}
}

// playSound — короткий сервисный звук через paplay, не ждём окончания.
// PA на колонке в system mode; unix-сокет задаём явно, чтобы не зависеть
// от /etc/pulse/client.conf и рантайм-симлинка в $HOME root.
func playSound(path string) {
	go func() {
		cmd := exec.Command("paplay", path)
		cmd.Env = append(os.Environ(), "PULSE_SERVER=unix:/var/run/pulse/native")
		if err := cmd.Run(); err != nil {
			log.Printf("sound: %s: %v", path, err)
		}
	}()
}
