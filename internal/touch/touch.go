// Package touch — сенсорная панель VK Mini: Hynitron CSK05T (I2C @0x15),
// в ядре как evdev-устройство (в DT — keycodes 114, 248, 115, 148, 582).
// Читаем нажатия из /dev/input/event0.
package touch

import (
	"context"
	"log"
	"time"

	"github.com/holoplot/go-evdev"
)

// DEBOUNCE_MS — окно дребезга: повторный код кнопки раньше этого срока
// считается тем же нажатием и отбрасывается.
const DEBOUNCE_MS = 50 * time.Millisecond

// Start читает нажатия (value=1) и отдаёт keycode'ы в канал.
// Устройство может появиться позже (модуль I2C) — переподключение с паузой.
func Start(ctx context.Context, dev string) <-chan uint16 {
	codeCh := make(chan uint16, 8)
	go func() {
		last := make(map[uint16]time.Time) // последний принятый код
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			d, err := evdev.Open(dev)
			if err != nil {
				log.Printf("touch: %s: %v (повтор через 3с)", dev, err)
				sleepCtx(ctx, 3*time.Second)
				continue
			}
			name, _ := d.Name()
			log.Printf("touch: %s = %s", dev, name)
			for {
				e, err := d.ReadOne()
				if err != nil {
					d.Close()
					log.Printf("touch: чтение: %v (переподключение)", err)
					break
				}
				if e.Type == evdev.EV_KEY && e.Value == 1 {
					code := uint16(e.Code)
					// дребезг: тот же код раньше 50 мс — то же нажатие
					if t, ok := last[code]; ok && time.Since(t) < DEBOUNCE_MS {
						continue
					}
					last[code] = time.Now()
					select {
					case codeCh <- code:
					case <-ctx.Done():
						d.Close()
						return
					}
				}
			}
			sleepCtx(ctx, 3*time.Second)
		}
	}()
	return codeCh
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
