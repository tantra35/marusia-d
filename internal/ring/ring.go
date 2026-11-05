// Package ring — световое кольцо VK Mini: 12× WS2812, управление через
// vendor-утилиту ringled (/usr/local/bin/ringled).
//
// Почему не напрямую SPI: DMA-транспорт amlogic spicc требует rebind
// контроллера на каждую «сессию» вывода, а частый rebind из userspace валит
// ядро 6.18 (Oops в meson_spicc_probe → sysfs_create_link, UART-лог
// 2026-11-05). ringled сам делает rebind/prime/кадры — его одиночные запуски
// работают стабильно (проверено: off/all/rainbow/breathe).
//
// Архитектура — АКТОР: единственная горутина (Serve) владеет всем состоянием
// Ring и запускает ringled. Любое действие (сеттеры, Flash, запросы) —
// команда через cmdCh; каждая команда несёт свой reply-канал, через который
// вызывающий забирает результат. Мьютексов нет: состояние трогает только
// актор, сериализация бесплатная. ringled запускается ровно одной горутиной.
//
// Модель показа: сцена → аргументы ringled. Смена сцены → убить старый
// процесс, запустить новый; анимации (--duration) перезапускаются по
// завершении. Статика (off/all) защёлкивается одним кадром и не
// перезапускается.
package ring

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	ringledBin    = "/usr/local/bin/ringled"
	animDurMs     = "10000"          // длительность одного анимационного прогона
	DefaultDevice = "/dev/spidev0.0" // spidev-узел (для совместимости вызовов)
	ERROR_MS      = "600"            // ошибка: дыхание за 0.6 с
	ANIM_DUR_MS   = "10000"          // длительность анимационного прогона
	// Вспышка громкости: у ringled «--breathe MS» — ПЕРИОД дыхания
	// (полуволна вверх + полуволна вниз). Длительность меньше периода:
	// обрыв на нисходящей фазе — плавное затухание без хвоста второго
	// вдоха (при duration==breathe рендерилось ~1.2 вдоха — «короткая
	// вспышка» в конце).
	FLASH_MS      = "800"  // вспышка громкости: суммарно
	FLASH_BREATHE = "1000" // период дыхания больше длительности
)

// Color — RGB 0..255.
type Color struct{ R, G, B uint8 }

var (
	Off   = Color{}
	White = Color{255, 255, 255}
	Amber = Color{255, 140, 0}
)

func hex(c Color) string { return fmt.Sprintf("%02X%02X%02X", c.R, c.G, c.B) }

func brightnessPct(scale float64) string {
	p := int(scale * 100)
	if p < 1 {
		p = 1
	}
	if p > 100 {
		p = 100
	}
	return fmt.Sprint(p)
}

// Цвета сцен (hex RRGGBB).
const (
	MUTED_HEX  = "8C0000" // приглушённый красный
	LISTEN_HEX = "003CFF" // синий
	THINK_HEX  = "9628FF" // фиолетовый
	SPEAK_HEX  = "00AAE6" // голубой
)

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

// Ring — состояние сцен + актор-супервизор процесса ringled.
// Все поля ниже cmdCh доступны ТОЛЬКО из горутины Serve.
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

	overlayKey  string    // ключ оверлея (вспышка громкости)
	overlayArgs []string  // его аргументы ringled
	overlayTt   time.Time // когда оверлей истекает; zero = нет

	cmd      *exec.Cmd  // текущий процесс ringled (nil = ничего не запущено)
	done     chan error // завершение текущего процесса
	cmdKey   string     // ключ того, что процесс показывает
	lastFail time.Time  // троттлинг ошибок запуска ringled (повтор не раньше 3 с)
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

// SetError — вспышка ошибки диалога (сбрасывается по таймеру).
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

// SetScale — глобальная яркость от датчика света (0..1) → --brightness.
func (r *Ring) SetScale(s float64) {
	cmd := ringCmd{kind: cmdSetScale, f: s, reply: make(chan any, 1)}
	r.send(&cmd)
}

// SetSolid — цвет для сцены solid (управление кольцом из HA).
func (r *Ring) SetSolid(c Color) {
	cmd := ringCmd{kind: cmdSetSolid, c: c, reply: make(chan any, 1)}
	r.send(&cmd)
}

