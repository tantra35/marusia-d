package ring

// SPI-транспорт кольца: rebind контроллера + отправка кадра через spidev.
//
// Зависимости от железа (экспериментально проверено 2026-11-06, живая полоса):
//   - бит → 4 SPI-бита: 0 → 1000b, 1 → 1100b («1»=1110 даёт high 0.9 мкс —
//     вне спекции WS2812 и декодируется полосой нестабильно);
//   - каналы внутри диода в порядке байт, как шлём (для RGB-цвета — G,R,B);
//   - кадр = 128 reset + 144 байта (12 диодов × 12 байт) + 128 reset = 400;
//   - speed 3 346 144 Гц, bits_per_word=8 (64-битный DMA переставляет байты
//     внутри слов — каналы скачмаплены);
//   - rebind контроллера (unbind/bind) перед КАЖДЫМ кадром: делитель SPI
//     корректен только после probe, мульти-кадровые сессии не живут.
//
// Все вызовы — только из актора (Serve).
import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

const (
	spiDev       = "/dev/spidev0.0"
	frameHz      = 3346144
	primeHz      = 1000000
	primeLen     = 16
	resetLen     = 128
	bytesPerLED  = 12 // 24 бита × 4 SPI-бита / 8
	spiMajor     = 153
	spiMinor     = 0
	rebindSettle = 15 * time.Millisecond // пауза unbind→bind
)

// encodeByte — байт → 4 байта SPI: бит 0 → 1000b, бит 1 → 1100b.
func encodeByte(b byte) [4]byte {
	var out uint32
	for i := 7; i >= 0; i-- {
		n := byte(0x8)
		if b>>uint(i)&1 == 1 {
			n = 0xC
		}
		out = out<<4 | uint32(n)
	}
	return [4]byte{byte(out >> 24), byte(out >> 16), byte(out >> 8), byte(out)}
}

// encodeFrame — кадр целиком: reset + GRB-пиксели + reset.
func encodeFrame(px [leds][3]byte) []byte {
	f := make([]byte, 0, resetLen*2+leds*bytesPerLED)
	f = append(f, make([]byte, resetLen)...)
	for _, p := range px {
		for _, ch := range []byte{p[1], p[0], p[2]} { // GRB
			e := encodeByte(ch)
			f = append(f, e[:]...)
		}
	}
	f = append(f, make([]byte, resetLen)...)
	return f
}

// spiXfer — struct spi_ioc_transfer, uapi-порядок полей (bits_per_word ПЕРЕД
// cs_change), 32 байта — сверено с ядром 6.18.
type spiXfer struct {
	txBuf uint64
	rxBuf uint64
	len   uint32
	speed uint32
	delay uint16
	bits  uint8
	cs    uint8
	txN   uint8
	rxN   uint8
	pad   uint16
}

func iocMessage(n int) uintptr {
	return 0x40000000 | uintptr(n*32)<<16 | 0x6B<<8 | 0
}

// draw — rebind + открыть узел + отправить кадр. Блокирует ~30–60 мс.
func (r *Ring) draw(frame []byte) error {
	// unbind: «уже отвязан» игнорируем; bind: свежий probe → живой делитель
	os.WriteFile("/sys/bus/platform/drivers/meson-spicc/unbind", []byte("ffd13000.spi"), 0644)
	time.Sleep(rebindSettle)
	if err := os.WriteFile("/sys/bus/platform/drivers/meson-spicc/bind", []byte("ffd13000.spi"), 0644); err != nil {
		return fmt.Errorf("bind meson-spicc: %w", err)
	}

	f, err := r.openDev()
	if err != nil {
		return err
	}
	defer f.Close()

	prime := make([]byte, primeLen)
	tx := []spiXfer{
		{txBuf: uint64(uintptr(unsafe.Pointer(&prime[0]))), len: primeLen, speed: primeHz, bits: 8},
		{txBuf: uint64(uintptr(unsafe.Pointer(&frame[0]))), len: uint32(len(frame)), speed: frameHz, bits: 8},
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), iocMessage(len(tx)), uintptr(unsafe.Pointer(&tx[0]))); e != 0 {
		return fmt.Errorf("SPI_IOC_MESSAGE: %w", e)
	}
	return nil
}

// openDev — открыть узел; после rebind mdev может не успеть его создать —
// создаём сами (mknod), висячий старый узел сносим.
func (r *Ring) openDev() (*os.File, error) {
	for attempt := 0; attempt < 10; attempt++ {
		f, err := os.OpenFile(spiDev, os.O_RDWR, 0)
		if err == nil {
			return f, nil
		}
		os.Remove(spiDev)
		if err := syscall.Mknod(spiDev, 0620|syscall.S_IFCHR, int(mkdev(spiMajor, spiMinor))); err != nil {
			// mknod может не удаться — дать шанс mdev
			exec.Command("mdev", "-s").Run()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, fmt.Errorf("%s не открылся после rebind", spiDev)
}

func mkdev(major, minor uint32) uint64 {
	return uint64(minor)&0xff | uint64(major)&0xfff<<8 | uint64(minor)&0xfff00<<12 | uint64(major)&0xfff00<<32
}
