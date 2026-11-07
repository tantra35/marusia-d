// Package clock — передняя панель VK Mini: 4×7-сегментных цифры + двоеточие
// на IS31FL3236A (I2C 0x3C), через кастомный seg-led класс ядра.
// Логика повторяет vendor-скрипт clockdisp: текст — запись в linedisp/message,
// произвольные маски сегментов — патч 4 управляющих кодов в map_seg7,
// двоеточие — обычные leds (clock:colon:top/bottom).
//
// Архитектура — АКТОР (как ring.Ring): единственная горутина Serve владеет
// всем состоянием панели и ЕДИНСТВЕННАЯ пишет в sysfs. Любое действие —
// команда через cmdCh (PlayFaces/ShowVolume/SetBrightness/SetCurrent);
// мьютексов, флагов занятости и фоновых горутин нет — параллельные
// анимации невозможны структурно. Отрисовка — по тику 100 мс: режим
// определяет, что на панели (время / лицо / громкость), запись происходит
// только при смене содержимого.
package clock

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Masks сегментов: a=0x01 b=0x02 c=0x04 d=0x08 e=0x10 f=0x20 g=0x40
// (порядок бит из map_to_7segment.h, как в clockdisp).
const (
	EyeArch   = 0x23 // a b f — открытый глаз, покой
	EyeLine   = 0x40 // g     — прищур
	EyeLow    = 0x08 // d     — сонный
	EyeHigh   = 0x01 // a     — поднятая бровь
	EyeWide   = 0x3f // 0     — широко
	EyeRound  = 0x5c // o     — маленький круглый
	AngryEyeL = 0x21 // a f   — бровь к носу
	AngryEyeR = 0x03 // a b   — зеркало
	blank     = 0x00
)

const (
	VOLUME_SHOW_MS = 1500 * time.Millisecond // сколько на панели держатся проценты громкости
	FACE_MS        = 350 * time.Millisecond  // длительность одного лица в анимации
	renderInterval = 100 * time.Millisecond  // тик отрисовки актора
)

// Команды актору.
const (
	cmdPlayFaces = iota
	cmdShowVolume
	cmdSetBrightness
	cmdSetCurrent
)

// clockCmd — команда актору. Fire-and-forget: подтверждение не нужно —
// запись sysfs занимает миллисекунды, ошибки актор логирует сам.
type clockCmd struct {
	kind  int
	faces []string // cmdPlayFaces
	pct   int      // cmdShowVolume
	val   int      // cmdSetBrightness (0..255) / cmdSetCurrent (1..4)
}

// Режим панели — что сейчас должно быть нарисовано.
type modeT int

const (
	modeTime   modeT = iota // часы ЧЧ:ММ
	modeFaces               // анимация лиц
	modeVolume              // проценты громкости
)

// Clock — пути в sysfs (Open находит дисплей так же, как clockdisp) +
// состояние владельца: все поля ниже cmdCh доступны ТОЛЬКО из Serve.
type Clock struct {
	disp       string // .../linedisp.N
	message    string
	mapSeg     string
	brightFile string // parent brightness (PWM 0..255)
	colonTop   string
	colonBot   string
	curDiv     string // i2c .../current_divider, 1..4
	bright     byte
	cmdCh      chan clockCmd

	mode       modeT     // текущий режим панели
	faces      []string  // modeFaces: очередь лиц
	faceIdx    int       // modeFaces: индекс следующего лица
	faceNext   time.Time // modeFaces: когда рисовать следующее лицо
	volUntil   time.Time // modeVolume: когда вернуть часы
	lastMinute string    // последняя нарисованная минута «15:04» (дедуп)
}

func Open() (*Clock, error) {
	var disp string
	patterns := []string{
		"/sys/class/linedisp/linedisp.*",
		"/sys/bus/platform/devices/*/linedisp.*",
		"/sys/devices/platform/*/linedisp.*",
	}
	for _, p := range patterns {
		m, _ := filepath.Glob(p)
		for _, d := range m {
			if _, err := os.Stat(filepath.Join(d, "message")); err == nil {
				disp = d
				break
			}
		}
		if disp != "" {
			break
		}
	}
	if disp == "" {
		return nil, fmt.Errorf("linedisp не найден: CONFIG_SEG_LED_CLASS в ядре?")
	}

	c := &Clock{
		disp:     disp,
		message:  filepath.Join(disp, "message"),
		mapSeg:   filepath.Join(disp, "map_seg7"),
		colonTop: findLed("clock:colon:top"),
		colonBot: findLed("clock:colon:bottom"),
		bright:   255,
		cmdCh:    make(chan clockCmd),
	}
	// яркость — на родительской платформе (уровень панели)
	for _, b := range []string{filepath.Join(disp, "../brightness"), filepath.Join(disp, "device/brightness")} {
		if fi, err := os.Stat(b); err == nil && !fi.IsDir() {
			c.brightFile = b
			break
		}
	}
	// current divider — на i2c-клиенте (is31fl3236a)
	if m, _ := filepath.Glob("/sys/bus/i2c/devices/*/current_divider"); len(m) > 0 {
		c.curDiv = m[0]
	}
	return c, nil
}

