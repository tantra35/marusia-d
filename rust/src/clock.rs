//! Порт internal/clock (Go): передняя панель VK Mini — 4×7-сегментных цифры +
//! двоеточие на IS31FL3236A (I2C 0x3C), через кастомный seg-led класс ядра.
//! Логика повторяет vendor-скрипт clockdisp: текст — запись в linedisp/message,
//! произвольные маски сегментов — патч 4 управляющих кодов в map_seg7,
//! двоеточие — обычные leds (clock:colon:top/bottom).
use std::io;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU8, Ordering};
use std::sync::Mutex;
use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio::sync::{mpsc, watch};

use crate::{logln, util};

/// Сколько на панели держатся проценты громкости перед возвратом к часам.
pub const VOLUME_SHOW: Duration = Duration::from_millis(1500);
/// 350 мс на лицо (как в clockdisp).
const FACE_MS: u64 = 350;

// Маски сегментов: a=0x01 b=0x02 c=0x04 d=0x08 e=0x10 f=0x20 g=0x40
// (порядок бит из map_to_7segment.h, как в clockdisp).
pub const EYE_ARCH: u8 = 0x23; // a b f — открытый глаз, покой
pub const EYE_LINE: u8 = 0x40; // g     — прищур
pub const EYE_LOW: u8 = 0x08; // d     — сонный
pub const EYE_HIGH: u8 = 0x01; // a     — поднятая бровь
pub const EYE_WIDE: u8 = 0x3f; // 0     — широко
pub const EYE_ROUND: u8 = 0x5c; // o     — маленький круглый
pub const ANGRY_EYE_L: u8 = 0x21; // a f   — бровь к носу
pub const ANGRY_EYE_R: u8 = 0x03; // a b   — зеркало
const BLANK: u8 = 0x00;

/// Пути в sysfs. open() находит дисплей так же, как clockdisp.
/// Возвращает (часы, приёмник сигнала «анимация лиц закончилась»).
pub fn open() -> io::Result<(Arc<Clock>, mpsc::Receiver<()>)> {
    let mut disp: Option<String> = None;
    let patterns = [
        "/sys/class/linedisp/linedisp.*",
        "/sys/bus/platform/devices/*/linedisp.*",
        "/sys/devices/platform/*/linedisp.*",
    ];
    'outer: for p in patterns {
        let Ok(m) = glob::glob(p) else { continue };
        for d in m.flatten() {
            if d.join("message").exists() {
                disp = Some(d.to_string_lossy().into_owned());
                break 'outer;
            }
        }
    }
    let Some(disp) = disp else {
        return Err(io::Error::new(
            io::ErrorKind::NotFound,
            "linedisp не найден: CONFIG_SEG_LED_CLASS в ядре?",
        ));
    };
    let disp = PathBuf::from(disp);

    // яркость — на родительской платформе (уровень панели)
    let mut bright_file: Option<PathBuf> = None;
    for b in [disp.join("../brightness"), disp.join("device/brightness")] {
        if let Ok(fi) = std::fs::metadata(&b) {
            if !fi.is_dir() {
                bright_file = Some(b);
                break;
            }
        }
    }
    // current divider — на i2c-клиенте (is31fl3236a)
    let cur_div = glob::glob("/sys/bus/i2c/devices/*/current_divider")
        .ok()
        .and_then(|m| m.flatten().next());

    let (face_done_tx, face_done_rx) = mpsc::channel(1);
    let c = Clock {
        message: disp.join("message"),
        map_seg: disp.join("map_seg7"),
        bright_file,
        colon_top: find_led("clock:colon:top"),
        colon_bot: find_led("clock:colon:bottom"),
        cur_div,
        bright: AtomicU8::new(255),
        anim: AtomicBool::new(false),
        face_done_tx,
        vol_deadline: Mutex::new(None),
    };
    Ok((Arc::new(c), face_done_rx))
}

fn find_led(name: &str) -> Option<PathBuf> {
    let p = PathBuf::from("/sys/class/leds").join(name).join("brightness");
    p.exists().then_some(p)
}

