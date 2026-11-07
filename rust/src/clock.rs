//! Порт internal/clock (Go): передняя панель VK Mini — 4×7-сегментных цифры +
//! двоеточие на IS31FL3236A (I2C 0x3C), через кастомный seg-led класс ядра.
//! Логика повторяет vendor-скрипт clockdisp: текст — запись в linedisp/message,
//! произвольные маски сегментов — глифы в map_seg7 ('a'..'h', вписываются один
//! раз), двоеточие — обычные leds (clock:colon:top/bottom).
//!
//! Архитектура — АКТОР (как ring.Ring): единственная задача (serve) владеет
//! всем состоянием панели и ЕДИНСТВЕННАЯ пишет в sysfs. Любое действие —
//! команда через канал (play_faces/show_volume/set_brightness/set_current);
//! мьютексов, флагов занятости и фоновых задач нет — параллельные анимации
//! невозможны структурно. Шаг состояния — по тику 100 мс: режим определяет,
//! что на панели (время / лицо / громкость), запись происходит только при
//! смене содержимого.
use std::io;
use std::path::PathBuf;
use std::time::{Duration, Instant};

use chrono::Timelike;
use tokio::sync::{mpsc, watch};

use crate::logln;

/// Сколько на панели держатся проценты громкости перед возвратом к часам.
pub const VOLUME_SHOW: Duration = Duration::from_millis(1500);
/// Длительность одного лица в анимации.
const FACE_MS: Duration = Duration::from_millis(350);
/// Тик актора (шаг состояния панели).
const UPDATE_INTERVAL: Duration = Duration::from_millis(100);

/// Глифы лиц: буква → маска сегментов. Буква и есть код в map_seg7 (ASCII),
/// маски вписываются в таблицу ОДИН раз под эти коды. Буквы выбраны из
/// символов, которые часы никогда не печатают (текст — только '0'..'9' и
/// пробел), поэтому обычный вывод времени не затрагивается. Кадр лица — одна
/// запись 4 байт в message (' ' + буква + буква + ' '; пробел в дефолтной
/// таблице = пустая цифра).
///
/// Маски сегментов: a=0x01 b=0x02 c=0x04 d=0x08 e=0x10 f=0x20 g=0x40
/// (порядок бит из map_to_7segment.h, как в clockdisp).
const GLYPH_MASKS: [(u8, u8); 8] = [
    (b'a', 0x23), // a b f — открытый глаз, покой
    (b'b', 0x40), // g     — прищур
    (b'c', 0x08), // d     — сонный
    (b'd', 0x01), // a     — поднятая бровь
    (b'e', 0x3f), // 0     — широко
    (b'f', 0x5c), // o     — маленький круглый
    (b'g', 0x21), // a f   — бровь к носу
    (b'h', 0x03), // a b   — зеркало
];

/// Режим панели — что сейчас должно быть нарисовано.
#[derive(PartialEq, Clone, Copy)]
enum Mode {
    /// часы ЧЧ:ММ
    Time,
    /// анимация лиц
    Faces,
    /// проценты громкости
    Volume,
}

/// Команды актору. Fire-and-forget: подтверждение не нужно — запись sysfs
/// занимает миллисекунды, ошибки актор логирует сам.
enum Cmd {
    PlayFaces(Vec<String>),
    ShowVolume(i32),
    SetBrightness(u8),
    SetCurrent(i32),
}

/// Пути в sysfs (open() находит дисплей так же, как clockdisp) + хэндл
/// команд актору. Clone — можно раздать во все задачи, как ring::Ring.
#[derive(Clone)]
pub struct Clock {
    tx: mpsc::UnboundedSender<Cmd>,
}

impl Clock {
    /// Найти дисплей, создать актора и запустить его задачей (аналог
    /// Open + go Serve в Go).
    pub fn open(ctx: watch::Receiver<bool>) -> io::Result<Clock> {
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

        let (tx, rx) = mpsc::unbounded_channel();
        let actor = Actor {
            message: disp.join("message"),
            map_seg: disp.join("map_seg7"),
            bright_file,
            colon_top: find_led("clock:colon:top"),
            colon_bot: find_led("clock:colon:bottom"),
            cur_div,
            bright: 255,
            cur: 0,
            bright_dirty: false,
            cur_dirty: false,
            mode: Mode::Time,
            faces: Vec::new(),
            face_idx: 0,
            face_next: None,
            vol_pct: 0,
            vol_drawn: false,
            vol_until: None,
            last_min_bucket: -1, // −1 → первый render нарисует часы
            seg_map: [0; 128],
            seg_have: false,
        };
        tokio::spawn(actor.serve(rx, ctx));
        Ok(Clock { tx })
    }

