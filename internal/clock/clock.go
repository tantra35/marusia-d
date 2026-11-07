// Package clock — передняя панель VK Mini: 4×7-сегментных цифры + двоеточие
// на IS31FL3236A (I2C 0x3C), через кастомный seg-led класс ядра.
// Логика повторяет vendor-скрипт clockdisp: текст — запись в linedisp/message,
// произвольные маски сегментов — глифы в map_seg7 ('a'..'h',
// вписываются один раз),
// двоеточие — обычные leds (clock:colon:top/bottom).
//
// Архитектура — АКТОР (как ring.Ring): единственная горутина Serve владеет
// всем состоянием панели и ЕДИНСТВЕННАЯ пишет в sysfs. Любое действие —
// команда через cmdCh (PlayFaces/ShowVolume/SetBrightness/SetCurrent);
// мьютексов, флагов занятости и фоновых горутин нет — параллельные
// анимации невозможны структурно. Шаг состояния — по тику 100 мс: режим
// определяет, что на панели (время / лицо / громкость), запись происходит
// только при смене содержимого.
//
// Горячие пути — БЕЗ аллокаций: дедуп минуты по Unix()/60 (не Format),
// поля и числа собираются в переиспользуемые буферы актора (fieldBuf/
// numBuf); map_seg7 читается один раз (регистрация глифов), кадр лица —
// одна запись 4 байт в message.
package clock

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Masks сегментов: a=0x01 b=0x02 c=0x04 d=0x08 e=0x10 f=0x20 g=0x40
// (порядок бит из map_to_7segment.h, как в clockdisp).

const (
	VOLUME_SHOW_MS = 1500 * time.Millisecond // сколько на панели держатся проценты громкости
	FACE_MS        = 350 * time.Millisecond  // длительность одного лица в анимации
	updateInterval = 100 * time.Millisecond  // тик актора (шаг состояния панели)
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

// Глифы лиц: GLYPH_MASKS — буква → маска сегментов. Буква и есть код в
// map_seg7 (ASCII), маски вписываются в таблицу ОДИН раз под эти коды.
// Буквы выбраны из символов, которые часы никогда не печатают (текст —
// только '0'..'9' и пробел), поэтому обычный вывод времени не затрагивается.
// Кадр лица — одна запись 4 байт в message (' ' + буква + буква + ' ';
// пробел в дефолтной таблице = пустая цифра).
var GLYPH_MASKS = map[byte]byte{
	'a': 0x23, // a b f — открытый глаз, покой
	'b': 0x40, // g     — прищур
	'c': 0x08, // d     — сонный
	'd': 0x01, // a     — поднятая бровь
	'e': 0x3f, // 0     — широко
	'f': 0x5c, // o     — маленький круглый
	'g': 0x21, // a f   — бровь к носу
	'h': 0x03, // a b   — зеркало
}

// zeroDigit — «0» для выключения LED (writeBytes, без конверсии строки).
var zeroDigit = [1]byte{'0'}

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

	mode        modeT     // текущий режим панели
	faces       []string  // modeFaces: очередь лиц
	faceIdx     int       // modeFaces: индекс следующего лица
	faceNext    time.Time // modeFaces: когда рисовать следующее лицо
	volPct      int       // modeVolume: какие проценты рисовать
	volDrawn    bool      // modeVolume: проценты уже на панели
	volUntil    time.Time // modeVolume: когда вернуть часы
	cur         int       // current_divider 1..4 (на панель попадёт render'ом)
	brightDirty bool      // яркость изменилась — render перезапишет
	curDirty    bool      // current изменился — render перезапишет

	// дедуп минут без аллокаций: номер минуты Unix()/60; −1 = ещё не рисовали
	lastMinBucket int64

	// буферы актора (переиспользуются, ноль аллокаций на запись):
	segMap   [128]byte // кэш map_seg7: читается один раз (регистрация глифов)
	segHave  bool      // глифы уже вписаны в таблицу
	fieldBuf [4]byte   // выравненное поле 4 символов для message
	numBuf   [8]byte   // числовые значения (яркость/ток/громкость) для sysfs
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
	tick := time.NewTicker(updateInterval)
	defer tick.Stop()

	c.lastMinBucket = -1 // −1 → первый render нарисует часы
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

// exec — исполнить команду внутри актора: ТОЛЬКО поменять состояние,
// ничего не писать. Единственный писатель — render(); Serve вызывает его
// сразу после exec, поэтому задержки отображения команда не добавляет.
func (c *Clock) exec(cmd clockCmd) {
	switch cmd.kind {
	case cmdPlayFaces:
		c.mode = modeFaces
		c.faces = cmd.faces
		c.faceIdx = 0
		c.faceNext = time.Time{} // нулевое время → render нарисует сразу
	case cmdShowVolume:
		c.mode = modeVolume
		c.volPct = cmd.pct
		c.volDrawn = false
		c.volUntil = time.Now().Add(VOLUME_SHOW_MS)
	case cmdSetBrightness:
		c.bright = byte(cmd.val)
		c.brightDirty = true
	case cmdSetCurrent:
		if cmd.val < 1 || cmd.val > 4 {
			log.Printf("clock: current %d: допустимо 1..4", cmd.val)
			return
		}
		c.cur = cmd.val
		c.curDirty = true
	}
}

// render — шаг состояния панели в момент now и ЕДИНСТВЕННАЯ точка записи
// в sysfs: сперва доводит до устройства отложенные команды (яркость, ток),
// затем рисует то, что должно быть на панели (лица / проценты / часы).
// В отличие от ring.render, кадр строится не каждый тик — запись только
// при смене содержимого (дедуп минутой, volDrawn, faceNext).
func (c *Clock) render(now time.Time) {
	if c.brightDirty {
		c.brightDirty = false
		if err := c.writeBrightness(); err != nil {
			log.Printf("clock: яркость %d: %v", c.bright, err)
		}
	}
	if c.curDirty {
		c.curDirty = false
		if err := c.writeCurrent(); err != nil {
			log.Printf("clock: current %d: %v", c.cur, err)
		}
	}
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
			if !c.volDrawn {
				if err := c.writeText(c.appendNum(c.volPct), false); err != nil {
					log.Printf("clock: громкость %d: %v", c.volPct, err)
					c.toTime(now) // не смогли — вернуть часы, а не долбить ошибку каждый тик
					return
				}
				c.volDrawn = true
			}
			return
		}
		c.toTime(now)
	default: // modeTime — перерисовка при смене минуты (дедуп без аллокаций)
		if bucket := now.Unix() / 60; bucket != c.lastMinBucket {
			c.lastMinBucket = bucket
			c.showTime(now)
		}
	}
}