/// Guard анимации: anim снимается при любом выходе (аналог defer в Go).
struct AnimGuard<'a>(&'a AtomicBool);
impl Drop for AnimGuard<'_> {
    fn drop(&mut self) {
        self.0.store(false, Ordering::SeqCst);
    }
}

pub struct Clock {
    message: PathBuf,
    map_seg: PathBuf,
    bright_file: Option<PathBuf>,
    colon_top: Option<PathBuf>,
    colon_bot: Option<PathBuf>,
    cur_div: Option<PathBuf>,
    /// уровень панели (PWM 0..255) — запоминаем: им же зажигается «нос» лица
    bright: AtomicU8,
    /// идёт анимация лиц — serve панель не трогает
    anim: AtomicBool,
    face_done_tx: mpsc::Sender<()>,
    /// отложенный возврат к часам после показа громкости
    vol_deadline: Mutex<Option<Instant>>,
}

impl Clock {
    /// Показать текст, выравнивание вправо на 4 позиции (как clockdisp).
    /// Формат "12:34" зажигает двоеточие.
    pub fn set_text(&self, text: &str) -> io::Result<()> {
        let want_colon = text.contains(':');
        let clean: String = text.chars().filter(|&c| c != ':').collect();
        // Go: fmt.Sprintf("%4.4s", text) — первые 4 знака, вправо
        let t: String = clean.chars().take(4).collect();
        let s = format!("{:>4}", t);
        write_str(&self.message, &s)?;
        self.colon(want_colon)
    }

