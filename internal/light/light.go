// Package light — датчик освещённости Rohm BH1730 (I2C @0x29),
// в ядре как IIO-устройство: /sys/bus/iio/devices/iio:deviceN,
// lux читается из in_illuminance_input (люкс уже посчитан драйвером).
package light

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Start опрашивает датчик раз в interval и шлёт lux в канал.
// Если датчика нет — канал закрывается и поток завершается (не критично).
func Start(ctx context.Context, interval time.Duration) <-chan float64 {
	luxCh := make(chan float64, 4)
	go func() {
		dir := findBH1730()
		if dir == "" {
			log.Printf("light: bh1730 в /sys/bus/iio не найден — автояркость выключена")
			return
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b, err := os.ReadFile(filepath.Join(dir, "in_illuminance_input"))
				if err != nil {
					continue
				}
				lux, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
				if err != nil {
					continue
				}
				select {
				case luxCh <- lux:
				default: // получатель не успел — пропускаем кадр
				}
			}
		}
	}()
	return luxCh
}

func findBH1730() string {
	m, _ := filepath.Glob("/sys/bus/iio/devices/iio:device*")
	for _, d := range m {
		b, err := os.ReadFile(filepath.Join(d, "name"))
		if err == nil && strings.TrimSpace(string(b)) == "bh1730" {
			return d
		}
	}
	return ""
}

// Scale — lux → множитель яркости кольца 0.15..1.0
// (в темноте приглушаем, при 300+ лк — полный ход). Без гистерезиса:
// EMA в main сглаживает скачки.
func Scale(lux float64) float64 {
	const lo, hi = 5.0, 300.0
	switch {
	case lux <= lo:
		return 0.15
	case lux >= hi:
		return 1.0
	default:
		return 0.15 + 0.85*(lux-lo)/(hi-lo)
	}
}

// ClockPWM — lux → яркость панели-часов 0..255. Как Scale, но минимум
// глубже (12/255): ночью часы не должны светить в потолок.
func ClockPWM(lux float64) int {
	const lo, hi, loPWM, hiPWM = 5.0, 300.0, 12, 255
	switch {
	case lux <= lo:
		return loPWM
	case lux >= hi:
		return hiPWM
	default:
		return loPWM + int(float64(hiPWM-loPWM)*(lux-lo)/(hi-lo))
	}
}

// ClockCurrent — lux → current_divider 1..4 (1 ярче всего, 4 — ночной режим).
// Токовые ступени режут ток LED сами по себе, независимо от PWM.
func ClockCurrent(lux float64) int {
	switch {
	case lux >= 100:
		return 1
	case lux >= 20:
		return 2
	case lux >= 5:
		return 3
	default:
		return 4
	}
}