    /// Проиграть лица по очереди (FACE_MS на лицо). Команда актору: последний
    /// запрос побеждает — новая анимация заменяет текущую, а не отбрасывается.
    /// По завершении актор сам возвращает часы.
    pub fn play_faces(&self, faces: &[&str]) {
        let _ = self
            .tx
            .send(Cmd::PlayFaces(faces.iter().map(|f| f.to_string()).collect()));
    }

    /// Показать громкость в процентах вместо часов; через VOLUME_SHOW актор
    /// вернёт часы.
    pub fn show_volume(&self, pct: i32) {
        let _ = self.tx.send(Cmd::ShowVolume(pct));
    }

    /// PWM 0..255 на панель. Двоеточие — отдельный LED, PWM родителя его не
    /// накрывает (vendor clockdisp тоже пишет его абсолютным значением): актор
    /// после смены яркости синхронизирует горящее двоеточие, иначе оно отстаёт
    /// до следующей перерисовки минутного тика (ловилось: цифры яркие —
    /// двоеточие тусклое).
    pub fn set_brightness(&self, v: u8) {
        let _ = self.tx.send(Cmd::SetBrightness(v));
    }

    /// Аналоговый ток 1..4 (1 ярче всего; 4 — тихий режим).
    pub fn set_current(&self, v: i32) {
        let _ = self.tx.send(Cmd::SetCurrent(v));
    }
}

fn find_led(name: &str) -> Option<PathBuf> {
    let p = PathBuf::from("/sys/class/leds").join(name).join("brightness");
    p.exists().then_some(p)
}

/// Актор: единственная задача, владеющая состоянием панели и пишущая в sysfs.
struct Actor {
    // пути (после open не меняются)
    message: PathBuf,
    map_seg: PathBuf,
    bright_file: Option<PathBuf>,
    colon_top: Option<PathBuf>,
    colon_bot: Option<PathBuf>,
    cur_div: Option<PathBuf>,

    // состояние панели — доступно ТОЛЬКО из serve
    /// уровень панели (PWM 0..255) — запоминаем: им же зажигается «нос» лица
    bright: u8,
    /// current_divider 1..4 (на панель попадёт render'ом)
    cur: i32,
    /// яркость изменилась — render перезапишет
    bright_dirty: bool,
    /// current изменился — render перезапишет
    cur_dirty: bool,

    mode: Mode,
    /// Mode::Faces: очередь лиц
    faces: Vec<String>,
    /// Mode::Faces: индекс следующего лица
    face_idx: usize,
    /// Mode::Faces: когда рисовать следующее лицо
    face_next: Option<Instant>,
    /// Mode::Volume: какие проценты рисовать
    vol_pct: i32,
    /// Mode::Volume: проценты уже на панели
    vol_drawn: bool,
    /// Mode::Volume: когда вернуть часы
    vol_until: Option<Instant>,

    /// дедуп минут без пересборки строки: номер минуты Unix()/60; −1 = ещё не рисовали
    last_min_bucket: i64,

    /// кэш map_seg7: читается один раз (регистрация глифов)
    seg_map: [u8; 128],
    /// глифы уже вписаны в таблицу
    seg_have: bool,
}

impl Actor {
    /// Актор: единственная задача, владеющая состоянием панели и рисующая её
    /// содержимое по тику. Команды приходят через rx. При отмене гасит панель.
    async fn serve(
        mut self,
        mut rx: mpsc::UnboundedReceiver<Cmd>,
        mut ctx: watch::Receiver<bool>,
    ) {
        let mut tick = tokio::time::interval(UPDATE_INTERVAL);
        loop {
            tokio::select! {
                _ = ctx.changed() => {
                    if crate::util::cancelled(&ctx) {
                        // панель гасим сами при остановке — отдельной off не нужно
                        self.blank();
                        return;
                    }
                }
                Some(cmd) = rx.recv() => {
                    // 1. состояние, 2. кадр (exec только меняет состояние —
                    // единственный писатель render, задержки не добавляет)
                    self.exec(cmd);
                    self.render();
                }
                _ = tick.tick() => {
                    self.render();
                }
            }
        }
    }