// writeBrightness — PWM на панель; горящее двоеточие синхронизируется
// (оно — отдельный LED, PWM родителя его не накрывает: ловилось «цифры
// яркие — двоеточие тусклое»). Только из render.
func (c *Clock) writeBrightness() error {
	if c.brightFile == "" {
		return fmt.Errorf("нет файла яркости")
	}
	if err := writeBytes(c.brightFile, c.appendNum(int(c.bright))); err != nil {
		return err
	}
	return c.resyncColon()
}

// writeCurrent — аналоговый ток 1..4. Только из render.
func (c *Clock) writeCurrent() error {
	if c.curDiv == "" {
		return fmt.Errorf("нет current_divider")
	}
	return writeBytes(c.curDiv, c.appendNum(c.cur))
}

// toTime — вернуться в режим часов (запомнить текущую минуту, чтобы
// не перерисовывать её повторно).
func (c *Clock) toTime(now time.Time) {
	c.mode = modeTime
	c.lastMinBucket = now.Unix() / 60
	c.showTime(now)
}

// ---------- запись в sysfs (только из Serve) ----------

// appendNum — число в переиспользуемый numBuf актора: ноль аллокаций на
// запись чисел в sysfs (яркость, ток, проценты, нос).
func (c *Clock) appendNum(v int) []byte {
	return strconv.AppendInt(c.numBuf[:0], int64(v), 10)
}

