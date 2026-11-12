// marusia-d — демон периферии колонки VK Mini (Маруся):
// световое кольцо (SPI), сенсорная панель (evdev), панель-часы (seg-led),
// датчик света (IIO), состояние музыки — от MPD, диалог ассистента — от LVA.
//
// Источники событий → state machine → рендер:
//
//	mpd watcher (player)      → play/pause кнопкой, лица на панели, репорт
//	                           media_player_state в LVA; кольцо от mpd не светится
//	LVA peripheral API (ws)   → диалог: wake/listening/thinking/speaking/muted/error/noconn;
//	                           внешний медиа-плеер: media_player_* → mpd
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
	VOLUME_VISUAL_MAX = 100                     // визуальная шкала (кольцо, панель): 0..100
)

// mpdLink — переподключающийся клиент MPD. conn и watcher живут в своей
// горутине; наружу — Serve, команды воспроизведения (togglePause/playURLs/
// pause/resume/stop) и statesCh. lastState — последнее известное состояние
// плеера (для репорта новому LVA-коннекту); писатель один — Serve.
type mpdLink struct {
	addr      string
	mu        sync.Mutex
	conn      *mpd.Client
	lastState string
	statesCh  chan string
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
			m.setState(st["state"])
		}
		m.watch(ctx, c)
		m.mu.Lock()
		m.conn = nil
		m.mu.Unlock()
		c.Close()
		m.setState("stop") // нет связи с mpd — кольцо гасим
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

// setState — положить состояние плеера в канал и запомнить его. Писатель
// один — Serve/watch; читатели — main-цикл (репорт media_player_state) и
// onConnect (начальный репорт при новом LVA-коннекте).
func (m *mpdLink) setState(s string) {
	m.mu.Lock()
	m.lastState = s
	m.mu.Unlock()
	m.statesCh <- s
}

// state — последнее известное состояние плеера mpd.
func (m *mpdLink) state() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastState
}

// playURLs — HA запустил медиа: очередь заменяется URL(ами) из команды
// (media_url приходит строкой или списком), играем последний добавленный.
func (m *mpdLink) playURLs(urls []string) {
	m.mu.Lock()
	c := m.conn
	m.mu.Unlock()
	if c == nil || len(urls) == 0 {
		return
	}
	c.Clear()
	last := -1
	for _, u := range urls {
		if id, err := c.AddID(u, -1); err == nil {
			last = id
		}
	}
	if last >= 0 {
		c.PlayID(last)
	}
}

// pause — пауза воспроизведения (инструкция media_player_pause).
func (m *mpdLink) pause() {
	m.mu.Lock()
	c := m.conn
	m.mu.Unlock()
	if c == nil {
		return
	}
	c.Pause(true)
}

// resume — продолжить воспроизведение (инструкция media_player_resume).
func (m *mpdLink) resume() {
	m.mu.Lock()
	c := m.conn
	m.mu.Unlock()
	if c == nil {
		return
	}
	c.Pause(false)
}

// stop — остановить воспроизведение (инструкция media_player_stop).
func (m *mpdLink) stop() {
	m.mu.Lock()
	c := m.conn
	m.mu.Unlock()
	if c == nil {
		return
	}
	c.Stop()
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
					m.setState(st["state"])
				}
			}
		case <-ping.C:
			if err := c.Ping(); err != nil {
				return
			}
		}
	}
}