    /// Исполнить команду: ТОЛЬКО поменять состояние, ничего не писать.
    fn exec(&mut self, cmd: Cmd) {
        match cmd {
            Cmd::PlayFaces(faces) => {
                self.mode = Mode::Faces;
                self.faces = faces;
                self.face_idx = 0;
                self.face_next = None; // None → render нарисует сразу
            }
            Cmd::ShowVolume(pct) => {
                self.mode = Mode::Volume;
                self.vol_pct = pct;
                self.vol_drawn = false;
                self.vol_until = Some(Instant::now() + VOLUME_SHOW);
            }
            Cmd::SetBrightness(v) => {
                self.bright = v;
                self.bright_dirty = true;
            }
            Cmd::SetCurrent(v) => {
                if !(1..=4).contains(&v) {
                    logln!("clock: current {}: допустимо 1..4", v);
                    return;
                }
                self.cur = v;
                self.cur_dirty = true;
            }
        }
    }

    /// Шаг состояния панели и ЕДИНСТВЕННАЯ точка записи в sysfs: сперва
    /// доводит до устройства отложенные команды (яркость, ток), затем рисует
    /// то, что должно быть на панели (лица / проценты / часы). В отличие от
    /// ring, кадр строится не каждый тик — запись только при смене
    /// содержимого (дедуп минутой, vol_drawn, face_next).
    fn render(&mut self) {
        let now = Instant::now();
        if self.bright_dirty {
            self.bright_dirty = false;
            if let Err(e) = self.write_brightness() {
                logln!("clock: яркость {}: {}", self.bright, e);
            }
        }
        if self.cur_dirty {
            self.cur_dirty = false;
            if let Err(e) = self.write_current() {
                logln!("clock: current {}: {}", self.cur, e);
            }
        }
        match self.mode {
            Mode::Faces => {
                if self.face_next.is_some_and(|t| now < t) {
                    return; // лицо ещё играет
                }
                if self.face_idx >= self.faces.len() {
                    // анимация кончилась — вернуть часы немедленно
                    self.to_time();
                    return;
                }
                let f = self.faces[self.face_idx].clone();
                self.face_idx += 1;
                self.face_next = Some(now + FACE_MS);
                if let Err(e) = self.face(&f) {
                    logln!("clock: лицо {}: {}", f, e);
                    self.to_time(); // ошибка отыгрыша — сразу часы, анимацию бросаем
                }
            }
            Mode::Volume => {
                if now < self.vol_until.unwrap_or(now) {
                    if !self.vol_drawn {
                        let b = self.vol_pct.to_string();
                        if let Err(e) = self.write_text(b.as_bytes(), false) {
                            logln!("clock: громкость {}: {}", self.vol_pct, e);
                            // не смогли — вернуть часы, а не долбить ошибку каждый тик
                            self.to_time();
                            return;
                        }
                        self.vol_drawn = true;
                    }
                    return;
                }
                self.to_time();
            }
            Mode::Time => {
                // перерисовка при смене минуты (дедуп по номеру минуты)
                let bucket = chrono::Local::now().timestamp() / 60;
                if bucket != self.last_min_bucket {
                    self.last_min_bucket = bucket;
                    self.show_time();
                }
            }
        }
    }

    /// PWM на панель; горящее двоеточие синхронизируется (оно — отдельный
    /// LED, PWM родителя его не накрывает: ловилось «цифры яркие — двоеточие
    /// тусклое»). Только из render.
    fn write_brightness(&self) -> io::Result<()> {
        let Some(p) = &self.bright_file else {
            return Err(io::Error::new(io::ErrorKind::NotFound, "нет файла яркости"));
        };
        write_bytes(p, self.bright.to_string().as_bytes())?;
        self.resync_colon()
    }

    /// Аналоговый ток 1..4. Только из render.
    fn write_current(&self) -> io::Result<()> {
        let Some(p) = &self.cur_div else {
            return Err(io::Error::new(io::ErrorKind::NotFound, "нет current_divider"));
        };
        write_bytes(p, self.cur.to_string().as_bytes())
    }

    /// Вернуться в режим часов (запомнить текущую минуту, чтобы не
    /// перерисовывать её повторно).
    fn to_time(&mut self) {
        self.mode = Mode::Time;
        self.last_min_bucket = chrono::Local::now().timestamp() / 60;
        self.show_time();
    }