func findLed(name string) string {
	p := filepath.Join("/sys/class/leds", name, "brightness")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// ---------- команды актору (безопасны из любой горутины) ----------

// PlayFaces — проиграть лица по очереди (FACE_MS на лицо). Команда актору:
// последний запрос побеждает — новая анимация заменяет текущую, а не
// отбрасывается. По завершении актор сам возвращает часы.
func (c *Clock) PlayFaces(faces []string) {
	c.cmdCh <- clockCmd{kind: cmdPlayFaces, faces: faces}
}

// ShowVolume — показать громкость в процентах вместо часов; через
// VOLUME_SHOW_MS актор вернёт часы.
func (c *Clock) ShowVolume(pct int) {
	c.cmdCh <- clockCmd{kind: cmdShowVolume, pct: pct}
}

// SetBrightness — PWM 0..255 на панель. Двоеточие — отдельный LED, PWM
// родителя его не накрывает (vendor clockdisp тоже пишет его абсолютным
// значением): актор после смены яркости синхронизирует горящее двоеточие,
// иначе оно отстаёт до следующей перерисовки минутного тика (ловилось:
// цифры яркие — двоеточие тусклое).
func (c *Clock) SetBrightness(v byte) {
	c.cmdCh <- clockCmd{kind: cmdSetBrightness, val: int(v)}
}

// SetCurrent — аналоговый ток 1..4 (1 ярче всего; 4 — тихий режим).
func (c *Clock) SetCurrent(v int) {
	c.cmdCh <- clockCmd{kind: cmdSetCurrent, val: v}
}

// ---------- актор: единственный писатель в sysfs ----------

// Serve — актор: единственная горутина, владеющая состоянием панели и
// рисующая её содержимое по тику. Команды приходят через cmdCh.
// Блокирует, запускать в горутине.
func (c *Clock) Serve(ctx context.Context) {
	tick := time.NewTicker(renderInterval)
	defer tick.Stop()

	c.lastMinute = time.Now().Format("15:04")
	c.showTime()
	for {
		select {
		case <-ctx.Done():
			// панель гасим сами при остановке — отдельной Off не нужно
			c.blank()
			return
		case cmd := <-c.cmdCh:
			c.exec(cmd)
			c.render(time.Now())
		case <-tick.C:
			c.render(time.Now())
		}
	}
}

// exec — исполнить команду внутри актора: поменять режим/состояние и при
// необходимости немедленно написать содержимое. Отложенные кадры
// дорисовывает render().
func (c *Clock) exec(cmd clockCmd) {
	switch cmd.kind {
	case cmdPlayFaces:
		c.mode = modeFaces
		c.faces = cmd.faces
		c.faceIdx = 0
		c.faceNext = time.Time{} // нулевое время → render нарисует сразу
	case cmdShowVolume:
		if err := c.setText(fmt.Sprintf("%d", cmd.pct)); err != nil {
			log.Printf("clock: громкость %d: %v", cmd.pct, err)
			return
		}
		c.mode = modeVolume
		c.volUntil = time.Now().Add(VOLUME_SHOW_MS)
	case cmdSetBrightness:
		if c.brightFile == "" {
			log.Printf("clock: яркость %d: нет файла яркости", cmd.val)
			return
		}
		c.bright = byte(cmd.val)
		if err := writeStr(c.brightFile, fmt.Sprint(cmd.val)); err != nil {
			log.Printf("clock: яркость %d: %v", cmd.val, err)
			return
		}
		c.resyncColon()
	case cmdSetCurrent:
		if c.curDiv == "" {
			log.Printf("clock: current %d: нет current_divider", cmd.val)
			return
		}
		if cmd.val < 1 || cmd.val > 4 {
			log.Printf("clock: current %d: допустимо 1..4", cmd.val)
			return
		}
		if err := writeStr(c.curDiv, fmt.Sprint(cmd.val)); err != nil {
			log.Printf("clock: current %d: %v", cmd.val, err)
		}
	}
}

// render — нарисовать то, что должно быть на панели в момент now.
// Пишет в sysfs только при смене содержимого. Только в акторе.
func (c *Clock) render(now time.Time) {
	switch c.mode {
	case modeFaces:
		if now.Before(c.faceNext) {
			return // лицо ещё играет
		}
		if c.faceIdx >= len(c.faces) {
			// анимация кончилась — вернуть часы немедленно
			c.toTime(now)
			return
		}
		f := c.faces[c.faceIdx]
		c.faceIdx++
		c.faceNext = now.Add(FACE_MS)
		if err := c.face(f); err != nil {
			log.Printf("clock: лицо %s: %v", f, err)
			c.toTime(now) // ошибка отыгрыша — сразу часы, анимацию бросаем
		}
	case modeVolume:
		if now.Before(c.volUntil) {
			return
		}
		c.toTime(now)
	default: // modeTime — перерисовка при смене минуты
		m := now.Format("15:04")
		if m != c.lastMinute {
			c.lastMinute = m
			c.showTime()
		}
	}
}

// toTime — вернуться в режим часов (запомнить текущую минуту, чтобы
// не перерисовывать её повторно).
func (c *Clock) toTime(now time.Time) {
	c.mode = modeTime
	c.lastMinute = now.Format("15:04")
	c.showTime()
}

// ---------- запись в sysfs (только из Serve) ----------

// setText — показать текст, выравнивание вправо на 4 позиции (как clockdisp).
// Формат "12:34" зажигает двоеточие.
func (c *Clock) setText(text string) error {
	wantColon := strings.Contains(text, ":")
	text = strings.ReplaceAll(text, ":", "")
	s := fmt.Sprintf("%4.4s", text)
	if err := writeStr(c.message, s); err != nil {
		return err
	}
	return c.colon(wantColon)
}

// showMasks — произвольные маски на 4 цифры: патчим 4 неиспользуемых
// управляющих кода (0x01..0x04) в map_seg7 и печатаем их.
func (c *Clock) showMasks(m [4]byte) error {
	raw, err := os.ReadFile(c.mapSeg)
	if err != nil || len(raw) != 128 {
		return fmt.Errorf("map_seg7: нужно ровно 128 байт, %v", err)
	}
	for i, v := range m {
		raw[1+i] = v
	}
	if err := writeFile(c.mapSeg, raw); err != nil {
		return err
	}
	return writeStr(c.message, "\x01\x02\x03\x04")
}

// face — лицо из clockdisp: два глаза на средних цифрах, нос — нижняя точка.
func (c *Clock) face(name string) error {
	l, r, ok := faceEyes(name)
	if !ok {
		return fmt.Errorf("нет лица %q", name)
	}
	if err := c.showMasks([4]byte{blank, l, r, blank}); err != nil {
		return err
	}
	if c.colonTop != "" && c.colonBot != "" {
		writeStr(c.colonTop, "0")
		return writeStr(c.colonBot, fmt.Sprint(c.bright)) // нос
	}
	return nil
}

// showTime — текущее время ЧЧ:ММ.
func (c *Clock) showTime() {
	if err := c.setText(time.Now().Format("15:04")); err != nil {
		log.Printf("clock: %v", err)
	}
}

// blank — погасить всё (финал при остановке актора).
func (c *Clock) blank() {
	if err := writeStr(c.message, "    "); err != nil {
		log.Printf("clock: гашение: %v", err)
	}
	c.colon(false)
}

// colon — двоеточие on/off.
func (c *Clock) colon(on bool) error {
	v := "0"
	if on {
		v = fmt.Sprint(c.bright)
	}
	for _, p := range []string{c.colonTop, c.colonBot} {
		if p == "" {
			continue
		}
		if err := writeStr(p, v); err != nil {
			return err
		}
	}
	return nil
}

// resyncColon — поднять двоеточие до текущей яркости, если оно горит.
func (c *Clock) resyncColon() error {
	if c.colonTop == "" {
		return nil
	}
	b, err := os.ReadFile(c.colonTop)
	if err != nil || strings.TrimSpace(string(b)) == "0" {
		return nil // выключено — не включаем
	}
	return c.colon(true)
}

func faceEyes(name string) (byte, byte, bool) {
	switch name {
	case "idle":
		return EyeArch, EyeArch, true
	case "neutral":
		return EyeLine, EyeLine, true
	case "surprised":
		return EyeWide, EyeWide, true
	case "curious":
		return EyeRound, EyeRound, true
	case "sleepy":
		return EyeLow, EyeLow, true
	case "shock":
		return EyeHigh, EyeHigh, true
	case "wink":
		return EyeArch, EyeLine, true
	case "angry":
		return AngryEyeL, AngryEyeR, true
	}
	return 0, 0, false
}

func writeStr(path, s string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s)
	return err
}

func writeFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}
