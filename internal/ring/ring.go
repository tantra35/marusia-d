// Package ring — световое кольцо VK Mini: 12× WS2812-подобных диодов,
// прямой вывод через spidev (транспорт и ограничения — в spi.go).
//
// Архитектура — АКТОР: единственная горутина (Serve) владеет всем состоянием
// Ring и рисует кадры. Любое действие (сеттеры, Flash, запросы) — команда
// через cmdCh; каждая команда несёт свой reply-канал для результата.
// Мьютексов нет: состояние трогает только актор.
//
// Отрисовка (как у часов, clock.Serve): тик ~40 мс; на каждом тике render()
// вычисляет кадр из состояния + фазы времени и шлёт его через SPI (rebind +
// prime + кадр). Статичные сцены (off/muted/solid) рисуются один раз —
// диоды защёлкивают последнее значение, повтор не нужен. Анимации (дыхание,
// мигание, затухание вспышки) — новый кадр на каждый тик. Частота кадров
// ограничена ценой rebind (~30–60 мс).
package ring

import (
	"context"
	"log"
	"math"
	"time"
)

const (
	DefaultDevice = "/dev/spidev0.0"
	leds          = 12

	renderInterval = 40 * time.Millisecond // тик отрисовки (~25 fps сверху)
	breathePeriod  = 1500 * time.Millisecond
	blinkPeriod    = 500 * time.Millisecond
	FLASH_MS       = 1200 * time.Millisecond // вспышка громкости: полный спад
)

// Палитра сцен (RGB). var, не const: литералы массивов в Go константами не бывают.
var (
	Off        = Color{}                   // чёрный (используется main)
	White      = Color{255, 255, 255}      // (main: цвет по умолчанию)
	MUTED_RGB  = [3]byte{0x8C, 0x00, 0x00} // приглушённый красный
	LISTEN_RGB = [3]byte{0x00, 0x3C, 0xFF} // синий
	THINK_RGB  = [3]byte{0x96, 0x28, 0xFF} // фиолетовый
	SPEAK_RGB  = [3]byte{0x00, 0xAA, 0xE6} // голубой
	ERROR_RGB  = [3]byte{0xFF, 0x00, 0x00} // ошибка
	noconnRGB  = [3]byte{0x64, 0x00, 0x00} // нет связи с LVA — тусклый красный
)

// Color — RGB 0..255.
type Color struct{ R, G, B uint8 }

// Команды актору.
const (
	cmdSetMPD = iota
	cmdSetDialog
	cmdSetMuted
	cmdGetMuted
	cmdSetError
	cmdSetLive
	cmdSetSnapshot
	cmdSetHA
	cmdSetScale
	cmdSetSolid
	cmdFlash
)

// ringCmd — команда актору. reply — канал результата: сеттеры получают
// подтверждение (nil), запросы — значение (например, cmdGetMuted → bool).
type ringCmd struct {
	kind  int
	str   string
	c     Color
	d     time.Duration
	f     float64
	b     bool
	reply chan any
}

// Ring — состояние сцен + актор-рендер. Все поля ниже cmdCh доступны
// ТОЛЬКО из горутины Serve.
type Ring struct {
	dev   string
	n     int
	cmdCh chan ringCmd // команды актору; каждая несёт свой reply-канал

	mpd      string // сцена источника «музыка»: off (радугу убрали — мешала)
	dialog   string // "" | wake | listening | thinking | speaking
	errorT   bool
	muted    bool
	lvaSeen  bool // было хоть одно живое соединение с LVA
	lvaLive  bool // соединение открыто сейчас
	haActive bool // кольцом управляет HA (light_command)

	scale float64 // глобальная яркость от датчика света (0..1)
	solid Color   // цвет для сцены solid (light_command из HA)

	overlayColor Color         // цвет вспышки громкости
	overlayDur   time.Duration // её полная длительность (спад)
	overlayTt    time.Time     // когда вспышка истекает; zero = нет

	lastDrawn string // ключ последнего нарисованного статичного кадра ("" после анимации)
}

func New(dev string, n int) *Ring {
	return &Ring{
		dev:   dev,
		n:     n,
		cmdCh: make(chan ringCmd),
		scale: 1,
		mpd:   "off",
	}
}

// send — отправить команду актору и дождаться результата (синхронно).
func (r *Ring) send(cmd *ringCmd) any {
	r.cmdCh <- *cmd
	return <-cmd.reply
}