    /// Часы ЧЧ:ММ.
    fn show_time(&self) {
        let t = chrono::Local::now();
        let f = [
            b'0' + t.hour() as u8 / 10,
            b'0' + t.hour() as u8 % 10,
            b'0' + t.minute() as u8 / 10,
            b'0' + t.minute() as u8 % 10,
        ];
        if let Err(e) = self.write_text(&f, true) {
            logln!("clock: {}", e);
        }
    }

    /// Погасить всё (финал при остановке актора).
    fn blank(&self) {
        if let Err(e) = write_str(&self.message, "    ") {
            logln!("clock: гашение: {}", e);
        }
        let _ = self.colon(false);
    }

    /// Показать поле на message, выравнивание вправо на 4 позиции (как %4.4s
    /// в clockdisp: длиннее 4 — лишнее слева отрезается, короче — слева
    /// добивается пробелами); colon — зажечь двоеточие.
    fn write_text(&self, b: &[u8], colon: bool) -> io::Result<()> {
        let b = &b[..b.len().min(4)];
        let mut f = [b' '; 4];
        f[4 - b.len()..].copy_from_slice(b);
        write_bytes(&self.message, &f)?;
        self.colon(colon)
    }

    /// Двоеточие on/off.
    fn colon(&self, on: bool) -> io::Result<()> {
        let v = if on {
            self.bright.to_string()
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

    /// Поднять двоеточие до текущей яркости, если оно горит. Редкий путь
    /// (только при смене яркости) — чтение допустимо.
    fn resync_colon(&self) -> io::Result<()> {
        let Some(p) = &self.colon_top else { return Ok(()) };
        let b = std::fs::read_to_string(p)?;
        if b.trim() == "0" {
            return Ok(()); // выключено — не включаем
        }
        self.colon(true)
    }

    /// ОДИН раз вписать маски глаз в map_seg7 под их буквы (ключ GLYPH_MASKS =
    /// ASCII-код ячейки таблицы). Таблица читается в кэш seg_map, патчится
    /// только в этих 8 слотах и пишется обратно целиком; повторные вызовы —
    /// no-op. Только из serve (единственный писатель).
    fn register_glyphs(&mut self) -> io::Result<()> {
        if self.seg_have {
            return Ok(());
        }
        let raw = std::fs::read(&self.map_seg).map_err(|e| {
            io::Error::new(e.kind(), format!("map_seg7: нужно ровно 128 байт, {}", e))
        })?;
        if raw.len() != 128 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("map_seg7: нужно ровно 128 байт, получено {}", raw.len()),
            ));
        }
        self.seg_map.copy_from_slice(&raw);
        for &(code, mask) in &GLYPH_MASKS {
            self.seg_map[code as usize] = mask;
        }
        write_bytes(&self.map_seg, &self.seg_map)?;
        self.seg_have = true;
        Ok(())
    }

    /// Лицо из clockdisp: два глаза на средних цифрах, нос — нижняя точка.
    /// Кадр — одна запись 4 байт в message, таблица не трогается.
    fn face(&mut self, name: &str) -> io::Result<()> {
        self.register_glyphs()?;
        let Some((l, r)) = face_eyes(name) else {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("нет лица {:?}", name),
            ));
        };
        let f = [b' ', l, r, b' '];
        write_bytes(&self.message, &f)?;
        if let (Some(top), Some(bot)) = (&self.colon_top, &self.colon_bot) {
            write_str(top, "0")?;
            // нос
            return write_str(bot, &self.bright.to_string());
        }
        Ok(())
    }
}

/// Буквы-глифы для лица: два глаза из GLYPH_MASKS.
fn face_eyes(name: &str) -> Option<(u8, u8)> {
    match name {
        "idle" => Some((b'a', b'a')),
        "neutral" => Some((b'b', b'b')),
        "surprised" => Some((b'e', b'e')),
        "curious" => Some((b'f', b'f')),
        "sleepy" => Some((b'c', b'c')),
        "shock" => Some((b'd', b'd')),
        "wink" => Some((b'a', b'b')),
        "angry" => Some((b'g', b'h')),
        _ => None,
    }
}

fn write_str(path: &std::path::Path, s: &str) -> io::Result<()> {
    std::fs::write(path, s)
}

fn write_bytes(path: &std::path::Path, b: &[u8]) -> io::Result<()> {
    std::fs::write(path, b)
}

// Тесты — отдельным файлом (аналог _test.go): отсек cfg(test) в release
// не попадает, но видит приватные элементы этого модуля.
#[cfg(test)]
#[path = "clock_test.rs"]
mod tests;
