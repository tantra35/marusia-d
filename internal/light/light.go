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
		// Окно света у колонки слабое: комната 0.02..3.2 lux (замер
		// 2026-11-06). Дефолт x1 даёт 1.3 lux на отсчёт — темнота и лампы
		// слипаются в 0..2 отсчёта. x128 + интеграция 688 мс = 0.001 lux
		// на отсчёт (шум ±0.01). Лучшее усилие: без этого датчик почти слеп.
		for _, s := range [][2]string{
			{"hardwaregain", "128"},
			{"integration_time", "0.688296"},
		} {
			if err := os.WriteFile(filepath.Join(dir, s[0]), []byte(s[1]), 0o644); err != nil {
				log.Printf("light: %s: %v", s[0], err)
			}
		}
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
// (в темноте приглушаем, при 3+ лк — полный ход). Пороги под фактический
// диапазон окна: ночь ~0.02, лампы ~3.2 (замер 2026-11-06, x128).
// Без гистерезиса: EMA в main сглаживает скачки.
func Scale(lux float64) float64 {
	const lo, hi = 0.3, 3.0
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
	const lo, hi, loPWM, hiPWM = 0.3, 3.0, 12, 255
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
// Пороги под фактический диапазон окна (ночь ~0.02, лампы ~3.2).
func ClockCurrent(lux float64) int {
	switch {
	case lux >= 2.0:
		return 1
	case lux >= 1.0:
		return 2
	case lux >= 0.3:
		return 3
	default:
		return 4
	}
}