// SetMPD — сцена источника «музыка»: off (радугу убрали — мешала).
func (r *Ring) SetMPD(scene string) {
	cmd := ringCmd{kind: cmdSetMPD, str: scene, reply: make(chan any, 1)}
	r.send(&cmd)
}

// SetDialog — активная фаза диалога LVA ("" = диалога нет).
func (r *Ring) SetDialog(name string) {
	cmd := ringCmd{kind: cmdSetDialog, str: name, reply: make(chan any, 1)}
	r.send(&cmd)
}

// SetMuted — микрофон заглушен (источник истины — LVA).
func (r *Ring) SetMuted(m bool) {
	cmd := ringCmd{kind: cmdSetMuted, b: m, reply: make(chan any, 1)}
	r.send(&cmd)
}

// Muted — прочитать состояние мьюта (для кнопки микрофона).
func (r *Ring) Muted() bool {
	cmd := ringCmd{kind: cmdGetMuted, reply: make(chan any, 1)}
	return r.send(&cmd).(bool)
}

// SetError — мигание ошибки диалога (сбрасывается по таймеру).
func (r *Ring) SetError(on bool) {
	cmd := ringCmd{kind: cmdSetError, b: on, reply: make(chan any, 1)}
	r.send(&cmd)
}

// SetLive — LVA-соединение открыто/закрыто.
func (r *Ring) SetLive(live bool) {
	cmd := ringCmd{kind: cmdSetLive, b: live, reply: make(chan any, 1)}
	r.send(&cmd)
}

// SetSnapshot — первый коннект LVA прислал полное состояние.
func (r *Ring) SetSnapshot(muted bool) {
	cmd := ringCmd{kind: cmdSetSnapshot, b: muted, reply: make(chan any, 1)}
	r.send(&cmd)
}

// SetHA — кольцом управляет Home Assistant.
func (r *Ring) SetHA(on bool) {
	cmd := ringCmd{kind: cmdSetHA, b: on, reply: make(chan any, 1)}
	r.send(&cmd)
}

// SetScale — глобальная яркость от датчика света (0..1).
func (r *Ring) SetScale(s float64) {
	cmd := ringCmd{kind: cmdSetScale, f: s, reply: make(chan any, 1)}
	r.send(&cmd)
}

// SetSolid — цвет для сцены solid (управление кольцом из HA).
func (r *Ring) SetSolid(c Color) {
	cmd := ringCmd{kind: cmdSetSolid, c: c, reply: make(chan any, 1)}
	r.send(&cmd)
}

// Flash — вспышка цветом c длительностью d: мгновенно полная яркость,
// затем плавный спад (профиль в frame()). Синхронный вызов: возвращает
// подтверждение актора; спад дорисовывается в рендере.
func (r *Ring) Flash(c Color, d time.Duration) {
	cmd := ringCmd{kind: cmdFlash, c: c, d: d, reply: make(chan any, 1)}
	r.send(&cmd)
}

// resolve — вычислить сцену из состояния: solid (HA) > error > noconn >
// muted > диалог > mpd. Чистая функция — состояние не меняет.
func (r *Ring) resolve() string {
	switch {
	case r.haActive:
		return "solid"
	case r.errorT:
		return "error"
	case r.lvaSeen && !r.lvaLive:
		return "noconn"
	case r.muted:
		return "muted"
	case r.dialog != "":
		return r.dialog
	default:
		return r.mpd
	}
}

// exec — исполнить команду внутри актора: поменять поле состояния.
// Кадр из состояния рисует render(). Только в акторе.
func (r *Ring) exec(cmd *ringCmd) any {
	switch cmd.kind {
	case cmdSetMPD:
		r.mpd = "off" // радугу убрали — мешала
	case cmdSetDialog:
		r.dialog = cmd.str
	case cmdSetMuted:
		r.muted = cmd.b
	case cmdGetMuted:
		return r.muted
	case cmdSetError:
		r.errorT = cmd.b
	case cmdSetLive:
		r.lvaLive = cmd.b
	case cmdSetSnapshot:
		r.lvaSeen, r.lvaLive, r.muted = true, true, cmd.b
	case cmdSetHA:
		r.haActive = cmd.b
	case cmdSetScale:
		if cmd.f < 0 {
			cmd.f = 0
		} else if cmd.f > 1 {
			cmd.f = 1
		}
		r.scale = cmd.f
	case cmdSetSolid:
		r.solid = cmd.c
	case cmdFlash:
		r.overlayColor = Color{R: cmd.c.R, G: cmd.c.G, B: cmd.c.B}
		r.overlayDur = cmd.d
		r.overlayTt = time.Now().Add(cmd.d)
	}
	return nil
}

