// Package clock — передняя панель VK Mini: 4×7-сегментных цифры + двоеточие
// на IS31FL3236A (I2C 0x3C), через кастомный seg-led класс ядра.
// Логика повторяет vendor-скрипт clockdisp: текст — запись в linedisp/message,
// произвольные маски сегментов — патч 4 управляющих кодов в map_seg7,
// двоеточие — обычные leds (clock:colon:top/bottom).
package clock

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// VOLUME_SHOW_MS — сколько на панели держатся проценты громкости перед
// возвратом к часам.
const VOLUME_SHOW_MS = 1500 * time.Millisecond

// Clock — пути в sysfs. Open находит дисплей так же, как clockdisp.
type Clock struct {
	disp       string // .../linedisp.N
	message    string
	mapSeg     string
	brightFile string // parent brightness (PWM 0..255)
	colonTop   string
	colonBot   string
	curDiv     string // i2c .../current_divider, 1..4
	bright     byte
	anim       atomic.Bool   // идёт анимация лиц — Serve панель не трогает
	faceDoneCh chan struct{} // анимация закончилась — Serve возвращает часы

	volMu     sync.Mutex  // охраняет showTimer
	showTimer *time.Timer // отложенный возврат к часам после показа громкости
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
		disp:       disp,
		message:    filepath.Join(disp, "message"),
		mapSeg:     filepath.Join(disp, "map_seg7"),
		colonTop:   findLed("clock:colon:top"),
		colonBot:   findLed("clock:colon:bottom"),
		bright:     255,
		faceDoneCh: make(chan struct{}, 1),
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

// SetText — показать текст, выравнивание вправо на 4 позиции (как clockdisp).
// Формат "12:34" зажигает двоеточие.
func (c *Clock) SetText(text string) error {
	wantColon := strings.Contains(text, ":")
	text = strings.ReplaceAll(text, ":", "")
	s := fmt.Sprintf("%4.4s", text)
	if err := writeStr(c.message, s); err != nil {
		return err
	}
	if wantColon {
		return c.Colon(true)
	}
	return c.Colon(false)
}

// ShowMasks — произвольные маски на 4 цифры: патчим 4 неиспользуемых
// управляющих кода (0x01..0x04) в map_seg7 и печатаем их.
func (c *Clock) ShowMasks(m [4]byte) error {
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

// Face — лицо из clockdisp: два глаза на средних цифрах, нос — нижняя точка.
func (c *Clock) Face(name string) error {
	l, r, ok := faceEyes(name)
	if !ok {
		return fmt.Errorf("нет лица %q", name)
	}
	if err := c.ShowMasks([4]byte{blank, l, r, blank}); err != nil {
		return err
	}
	if c.colonTop != "" && c.colonBot != "" {
		writeStr(c.colonTop, "0")
		return writeStr(c.colonBot, fmt.Sprint(c.bright)) // нос
	}
	return nil
}

// PlayFaces — проиграть лица по очереди (350 мс на лицо) в фоновой горутине.
// С начала анимации Serve панель не трогает; по завершении возвращает часы.
// Прерывание: ctx или ошибка отыгрыша лица.
func (c *Clock) PlayFaces(ctx context.Context, faces []string) {
	go func() {
		c.anim.Store(true)
		defer c.anim.Store(false)
		for _, f := range faces {
			if ctx.Err() != nil {
				return
			}
			if err := c.Face(f); err != nil {
				log.Printf("clock: лицо %s: %v", f, err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(350 * time.Millisecond):
			}
		}
		select {
		case c.faceDoneCh <- struct{}{}:
		default:
		}
	}()
}

// ShowVolume — показать громкость в процентах вместо часов; через VOLUME_SHOW_MS
// вернуть часы. Во время анимации лиц панель не трогаем (закон занятости).
func (c *Clock) ShowVolume(pct int) {
	if c.anim.Load() {
		return
	}
	if err := c.SetText(fmt.Sprintf("%d", pct)); err != nil {
		log.Printf("clock: громкость %d: %v", pct, err)
		return
	}
	c.volMu.Lock()
	defer c.volMu.Unlock()
	if c.showTimer != nil {
		c.showTimer.Stop()
	}
	c.showTimer = time.AfterFunc(VOLUME_SHOW_MS, c.showTime)
}

// Serve — фоновый цикл панели: держать актуальное время (проверка каждую
// секунду, перерисовка при смене минуты). Пока идёт анимация лиц (anim),
// панель не трогает; по faceDoneCh — возвращает часы. Запускать в горутине.
func (c *Clock) Serve(ctx context.Context) {
	last := ""
	c.showTime()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.faceDoneCh:
			// анимация закончилась — вернуть часы немедленно
			c.showTime()
			last = time.Now().Format("15:04")
		case <-tick.C:
			if c.anim.Load() {
				continue // играет лицо — не затирать
			}
			now := time.Now().Format("15:04")
			if now != last {
				last = now
				c.showTime()
			}
		}
	}
}

// showTime — текущее время ЧЧ:ММ.
func (c *Clock) showTime() {
	if err := c.SetText(time.Now().Format("15:04")); err != nil {
		log.Printf("clock: %v", err)
	}
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

// Colon — двоеточие on/off.
func (c *Clock) Colon(on bool) error {
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

// Off — погасить всё.
func (c *Clock) Off() error {
	if err := writeStr(c.message, "    "); err != nil {
		return err
	}
	return c.Colon(false)
}

// SetBrightness — PWM 0..255 на панель.
func (c *Clock) SetBrightness(v byte) error {
	if c.brightFile == "" {
		return fmt.Errorf("нет файла яркости")
	}
	c.bright = v
	return writeStr(c.brightFile, fmt.Sprint(v))
}

// SetCurrent — аналоговый ток 1..4 (1 ярче всего; 4 — тихий режим).
func (c *Clock) SetCurrent(v int) error {
	if c.curDiv == "" {
		return fmt.Errorf("нет current_divider")
	}
	if v < 1 || v > 4 {
		return fmt.Errorf("current 1..4, получено %d", v)
	}
	return writeStr(c.curDiv, fmt.Sprint(v))
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