// Названия эффектов кольца, видимые в HA (регистрация в RegisterLight и
// ветвление в light_command должны совпадать).
const (
	HA_EFFECT_COLOR = "Цвет"      // просто цвет (solid)
	HA_EFFECT_PULSE = "Пульсация" // дыхание цветом из HA
	HA_EFFECT_SPIN  = "Вращение"  // бегущий диод цветом из HA
	HA_EFFECT_FIRE  = "Огонь"     // мерцание цветом из HA
)

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
	// Клиент существует ВСЕГДА — инвариант «lvaClient не бывает nil»:
	// общие ветки (репорты volume_changed / media_player_state) зовут
	// Send безусловно. Под условием только запуск сервера: при -lva ""
	// он не стартует, conn вечно nil — Send молча глотает, Connected()
	// = false, репорты уходят в никуда.
	var lvaEventsCh <-chan lva.Event
	var lvaClient *lva.Client
	onConnect := func(c *lva.Client) {
		if *lvaLight {
			c.RegisterLight("Кольцо", "ring", []string{HA_EFFECT_COLOR, HA_EFFECT_PULSE, HA_EFFECT_SPIN, HA_EFFECT_FIRE})
		}
		// Колонка — внешний медиа-плеер: HA управляет воспроизведением
		// через демон (инструкции media_player_*), демон репортит правду
		// (media_player_state / volume_changed). Регистрация умирает
		// вместе с коннектом — при каждом коннекте повторяем.
		c.Send("register_media_player", nil)
		// Начальный репорт: новый LVA не знает состояние нашего mpd
		switch mpdLink.state() {
		case "play":
			c.Send("media_player_state", map[string]string{"state": "playing"})
		case "pause":
			c.Send("media_player_state", map[string]string{"state": "paused"})
		}
	}
	srv := lva.New(*lvaAddr, onConnect)
	lvaEventsCh, lvaClient = srv.Events(), srv.Client()
	if *lvaAddr != "" {
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
					// (кламп внутри), отклик — от итогового значения
					p := fromVisual(v)
					setSinkPct(p)
					volFeedback(toVisual(p))
				default:
					log.Printf("volapi: неизвестная команда %q", fields[0])
				}
			}(conn)
		}
	}(*volapiAddr)
	// reportVolume — репорт реальной громкости в LVA (команда volume_changed):
	// v — уже переведённая ВИЗУАЛЬНАЯ громкость 0..VOLUME_VISUAL_MAX, уходит
	// долей 0..1. LVA синкает HA-сущность, репорт обратно НЕ переизлучается
	// (разрыв петли).
	reportVolume := func(v int) {
		lvaClient.Send("volume_changed", map[string]any{
			"volume": float64(v) / VOLUME_VISUAL_MAX,
		})
	}
	// Громкость клавиш — всегда локальный шаг: демон владеет физической
	// громкостью sink (get → snap → ±10 → кламп → set). Команд volume_up/down
	// в протоколе больше нет — LVA лишь принимает репорт volume_changed.
	// LVA ничего не отвечает на репорт, поэтому отклик (звук+вспышка+панель)
	// рисуем сразу, без ожидания.
	volStep := func(up bool) {
		pct := getUserPct() // во время duck — пользовательская, не приглушённый sink
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
		setSinkPct(pct)
		v := toVisual(pct)
		volFeedback(v)
		reportVolume(v)
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
	lastReported := "" // последнее состояние плеера, отрепорченное в LVA
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
			// Репорт внешнему плееру: HA-сущность должна знать правду о
			// воспроизведении (mpdLink.Serve шлёт и при потере связи —
			// «stop» = idle). Дедуп по последнему отрепорченному.
			if s := map[string]string{"play": "playing", "pause": "paused", "stop": "idle"}[state]; s != "" && s != lastReported {
				lastReported = s
				lvaClient.Send("media_player_state", map[string]string{"state": s})
			}
		case e, ok := <-lvaEventsCh:
			if !ok {
				lvaEventsCh = nil // поток закрыт
				continue
			}
			switch e.Event {
			case lva.EvDown:
				rg.SetLive(false)
				// LVA отвалил — duck снять: иначе музыка останется
				// приглушённой до следующего диалога
				setDucked(false)
			case "snapshot":
				rg.SetSnapshot(e.Bool("muted"))
				if e.Has("volume") {
					// volume — доля 0.0..1.0 от максимума колонки (150%),
					// ×100 — это визуальные 0..100 (шкала toVisual);
					// sink — источник истины по громкости колонки,
					// синхронизируем при коннекте: fromVisual — сразу
					// физическая шкала на сетке инкремента клавиш
					setSinkPct(fromVisual(int(math.Round(e.Float("volume") * VOLUME_VISUAL_MAX))))
				}
				if e.Has("ducked") {
					// duck_changed — state-событие, LVA реплеит его в
					// snapshot: после реконнекта демон сразу знает
					// актуальное состояние duck (satellite.py duck()/unduck())
					setDucked(e.Bool("ducked"))
				}

			case "duck_changed":
				// LVA приглушил/вернул музыку плеера на время диалога:
				// master ×DUCK_PCT%, TTS (роль tts) не трогается
				setDucked(e.Bool("ducked"))

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

			case "media_player_set_volume":
				// HA слайдер/мьют внешнему плееру: та же шкала, что у
				// volume_changed; применили sink — отрепортим правду
				pct := fromVisual(int(math.Round(e.Float("volume") * VOLUME_VISUAL_MAX)))
				setSinkPct(pct)
				v := toVisual(pct)
				volFeedback(v)
				reportVolume(v)

			case "media_player_play":
				// HA запустил медиа: очередь mpd заменяется URL из команды
				// (media_url — строка или список); анонсы играются как обычное
				// media, отдельной хореографии поверх трека нет
				mpdLink.playURLs(e.StrOrList("media_url"))

			case "media_player_pause":
				mpdLink.pause()

			case "media_player_resume":
				mpdLink.resume()

			case "media_player_stop":
				mpdLink.stop()

			case "media_player_playing", "media_player_paused":
				// эхо наших же репортов: emit_event шлёт событие всем клиентам,
				// плееру тоже; истину о состоянии демон знает от mpd напрямую

			case "light_command":
				lc := e.LightCommand()
				if lc.ObjectID != "ring" {
					continue
				}
				// HA держит выбранный эффект и шлёт его В КАЖДОЙ команде,
				// в т.ч. при смене цвета (проверено payload-щупом
				// 2026-11-11). Ветвимся по эффекту + state:
				//   вкл + Цвет      → solid (rgb×brightness);
				//   вкл + Пульсация → ha_pulse (дыхание цветом из HA);
				//   вкл + Вращение  → ha_spin  (бегущий диод, фон 30%);
				//   вкл + Огонь     → ha_fire  (независимое мерцание);
				//   выкл → вернуть кольцо демону (диалоговые сцены).
				if lc.State {
					k := lc.Brightness
					col := ring.Color{
						R: uint8(lc.Red * 255 * k),
						G: uint8(lc.Green * 255 * k),
						B: uint8(lc.Blue * 255 * k),
					}
					switch lc.Effect {
					case HA_EFFECT_PULSE:
						rg.SetHA(ring.HAPulse, col)
					case HA_EFFECT_SPIN:
						rg.SetHA(ring.HASpin, col)
					case HA_EFFECT_FIRE:
						rg.SetHA(ring.HAFire, col)
					default: // Цвет и всё незнакомое — просто цвет
						rg.SetHA("", col)
					}
				} else {
					// HA выключил сущность — кольцо снова живёт само
					rg.SetHAOff()
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

// duck — событие duck_changed от LVA (фork r4650g): на время диалога
// (wake/thinking/speaking) музыка плеера приглушается, по концу TTS
// возвращается. Плеер колонки — mpd → sink master (mpd.conf), TTS играет
// в соседнюю роль tts (lva.pa) и от master'а не зависит, поэтому гасим
// сам master (= @DEFAULT_SINK@) в DUCK_PCT% от пользовательской громкости.
// duck_changed реплеится в snapshot при реконнекте (state-событие).
const DUCK_PCT = 50

// duckState — состояние duck. userPct — пользовательская громкость master
// (0..VOLUME_MAX_PCT, -1 = не узнали): при duck в sink уходит ×DUCK_PCT/100,
// при unduck — полная. Мьютекс: пишут и главный цикл (LVA-события), и
// volapi-горутина (DLNA-громкость) через setSinkPct.
var duckState = struct {
	sync.Mutex
	ducked  bool
	userPct int
}{userPct: -1}

// setDucked — вход/выход из duck. При входе текущая громкость (ещё полная)
// запоминается как пользовательская, в sink уходит её DUCK_PCT%; при выходе
// пользовательская возвращается целиком — даже если во время duck громкость
// меняли (setSinkPct всё это время писал приглушённую и помнил полную).
func setDucked(d bool) {
	duckState.Lock()
	if duckState.ducked == d {
		duckState.Unlock()
		return
	}
	duckState.ducked = d
	user := duckState.userPct
	duckState.Unlock()

	if !d {
		if user >= 0 {
			writeSinkPct(user)
		}
		return
	}
	cur := getSinkPct() // ещё не задакано — это пользовательская громкость
	if cur < 0 {
		return // PA не ответил: duck останется чистым состоянием до setSinkPct
	}
	duckState.Lock()
	duckState.userPct = cur
	duckState.Unlock()
	writeSinkPct(cur * DUCK_PCT / 100)
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

// setSinkPct — единственная точка установки ПОЛЬЗОВАТЕЛЬСКОЙ громкости
// master (0..VOLUME_MAX_PCT, кламп внутри). Запоминает её как userPct, а в
// sink пишет с duck-фактором: во время диалога музыка приглушена, но
// пользовательская громкость не теряется и вернётся при unduck.
func setSinkPct(p int) {
	if p < 0 {
		p = 0
	}
	if p > VOLUME_MAX_PCT {
		p = VOLUME_MAX_PCT
	}
	duckState.Lock()
	duckState.userPct = p
	if duckState.ducked {
		p = p * DUCK_PCT / 100
	}
	duckState.Unlock()
	writeSinkPct(p)
}

// getUserPct — громкость, от которой шагают кнопки: во время duck sink
// держит приглушённое значение (×DUCK_PCT), а шагать и запоминать надо
// полную пользовательскую (duckState.userPct), иначе база шага — половина,
// и каждое нажатие занижает сохранённую громкость вдвое. -1 = не узнали.
func getUserPct() int {
	duckState.Lock()
	defer duckState.Unlock()
	if duckState.ducked {
		return duckState.userPct
	}
	return getSinkPct()
}

// writeSinkPct — голая запись физической громкости sink 0..VOLUME_MAX_PCT.
// Вызывают только setSinkPct (пользовательская, с duck-фактором) и
// setDucked (переходы duck).
func writeSinkPct(p int) {
	pactl("set-sink-volume", "@DEFAULT_SINK@", fmt.Sprintf("%d%%", p))
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
