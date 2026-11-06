//! Порт internal/touch (Go): сенсорная панель VK Mini — Hynitron CSK05T
//! (I2C @0x15), в ядре как evdev-устройство (в DT — keycodes 114, 248, 115,
//! 148, 582). Читаем нажатия из /dev/input/event0.
use std::collections::HashMap;
use std::time::{Duration, Instant};

use tokio::sync::{mpsc, watch};

use crate::{logln, util};

/// Окно дребезга: повторный код кнопки раньше этого срока считается тем же
/// нажатием и отбрасывается.
pub const DEBOUNCE: Duration = Duration::from_millis(50);

/// Читает нажатия (value=1) и отдаёт keycode'ы в канал.
/// Устройство может появиться позже (модуль I2C) — переподключение с паузой.
pub fn start(ctx: watch::Receiver<bool>, dev: &'static str) -> mpsc::Receiver<u16> {
    let (tx, rx) = mpsc::channel(8);
    tokio::spawn(async move {
        let mut last: HashMap<u16, Instant> = HashMap::new(); // последний принятый код
        let mut ctx = ctx;
        loop {
            if crate::util::cancelled(&ctx) {
                return;
            }
            let device = match evdev::Device::open(dev) {
                Ok(d) => d,
                Err(e) => {
                    logln!("touch: {}: {} (повтор через 3с)", dev, e);
                    util::sleep_ctx(&mut ctx, Duration::from_secs(3)).await;
                    continue;
                }
            };
            let name = device.name().unwrap_or("<неизвестно>").to_string();
            logln!("touch: {} = {}", dev, name);
            let mut stream = match device.into_event_stream() {
                Ok(s) => s,
                Err(e) => {
                    logln!("touch: {}: {} (повтор через 3с)", dev, e);
                    util::sleep_ctx(&mut ctx, Duration::from_secs(3)).await;
                    continue;
                }
            };
            loop {
                tokio::select! {
                    _ = ctx.changed() => {
                        if crate::util::cancelled(&ctx) { return; }
                    }
                    ev = stream.next_event() => {
                        let Ok(ev) = ev else {
                            logln!("touch: чтение: конец потока (переподключение)");
                            break;
                        };
                        if ev.event_type() == evdev::EventType::KEY && ev.value() == 1 {
                            let code = ev.code();
                            // дребезг: тот же код раньше 50 мс — то же нажатие
                            if let Some(t) = last.get(&code) {
                                if t.elapsed() < DEBOUNCE {
                                    continue;
                                }
                            }
                            last.insert(code, Instant::now());
                            if tx.send(code).await.is_err() {
                                return; // потребитель ушёл
                            }
                        }
                    }
                }
            }
            util::sleep_ctx(&mut ctx, Duration::from_secs(3)).await;
        }
    });
    rx
}