    /// Произвольные маски на 4 цифры: патчим 4 неиспользуемых управляющих
    /// кода (0x01..0x04) в map_seg7 и печатаем их.
    pub fn show_masks(&self, m: [u8; 4]) -> io::Result<()> {
        let mut raw = std::fs::read(&self.map_seg).map_err(|e| {
            io::Error::new(e.kind(), format!("map_seg7: нужно ровно 128 байт, {}", e))
        })?;
        if raw.len() != 128 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("map_seg7: нужно ровно 128 байт, получено {}", raw.len()),
            ));
        }
        for (i, v) in m.iter().enumerate() {
            raw[1 + i] = *v;
        }
        write_bytes(&self.map_seg, &raw)?;
        write_str(&self.message, "\u{1}\u{2}\u{3}\u{4}")
    }

    /// Лицо из clockdisp: два глаза на средних цифрах, нос — нижняя точка.
    pub fn face(&self, name: &str) -> io::Result<()> {
        let Some((l, r)) = face_eyes(name) else {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("нет лица {:?}", name),
            ));
        };
        self.show_masks([BLANK, l, r, BLANK])?;
        if let (Some(top), Some(bot)) = (&self.colon_top, &self.colon_bot) {
            write_str(top, "0")?;
            // нос
            return write_str(bot, &self.bright.load(Ordering::Relaxed).to_string());
        }
        Ok(())
    }

    /// Проиграть лица по очереди (350 мс на лицо) в фоновой задаче.
    /// С начала анимации serve панель не трогает; по завершении возвращает часы.
    pub fn play_faces(self: &Arc<Self>, mut ctx: watch::Receiver<bool>, faces: &[&str]) {
        let faces: Vec<String> = faces.iter().map(|f| f.to_string()).collect();
        let me = Arc::clone(self);
        tokio::spawn(async move {
            me.anim.store(true, Ordering::SeqCst);
            let _guard = AnimGuard(&me.anim);
            for f in &faces {
                if let Err(e) = me.face(f) {
                    logln!("clock: лицо {}: {}", f, e);
                    return;
                }
                tokio::select! {
                    _ = ctx.changed() => {
                        if crate::util::cancelled(&ctx) { return; }
                    }
                    _ = tokio::time::sleep(Duration::from_millis(FACE_MS)) => {}
                }
            }
            let _ = me.face_done_tx.try_send(());
        });
    }

    /// Показать громкость в процентах вместо часов; через VOLUME_SHOW вернуть
    /// часы. Во время анимации лиц панель не трогаем (закон занятости).
    pub fn show_volume(&self, pct: i32) {
        if self.anim.load(Ordering::SeqCst) {
            return;
        }
        if let Err(e) = self.set_text(&pct.to_string()) {
            logln!("clock: громкость {}: {}", pct, e);
            return;
        }
        *self.vol_deadline.lock().unwrap() = Some(Instant::now() + VOLUME_SHOW);
    }

    /// Фоновый цикл панели: держать актуальное время (проверка каждую секунду,
    /// перерисовка при смене минуты). Пока идёт анимация лиц (anim), панель не
    /// трогает; по face_done — возвращает часы.
    pub async fn serve(&self, mut ctx: watch::Receiver<bool>, mut face_done: mpsc::Receiver<()>) {
        let mut last = String::new();
        self.show_time();
        let mut tick = tokio::time::interval(Duration::from_secs(1));
        loop {
            // дедлайн возврата к часам после ShowVolume; None = нет
            let deadline = self.vol_deadline.lock().unwrap().take();
            tokio::select! {
                _ = ctx.changed() => {
                    if crate::util::cancelled(&ctx) { return; }
                }
                Some(_) = face_done.recv() => {
                    // анимация закончилась — вернуть часы немедленно
                    self.show_time();
                    last = util::hm();
                }
                _ = tokio::time::sleep_until(tokio::time::Instant::from_std(
                    deadline.unwrap_or_else(Instant::now),
                )), if deadline.is_some() => {
                    self.show_time();
                }
                _ = tick.tick() => {
                    if self.anim.load(Ordering::SeqCst) {
                        continue; // играет лицо — не затирать
                    }
                    let now = util::hm();
                    if now != last {
                        last = now;
                        self.show_time();
                    }
                }
            }
        }
    }

    fn show_time(&self) {
        if let Err(e) = self.set_text(&util::hm()) {
            logln!("clock: {}", e);
        }
    }

    /// Двоеточие on/off.
    pub fn colon(&self, on: bool) -> io::Result<()> {
        let v = if on {
            self.bright.load(Ordering::Relaxed).to_string()
        } else {
            "0".to_string()
        };
        for p in [&self.colon_top, &self.colon_bot] {
            if let Some(p) = p {
                write_str(p, &v)?;
            }
        }
        Ok(())
    }

    /// Погасить всё.
    pub fn off(&self) -> io::Result<()> {
        write_str(&self.message, "    ")?;
        self.colon(false)
    }

    /// PWM 0..255 на панель.
    pub fn set_brightness(&self, v: u8) -> io::Result<()> {
        let Some(p) = &self.bright_file else {
            return Err(io::Error::new(io::ErrorKind::NotFound, "нет файла яркости"));
        };
        self.bright.store(v, Ordering::Relaxed);
        write_str(p, &v.to_string())
    }

    /// Аналоговый ток 1..4 (1 ярче всего; 4 — тихий режим).
    pub fn set_current(&self, v: i32) -> io::Result<()> {
        let Some(p) = &self.cur_div else {
            return Err(io::Error::new(io::ErrorKind::NotFound, "нет current_divider"));
        };
        if !(1..=4).contains(&v) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("current 1..4, получено {}", v),
            ));
        }
        write_str(p, &v.to_string())
    }
}

fn face_eyes(name: &str) -> Option<(u8, u8)> {
    match name {
        "idle" => Some((EYE_ARCH, EYE_ARCH)),
        "neutral" => Some((EYE_LINE, EYE_LINE)),
        "surprised" => Some((EYE_WIDE, EYE_WIDE)),
        "curious" => Some((EYE_ROUND, EYE_ROUND)),
        "sleepy" => Some((EYE_LOW, EYE_LOW)),
        "shock" => Some((EYE_HIGH, EYE_HIGH)),
        "wink" => Some((EYE_ARCH, EYE_LINE)),
        "angry" => Some((ANGRY_EYE_L, ANGRY_EYE_R)),
        _ => None,
    }
}

fn write_str(path: &std::path::Path, s: &str) -> io::Result<()> {
    std::fs::write(path, s)
}

fn write_bytes(path: &std::path::Path, b: &[u8]) -> io::Result<()> {
    std::fs::write(path, b)
}