// mul — масштабировать цвет по коэффициенту 0..1.
func mul(c [3]byte, k float64) [3]byte {
	return [3]byte{
		byte(float64(c[0]) * k),
		byte(float64(c[1]) * k),
		byte(float64(c[2]) * k),
	}
}

// breathe — фаза дыхания 0.15..1 (плавная синусоида).
func breathe(now time.Time, period time.Duration) float64 {
	s := math.Sin(2 * math.Pi * float64(now.UnixNano()%int64(period)) / float64(period))
	return 0.15 + 0.85*(s+1)/2
}

// frame — кадр для текущего состояния и момента времени.
// key — ключ статичного кадра ("" у анимаций), px — пиксели.
// Чистая: только читает состояние — годится для тестов.
func (r *Ring) frame(now time.Time) (key string, px [12][3]byte, animated bool) {
	// вспышка громкости: мгновенный кадр полной яркости → плавный спад
	if now.Before(r.overlayTt) {
		k := float64(r.overlayTt.Sub(now)) / float64(r.overlayDur)
		if k > 1 {
			k = 1
		}
		b := math.Pow(k, 1.6) // резкий старт, плавный хвост
		col := mul([3]byte{r.overlayColor.R, r.overlayColor.G, r.overlayColor.B}, b)
		for i := range px {
			px[i] = col
		}
		return "", px, true
	}

	dim := 0.25 + 0.75*r.scale // датчик света приглушает, но не гасит
	switch scene := r.resolve(); scene {
	case "solid":
		c := [3]byte{r.solid.R, r.solid.G, r.solid.B}
		for i := range px {
			px[i] = mul(c, dim)
		}
		return "solid", px, false
	case "muted":
		for i := range px {
			px[i] = mul(MUTED_RGB, dim)
		}
		return "muted", px, false
	case "error":
		on := now.UnixMilli()%int64(blinkPeriod) < int64(blinkPeriod)/2
		for i := range px {
			if on {
				px[i] = mul(ERROR_RGB, dim)
			}
		}
		return "", px, true
	case "noconn":
		b := breathe(now, breathePeriod) * 0.4 // тускло
		for i := range px {
			px[i] = mul(noconnRGB, b)
		}
		return "", px, true
	case "listening", "thinking", "speaking":
		b := breathe(now, breathePeriod) * r.scale
		var c [3]byte
		switch scene {
		case "listening":
			c = LISTEN_RGB
		case "thinking":
			c = THINK_RGB
		default:
			c = SPEAK_RGB
		}
		for i := range px {
			px[i] = mul(c, b)
		}
		return "", px, true
	default: // off — чёрный
		return "off", px, false
	}
}

// render — нарисовать кадр, если он отличается от уже показанного.
// Анимации рисуются всегда (фаза меняется). Только в акторе.
func (r *Ring) render(now time.Time) {
	key, px, animated := r.frame(now)
	if !animated && key == r.lastDrawn {
		return // статичный кадр уже на диодах
	}
	if err := r.draw(encodeFrame(px)); err != nil {
		log.Printf("ring: draw: %v", err)
		r.lastDrawn = "" // повторить при следующем тике
		return
	}
	if animated {
		r.lastDrawn = "" // после анимации статичную сцену перерисовать
	} else {
		r.lastDrawn = key
	}
}

// Serve — актор: единственная горутина, владеющая состоянием Ring и
// рисующая кадры. Команды приходят через cmdCh, каждая отчитывается через
// свой reply-канал. Блокирует, запускать в горутине.
func (r *Ring) Serve(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("ring: panic: %v", p)
		}
	}()

	tick := time.NewTicker(renderInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			// кадр в диодах защёлкнут — погасить кольцо напоследок
			var black [12][3]byte
			if err := r.draw(encodeFrame(black)); err != nil {
				log.Printf("ring: финальное гашение: %v", err)
			}
			return
		case cmd := <-r.cmdCh:
			r.exec(&cmd)         // 1. состояние
			r.render(time.Now()) // 2. кадр
			cmd.reply <- nil     // 3. подтверждение
		case <-tick.C:
			r.render(time.Now())
		}
	}
}