// writeText — показать поле на message, выравнивание вправо на 4 позиции
// (как %4.4s в clockdisp: длиннее 4 — лишнее слева отрезается, короче —
// слева добивается пробелами); colon — зажечь двоеточие. Поле собирается
// в буфер актора, без аллокаций.
func (c *Clock) writeText(b []byte, colon bool) error {
	if len(b) > 4 {
		b = b[:4]
	}
	f := c.fieldBuf[:]
	// гасим только левую подкладку: b может алиасить fieldBuf (showTime),
	// полная очистка затирала бы сами цифры до копирования
	for i := range f[:4-len(b)] {
		f[i] = ' '
	}
	copy(f[4-len(b):], b)
	if err := writeBytes(c.message, f); err != nil {
		return err
	}
	return c.colon(colon)
}

// registerGlyphs — ОДИН раз вписать маски глаз в map_seg7 под их буквы
// (ключ GLYPH_MASKS = ASCII-код ячейки таблицы). Таблица читается в segMap
// кэш, патчится только в этих 8 слотах и пишется обратно целиком;
// повторные вызовы — no-op. Порядок обхода map не важен: каждый ключ
// пишет свою ячейку. Только из Serve (единственный писатель).
func (c *Clock) registerGlyphs() error {
	if c.segHave {
		return nil
	}
	raw, err := os.ReadFile(c.mapSeg)
	if err != nil || len(raw) != 128 {
		return fmt.Errorf("map_seg7: нужно ровно 128 байт, %v", err)
	}
	copy(c.segMap[:], raw)
	for code, m := range GLYPH_MASKS {
		c.segMap[code] = m
	}
	if err := writeBytes(c.mapSeg, c.segMap[:]); err != nil {
		return err
	}
	c.segHave = true
	return nil
}

// face — лицо из clockdisp: два глаза на средних цифрах, нос — нижняя
// точка. Кадр — одна запись 4 байт в message, таблица не трогается.
func (c *Clock) face(name string) error {
	if err := c.registerGlyphs(); err != nil {
		return err
	}
	l, r, ok := faceEyes(name)
	if !ok {
		return fmt.Errorf("нет лица %q", name)
	}
	f := c.fieldBuf[:]
	f[0] = ' '
	f[1] = l
	f[2] = r
	f[3] = ' '
	if err := writeBytes(c.message, f); err != nil {
		return err
	}
	if c.colonTop != "" && c.colonBot != "" {
		writeBytes(c.colonTop, zeroDigit[:])
		return writeBytes(c.colonBot, c.appendNum(int(c.bright))) // нос
	}
	return nil
}

// showTime — часы ЧЧ:ММ из готового now: поле собирается в буфер актора,
// без time.Format и без повторного time.Now().
func (c *Clock) showTime(now time.Time) {
	h, m := now.Hour(), now.Minute()
	f := c.fieldBuf[:]
	f[0] = byte('0' + h/10)
	f[1] = byte('0' + h%10)
	f[2] = byte('0' + m/10)
	f[3] = byte('0' + m%10)
	if err := c.writeText(f, true); err != nil {
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

// colon — двоеточие on/off (значение яркости — из numBuf, без аллокаций).
func (c *Clock) colon(on bool) error {
	v := c.appendNum(0)
	if on {
		v = c.appendNum(int(c.bright))
	}
	for _, p := range [2]string{c.colonTop, c.colonBot} {
		if p == "" {
			continue
		}
		if err := writeBytes(p, v); err != nil {
			return err
		}
	}
	return nil
}

// resyncColon — поднять двоеточие до текущей яркости, если оно горит.
// Редкий путь (только при смене яркости) — чтение с аллокацией допустимо.
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

// faceEyes — буквы-глифы для лица: два глаза из GLYPH_MASKS.
func faceEyes(name string) (byte, byte, bool) {
	switch name {
	case "idle":
		return 'a', 'a', true
	case "neutral":
		return 'b', 'b', true
	case "surprised":
		return 'e', 'e', true
	case "curious":
		return 'f', 'f', true
	case "sleepy":
		return 'c', 'c', true
	case "shock":
		return 'd', 'd', true
	case "wink":
		return 'a', 'b', true
	case "angry":
		return 'g', 'h', true
	}
	return 0, 0, false
}

func writeStr(path, s string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s) // редкие литералы: конверсия string→[]byte допустима
	return err
}

func writeBytes(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}
