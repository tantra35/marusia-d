//! Порт internal/light (Go): датчик Rohm BH1730 (I2C @0x29) через IIO sysfs,
//! lux читается из in_illuminance_input (люкс уже посчитан драйвером).
use std::path::PathBuf;
use std::time::Duration;

use tokio::sync::{mpsc, watch};

use crate::logln;

fn find_bh1730() -> Option<PathBuf> {
    for entry in glob::glob("/sys/bus/iio/devices/iio:device*").ok()? {
        let Ok(d) = entry else { continue };
        if let Ok(name) = std::fs::read_to_string(d.join("name")) {
            if name.trim() == "bh1730" {
                return Some(d);
            }
        }
    }
    None
}

/// Опрашивать датчик раз в interval и отдавать lux. Если датчика нет —
/// канал закрывается и поток завершается (не критично).
pub fn start(ctx: watch::Receiver<bool>, interval: Duration) -> mpsc::Receiver<f64> {
    let (tx, rx) = mpsc::channel(4);
    tokio::spawn(async move {
        let Some(dir) = find_bh1730() else {
            logln!("light: bh1730 в /sys/bus/iio не найден — автояркость выключена");
            return;
        };
        let mut tick = tokio::time::interval(interval);
        let mut ctx = ctx;
        loop {
            tokio::select! {
                _ = ctx.changed() => {
                    if crate::util::cancelled(&ctx) { return; }
                }
                _ = tick.tick() => {
                    let Ok(s) = tokio::fs::read_to_string(dir.join("in_illuminance_input")).await
                        else { continue };
                    let Ok(lux) = s.trim().parse::<f64>() else { continue };
                    // получатель не успел — пропускаем кадр
                    let _ = tx.try_send(lux);
                }
            }
        }
    });
    rx
}

/// lux → множитель яркости кольца 0.15..1.0 (в темноте приглушаем, при 300+ лк —
/// полный ход). Без гистерезиса: EMA в main сглаживает скачки.
pub fn scale(lux: f64) -> f64 {
    const LO: f64 = 5.0;
    const HI: f64 = 300.0;
    if lux <= LO {
        0.15
    } else if lux >= HI {
        1.0
    } else {
        0.15 + 0.85 * (lux - LO) / (HI - LO)
    }
}

/// lux → яркость панели-часов 0..255. Как scale, но минимум глубже (12/255):
/// ночью часы не должны светить в потолок.
pub fn clock_pwm(lux: f64) -> i32 {
    const LO: f64 = 5.0;
    const HI: f64 = 300.0;
    const LO_PWM: i32 = 12;
    const HI_PWM: i32 = 255;
    if lux <= LO {
        LO_PWM
    } else if lux >= HI {
        HI_PWM
    } else {
        LO_PWM + ((HI_PWM - LO_PWM) as f64 * (lux - LO) / (HI - LO)) as i32
    }
}

/// lux → current_divider 1..4 (1 ярче всего, 4 — ночной режим).
pub fn clock_current(lux: f64) -> i32 {
    if lux >= 100.0 {
        1
    } else if lux >= 20.0 {
        2
    } else if lux >= 5.0 {
        3
    } else {
        4
    }
}