// Flash — вспышка цветом c длительностью d: перекрывает текущую сцену
// оверлеем (breathe-прогон ringled: плавное нарастание и спад). Синхронный
// вызов: возвращается, когда актор принял команду и применил показ.
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
// Сцену из состояния выводит apply() перед показом. Только в акторе.
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
		r.overlayKey = "flash:" + hex(cmd.c)
		r.overlayArgs = []string{
			"--brightness", "100",
			"--duration", FLASH_MS, "--breathe", FLASH_BREATHE,
			"all", hex(cmd.c),
		}
		r.overlayTt = time.Now().Add(cmd.d)
	}
	return nil
}

// desire — что должен показывать супервизор прямо сейчас:
// key — идентификатор желаемого вывода, args — аргументы ringled,
// respawn — перезапускать ли процесс после завершения (анимации).
// Только в акторе.
func (r *Ring) desire() (key string, args []string, respawn bool) {
	// оверлей громкости — высший приоритет (перекрывает сцены)
	if time.Now().Before(r.overlayTt) {
		return r.overlayKey, r.overlayArgs, false
	}

	pct := brightnessPct(r.scale)
	switch r.resolve() {
	case "solid":
		return "solid@" + pct + ":" + hex(r.solid), []string{"--brightness", pct, "all", hex(r.solid)}, false
	case "muted":
		return "muted@" + pct, []string{"--brightness", pct, "all", MUTED_HEX}, false
	case "error":
		return "error:" + r.dialog, []string{"--brightness", "100", "--duration", ERROR_MS, "--breathe", ERROR_MS, "all", "FF0000"}, true
	case "noconn":
		return "noconn", []string{"--brightness", "30", "--duration", ANIM_DUR_MS, "--breathe", "2000", "all", "640000"}, true
	case "listening":
		return "listening@" + pct, []string{"--brightness", pct, "--duration", ANIM_DUR_MS, "--breathe", "1000", "all", LISTEN_HEX}, true
	case "thinking":
		return "thinking@" + pct, []string{"--brightness", pct, "--duration", ANIM_DUR_MS, "--breathe", "700", "all", THINK_HEX}, true
	case "speaking":
		return "speaking@" + pct, []string{"--brightness", pct, "--duration", ANIM_DUR_MS, "--breathe", "500", "all", SPEAK_HEX}, true
	case "rainbow":
		return "rainbow@" + pct, []string{"--brightness", pct, "--duration", ANIM_DUR_MS, "rainbow"}, true
	default: // off — всё тёмное
		return "off", []string{"off"}, false
	}
}

// Serve — актор: единственная горутина, владеющая состоянием Ring и
// запускающая ringled. Команды приходят через cmdCh, каждая отчитывается
// через свой reply-канал. Блокирует, запускать в горутине.
func (r *Ring) Serve(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("ring: panic: %v", p)
		}
	}()
	// на старте погасить кольцо (защёлкнутый кадр от прежних запусков)
	if err := r.start("off", []string{"off"}); err != nil {
		log.Printf("ring: стартовое гашение: %v", err)
	}

	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			r.killProc()
			// кадр в диодах защёлкнут — погасить кольцо напоследок
			_ = exec.Command(ringledBin, "off").Run()
			return
		case cmd := <-r.cmdCh:
			r.exec(&cmd) // 1. состояние
			r.apply()    // 2. запуск показа
			if cmd.kind == cmdFlash && r.cmd != nil {
				// вспышка — КОНЕЧНАЯ операция: дождаться конца анимации.
				// Актор занят только ею, сцену внутри никто не сменит.
				if err := <-r.done; err != nil {
					log.Printf("ringled %q завершился с ошибкой: %v", r.cmdKey, err)
				}
				r.cmd, r.done = nil, nil
			}
			cmd.reply <- nil // 3. «сделано»: для вспышки — отиграла до конца
		case <-tick.C:
			r.apply() // как showTime у часов: перерисовать, если изменилось
		}
	}
}

