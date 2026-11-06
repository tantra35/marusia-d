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
	"context"
	"flag"
	"fmt"
	"log"
	"math"
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
	SOUND_VOLUME   = "/etc/sounds/13_volume_voice.flac"
	SOUND_MIC_OFF  = "/etc/sounds/05_mic_on_to_mic_off.flac"
	SOUND_MIC_ON   = "/etc/sounds/06_mic_off_to_mic_on.flac"
	FLASH_MS       = 800 * time.Millisecond // вспышка громкости: дыхание 0.8 с
	VOLUME_MAX_PCT = 150                    // потолок громкости sink, больше нельзя
)

// mpdLink — переподключающийся клиент MPD. conn и watcher живут в своей
// горутине; наружу — Serve, togglePause и statesCh.
type mpdLink struct {
	addr     string
	mu       sync.Mutex
	conn     *mpd.Client
	statesCh chan string
	// onVolume — колбэк «громкость изменилась через API (DLNA/HA/mpd-клиент)»:
	// вотчер ловит idle-событие mixer и передаёт процент 0..100.
	onVolume func(int)
	// lastVol — последняя известная громкость для дедупа: пауза/плей
	// переоткрывают pulse-вывод и дёргают событие mixer без смены громкости.
	// -1 = ещё не известно (первое чтение молча запоминается).
	lastVol int
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
	w, err := mpd.NewWatcher("tcp", m.addr, "", "player", "mixer")
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
			case "mixer":
				// громкость сменилась через API (DLNA/HA → mpd setvol):
				// прокинуть в демона — отклик (вспышка кольца, панель).
				// Sink демон здесь НЕ трогает: масштаб 0..100 → 0..150
				// делает сам mpd (scale_volume в mpd.conf). На паузе/плей
				// pulse-вывод переоткрывается и событие mixer приходит
				// БЕЗ смены громкости (а на паузе volume вообще
				// исчезает из status) — реагируем только на изменение.
				if m.onVolume == nil {
					break
				}
				if st, err := c.Status(); err == nil {
					if v, err := strconv.Atoi(st["volume"]); err == nil && v != m.lastVol {
						first := m.lastVol < 0
						m.lastVol = v
						if !first {
							m.onVolume(v)
						}
					}
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
	flag.Parse()

	log.SetFlags(log.Ltime)
	log.Printf("marusia-d старт: mpd=%s lva=%q", *mpdAddr, *lvaAddr)

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
					if err := clk.SetBrightness(byte(b)); err != nil {
						log.Printf("clock: %v", err)
					}
				}
				if c := light.ClockCurrent(ema); c != cur {
					cur = c
					if err := clk.SetCurrent(c); err != nil {
						log.Printf("clock: %v", err)
					}
				}
			}
		}
	}()

	// ---------- mpd ----------
	mpdLink := &mpdLink{addr: *mpdAddr, statesCh: make(chan string, 8), lastVol: -1}
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
	// getSinkPct — текущая громкость default sink в процентах 0..100, -1 = не узнали.
	getSinkPct := func() int {
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
	// volFeedback — отклик на смену громкости: звук, вспышка всего кольца
	// одним цветом уровня (синий при 0 → оранжевый при 150, breathe-затухание),
	// проценты на панели-часах. Звук ПЕРВЫМ: Flash синхронный — блокирует до
	// конца анимации (~FLASH_MS), иначе paplay отставал бы на вспышку.
	// pct=-1 — громкость не узнали: вспышка белая, панель не трогаем.
	volFeedback := func(pct int) {
		playSound(SOUND_VOLUME)
		rg.Flash(volumeColor(pct), FLASH_MS)
		if clk != nil && pct >= 0 {
			clk.ShowVolume(pct)
		}
	}
	// абсолютная громкость sink строго в процентах 0..100 (не доля 0..1!)
	setSinkPct := func(p int) {
		if p < 0 {
			p = 0
		}
		if p > VOLUME_MAX_PCT {
			p = VOLUME_MAX_PCT
		}
		pactl("set-sink-volume", "@DEFAULT_SINK@", fmt.Sprintf("%d%%", p))
	}
	// громкость через API (DLNA/HA → mpd setvol → событие mixer). Протокол
	// MPD ограничен setvol 0..100, но mpd сам масштабирует свой поток
	// (scale_volume "1.5" в mpd.conf: setvol 100 = 150% на PA). Демон по
	// событию только реагирует — вспышка кольца и проценты на панели
	// в нашей шкале 0..150; sink не трогаем (его двигают кнопки/HA).
	var lastVolNotify time.Time
	mpdLink.onVolume = func(pct int) {
		if time.Since(lastVolNotify) < 700*time.Millisecond {
			return
		}
		lastVolNotify = time.Now()
		volFeedback(pct * VOLUME_MAX_PCT / 100)
	}
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
		// локальная ветка: шаг ±10% с потолком VOLUME_MAX_PCT.
		pct := getSinkPct()
		if pct >= 0 {
			if (up && pct >= VOLUME_MAX_PCT) || (!up && pct <= 0) {
				volFeedback(pct) // на пределе — оранжевый отклик, не шагаем
				return
			}
		}
		if up {
			pactl("set-sink-volume", "@DEFAULT_SINK@", "+10%")
		} else {
			pactl("set-sink-volume", "@DEFAULT_SINK@", "-10%")
		}
		if pct = getSinkPct(); pct > VOLUME_MAX_PCT {
			setSinkPct(VOLUME_MAX_PCT) // PA умеет >150% — режем потолком
			pct = VOLUME_MAX_PCT
		}
		volFeedback(pct)
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
			if clk != nil {
				clk.Off()
			}
			log.Printf("marusia-d стоп")
			return
		case state := <-mpdLink.statesCh:
			// Кольцо от mpd не светится (по просьбе): радуга на play мешает.
			// Состояние mpd нужно только для play/pause кнопкой и лиц на панели.
			rg.SetMPD("off")
			if clk != nil {
				switch state {
				case "play":
					clk.PlayFaces(ctx, []string{"neutral", "curious", "surprised"})
				case "pause":
					clk.PlayFaces(ctx, []string{"sleepy"})
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
					// upstream: volume — доля 0.0..1.0; sink — источник истины
					// по громкости колонки, синхронизируем при коннекте
					setSinkPct(int(math.Round(e.Float("volume") * 100)))
				}
			case "zeroconf":
				if e.Str("status") == "connected" {
					rg.SetLive(true)
				}
			case "muted":
				// звук — на подтверждённую смену состояния (кнопка, голос, HA)
				newMuted := e.Bool("muted")
				if newMuted != rg.Muted() {
					if newMuted {
						playSound(SOUND_MIC_OFF)
					} else {
						playSound(SOUND_MIC_ON)
					}
				}
				rg.SetMuted(newMuted)
			case "wake_word_detected":
				rg.SetDialog("wake")
			case "listening":
				rg.SetDialog("listening")
				if clk != nil {
					clk.PlayFaces(ctx, []string{"curious"})
				}
			case "thinking":
				rg.SetDialog("thinking")
				if clk != nil {
					clk.PlayFaces(ctx, []string{"wink"})
				}
			case "tts_speaking":
				rg.SetDialog("speaking")
				if clk != nil {
					clk.PlayFaces(ctx, []string{"neutral"})
				}
			case "tts_finished", "idle":
				rg.SetDialog("")
			case "pipeline_error":
				rg.SetError(true)
				time.AfterFunc(1600*time.Millisecond, func() {
					rg.SetError(false)
				})
			case "media_player_playing":
				// музыка через LVA (HA media player) — та же радуга, что и mpd
				rg.SetMPD("rainbow")
			case "volume_changed":
				// upstream: {"volume": float} 0.0..1.0 — применяем к sink
				// (у LVA аудио уходит в наш sink, так что это одна и та же громкость)
				pct := int(math.Round(e.Float("volume") * 100))
				setSinkPct(pct)
				volFeedback(pct)
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

// volumeColor — цвет вспышки громкости, двухсегментный градиент:
//
//	0..половины — синий → зелёный (B убывает, G растёт);
//	половина..максимума — зелёный → оранжевый (R растёт, G спадает до 140).
//
// pct=-1 — уровень не узнали: белая.
func volumeColor(pct int) ring.Color {
	if pct < 0 {
		return ring.White
	}
	t := float64(pct) / float64(VOLUME_MAX_PCT)
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