// apply — привести показ к желаемому (desire): перезапустить ringled, если
// желаемый вывод отличается от текущего, или оживить процесс после конца
// анимации. ЕДИНСТВЕННАЯ точка запуска ringled в процессе. Вызывается из
// тика и после каждой команды — чтобы reply означал «реально исполняется».
func (r *Ring) apply() {
	key, args, respawn := r.desire()

	// процесс кончился? (истёк --duration / отработал)
	if r.cmd != nil {
		select {
		case err := <-r.done:
			r.cmd, r.done = nil, nil
			if err != nil {
				log.Printf("ringled %q завершился с ошибкой: %v", r.cmdKey, err)
			}
		default:
		}
	}

	if !r.lastFail.IsZero() && time.Since(r.lastFail) < 3*time.Second {
		return // недавняя ошибка запуска — не спамим bind/запусками
	}

	if r.cmd != nil {
		if r.cmdKey == key {
			return // нужный вывод уже идёт (статика или незавершённая анимация)
		}
		r.killProc() // сцена сменилась — убить старый процесс
	} else if key == r.cmdKey && !respawn {
		return // статика уже защёлкнута ранее — процесс не нужен
	}

	if err := r.start(key, args); err != nil {
		log.Printf("ring: %v (повтор через 3с)", err)
		r.lastFail = time.Now()
		return
	}
	r.lastFail = time.Time{}
}

// start — запустить ringled. Только в акторе.
//
// Run() — в фоновой горутине: актор не должен блокироваться на время жизни
// ringled (анимации до 10 с). Канал done буферизован (cap 1) и отправитель
// один, поэтому результат всегда доставляется без блокировки: актор его
// либо прочитает (reconcile/killProc), либо он молча ляжет в буфер — если
// killProc сдался после таймаута 2 с.
func (r *Ring) start(key string, args []string) error {
	if err := r.ensureSPI(); err != nil {
		return err
	}
	cmd := exec.Command(ringledBin, args...)
	done := make(chan error, 1)
	r.cmd, r.done, r.cmdKey = cmd, done, key
	go func() {
		done <- cmd.Run()
	}()
	return nil
}

// ensureSPI — следить, что контроллер meson-spicc привязан и узел жив.
// Два вида мусора после горячих rebind-циклов:
//  1. контроллер не привязан → ringled умирает «no meson-spicc device
//     bound to rebind»;
//  2. узел-висяк: файл есть (Stat проходит!), но inode указывает в никуда
//     после unregister — любое открытие даёт ENOENT/ENXIO.
// Поэтому живость проверяем реальным открытием, а не Stat. Узел создаёт
// mdev по uevent и на горячих циклах тупит — после bind принудительно
// гоняем mdev -s, пока устройство не откроется.
func (r *Ring) ensureSPI() error {
	const (
		driverLink = "/sys/bus/platform/devices/ffd13000.spi/driver"
		bindPath   = "/sys/bus/platform/drivers/meson-spicc/bind"
	)
	alive := func() bool {
		f, err := os.OpenFile(r.dev, os.O_RDWR, 0)
		if err != nil {
			return false
		}
		f.Close()
		return true
	}
	if alive() {
		return nil
	}
	// мёртвый узел мешает mdev создать свежий — снести
	os.Remove(r.dev)
	// привязать контроллер, если драйвер не на месте
	if drv, err := os.Readlink(driverLink); err != nil || !strings.HasSuffix(drv, "/meson-spicc") {
		bind, err := os.OpenFile(bindPath, os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("bind meson-spicc: %w", err)
		}
		bind.WriteString("ffd13000.spi")
		bind.Close()
	}
	// пересканировать узлы; uevent-гонка mdev — главный источник «узел не появился»
	mdev := func() { exec.Command("mdev", "-s").Run() }
	mdev()
	for i := 0; i < 500; i++ { // до 2 с
		time.Sleep(4 * time.Millisecond)
		if alive() {
			return nil
		}
		if i%25 == 0 { // раз в 100 мс повторный mdev -s
			mdev()
		}
	}
	return fmt.Errorf("%s не открылся после bind meson-spicc + mdev -s", r.dev)
}

// killProc — SIGKILL текущему ringled и полное ожидание его смерти
// (следующий запуск возможен только после этого — два ringled на SPI
// одновременно недопустимы). Таймаут 2 с на случай зависания в D-state.
// Только в акторе.
func (r *Ring) killProc() {
	if r.cmd == nil {
		return
	}
	if r.cmd.Process != nil {
		r.cmd.Process.Kill()
	}
	if r.done != nil {
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
			log.Printf("ringled не завершился за 2с — продолжаю без ожидания")
		}
	}
	r.cmd, r.done, r.cmdKey = nil, nil, ""
}
