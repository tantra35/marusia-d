//! Порт internal/ring (Go): световое кольцо VK Mini — 12× WS2812-подобных
//! диодов, прямой вывод через spidev (транспорт и ограничения — в spi.rs).
//!
//! Архитектура — АКТОР: единственная задача (serve) владеет всем состоянием
//! RingState и рисует кадры. Сеттеры — fire-and-forget команды через
//! unbounded-канал (FIFO актора гарантирует порядок); sync-подтверждение не
//! нужно — оно не несло информации сверх «команда дошла», а единственный
//! запрос значения (muted) несёт свой oneshot-канал.
//!
//! Отрисовка (как у часов): тик ~40 мс; на каждом тике render() вычисляет кадр
//! из состояния + фазы времени и шлёт его через SPI (rebind + prime + кадр).
//! Статичные сцены (off/muted/solid) рисуются один раз — диоды защёлкивают
//! последнее значение. Анимации (дыхание, мигание, затухание вспышки) — новый
//! кадр на каждый тик, но не чаще animFrameInterval (≤8 rebind/с: частый
//! rebind портит память ядра — UAF в bind/unbind spidev, Oops 2026-11-06).
pub mod spi;

use std::fmt;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use tokio::sync::{mpsc, oneshot, watch};

use crate::logln;
use spi::{FRAME_LEN, PRIME_LEN};

/// для паритета с Go-API (New(dev, n)); транспорт берёт SPI_DEV из spi.rs
#[allow(dead_code)]
pub const DEFAULT_DEVICE: &str = "/dev/spidev0.0";
pub const LEDS: usize = 12;
pub const BYTES_PER_LED: usize = 12; // 24 бита × 4 SPI-бита / 8

/// тик отрисовки (~25 fps сверху)
const RENDER_INTERVAL: Duration = Duration::from_millis(40);
const BREATHE_PERIOD_MS: u64 = 1500;
const BLINK_PERIOD_MS: u64 = 500;
/// прореживание анимаций: ≤8 rebind/с
const ANIM_FRAME_INTERVAL: Duration = Duration::from_millis(125);

#[derive(Clone, Copy, PartialEq, Debug, Default)]
pub struct Color {
    pub r: u8,
    pub g: u8,
    pub b: u8,
}

pub const WHITE: Color = Color { r: 255, g: 255, b: 255 };

// Палитра сцен (RGB).
const MUTED_RGB: [u8; 3] = [0x8C, 0x00, 0x00]; // приглушённый красный
const LISTEN_RGB: [u8; 3] = [0x00, 0x3C, 0xFF]; // синий
const THINK_RGB: [u8; 3] = [0x96, 0x28, 0xFF]; // фиолетовый
const SPEAK_RGB: [u8; 3] = [0x00, 0xAA, 0xE6]; // голубой
const ERROR_RGB: [u8; 3] = [0xFF, 0x00, 0x00]; // ошибка
const NOCONN_RGB: [u8; 3] = [0x64, 0x00, 0x00]; // нет связи с LVA — тусклый красный

/// Состояние сцен + производный кадр. Поля доступны ТОЛЬКО из актора
/// (serve); снаружи — через команды Ring. Отдельно живёт для тестов:
/// чистые методы (resolve/frame) — без актора и SPI.
pub struct RingState {
    /// сцена источника «музыка»: всегда off (радугу убрали — мешала)
    pub mpd: String,
    /// "" | wake | listening | thinking | speaking
    pub dialog: String,
    pub error_t: bool,
    pub muted: bool,
    /// было хоть одно живое соединение с LVA
    pub lva_seen: bool,
    /// соединение открыто сейчас
    pub lva_live: bool,
    /// кольцом управляет HA (light_command)
    pub ha_active: bool,
    /// глобальная яркость от датчика света (0..1)
    pub scale: f64,
    /// цвет для сцены solid (light_command из HA)
    pub solid: Color,
    /// цвет вспышки громкости
    pub overlay_color: Color,
    /// её полная длительность (спад)
    pub overlay_dur: Duration,
    /// когда вспышка истекает; None = нет
    pub overlay_tt: Option<SystemTime>,
    /// ключ последнего нарисованного статичного кадра
    pub last_drawn: String,
    /// время последнего анимационного кадра (прореживание)
    pub last_anim_draw: Option<SystemTime>,
}

impl Default for RingState {
    fn default() -> Self {
        RingState {
            mpd: "off".to_string(),
            dialog: String::new(),
            error_t: false,
            muted: false,
            lva_seen: false,
            lva_live: false,
            ha_active: false,
            scale: 1.0,
            solid: Color::default(),
            overlay_color: Color::default(),
            overlay_dur: Duration::ZERO,
            overlay_tt: None,
            last_drawn: String::new(),
            last_anim_draw: None,
        }
    }
}

impl RingState {
    /// Вычислить сцену из состояния: solid (HA) > error > noconn > muted >
    /// диалог > mpd. Чистая функция — состояние не меняет.
    pub fn resolve(&self) -> String {
        if self.ha_active {
            "solid".to_string()
        } else if self.error_t {
            "error".to_string()
        } else if self.lva_seen && !self.lva_live {
            "noconn".to_string()
        } else if self.muted {
            "muted".to_string()
        } else if !self.dialog.is_empty() {
            self.dialog.clone()
        } else {
            self.mpd.clone()
        }
    }

    /// SetScale — глобальная яркость от датчика света (0..1), кламп 0..1.
    pub fn set_scale(&mut self, s: f64) {
        self.scale = s.clamp(0.0, 1.0);
    }

    /// Flash — вспышка цветом c длительностью d: мгновенно полная яркость,
    /// затем плавный спад (профиль в frame()).
    pub fn flash(&mut self, c: Color, d: Duration) {
        self.overlay_color = c;
        self.overlay_dur = d;
        self.overlay_tt = Some(SystemTime::now() + d);
    }

    /// Применить команду-сеттер (только мутация поля; кадр рисует render).
    pub fn exec(&mut self, cmd: &Cmd) {
        match cmd {
            Cmd::SetMPD => self.mpd = "off".to_string(), // радугу убрали — мешала
            Cmd::SetDialog(name) => self.dialog = name.clone(),
            Cmd::SetMuted(m) => self.muted = *m,
            Cmd::SetError(on) => self.error_t = *on,
            Cmd::SetLive(live) => self.lva_live = *live,
            Cmd::SetSnapshot(muted) => {
                self.lva_seen = true;
                self.lva_live = true;
                self.muted = *muted;
            }
            Cmd::SetHA(on) => self.ha_active = *on,
            Cmd::SetScale(s) => self.set_scale(*s),
            Cmd::SetSolid(c) => self.solid = *c,
            Cmd::Flash(c, d) => self.flash(*c, *d),
            Cmd::GetMuted(_) => {} // запрос, не сеттер
        }
    }

    /// Кадр для текущего состояния и момента времени.
    /// Возвращает (ключ статичного кадра, "" у анимаций, пиксели, анимация?).
    /// Чистая: только читает состояние — годится для тестов.
    pub fn frame(&self, now: SystemTime) -> (String, [[u8; 3]; LEDS], bool) {
        let mut px = [[0u8; 3]; LEDS];
        // вспышка громкости: мгновенный кадр полной яркости → плавный спад
        if let Some(tt) = self.overlay_tt {
            if now < tt {
                let rest = tt.duration_since(now).unwrap_or_default();
                let mut k = rest.as_secs_f64() / self.overlay_dur.as_secs_f64();
                if k > 1.0 {
                    k = 1.0;
                }
                let b = k.powf(1.6); // резкий старт, плавный хвост
                let col = mul(&rgb(self.overlay_color), b);
                px.fill(col);
                return (String::new(), px, true);
            }
        }

        let dim = 0.25 + 0.75 * self.scale; // датчик света приглушает, но не гасит
        match self.resolve().as_str() {
            "solid" => {
                let c = rgb(self.solid);
                px.fill(mul(&c, dim));
                ("solid".to_string(), px, false)
            }
            "muted" => {
                px.fill(mul(&MUTED_RGB, dim));
                ("muted".to_string(), px, false)
            }
            "error" => {
                let on = unix_millis(now) % BLINK_PERIOD_MS < BLINK_PERIOD_MS / 2;
                if on {
                    px.fill(mul(&ERROR_RGB, dim));
                }
                (String::new(), px, true)
            }
            "noconn" => {
                let b = breathe(now, BREATHE_PERIOD_MS) * 0.4; // тускло
                px.fill(mul(&NOCONN_RGB, b));
                (String::new(), px, true)
            }
            "listening" | "thinking" | "speaking" => {
                let b = breathe(now, BREATHE_PERIOD_MS) * self.scale;
                let c = match self.resolve().as_str() {
                    "listening" => LISTEN_RGB,
                    "thinking" => THINK_RGB,
                    _ => SPEAK_RGB,
                };
                px.fill(mul(&c, b));
                (String::new(), px, true)
            }
            // off — чёрный
            _ => ("off".to_string(), px, false),
        }
    }
}

fn rgb(c: Color) -> [u8; 3] {
    [c.r, c.g, c.b]
}

impl fmt::Display for Color {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "#{:02X}{:02X}{:02X}", self.r, self.g, self.b)
    }
}

/// Масштабировать цвет по коэффициенту 0..1.
fn mul(c: &[u8; 3], k: f64) -> [u8; 3] {
    [
        (c[0] as f64 * k) as u8,
        (c[1] as f64 * k) as u8,
        (c[2] as f64 * k) as u8,
    ]
}

/// Фаза дыхания 0.15..1 (плавная синусоида).
fn breathe(now: SystemTime, period_ms: u64) -> f64 {
    let ns = now
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    let period_ns = (period_ms as u128).checked_mul(1_000_000).unwrap_or(1);
    let s = (2.0 * std::f64::consts::PI * (ns % period_ns) as f64 / period_ns as f64).sin();
    0.15 + 0.85 * (s + 1.0) / 2.0
}

fn unix_millis(now: SystemTime) -> u64 {
    now.duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

/// Команды актору. Сеттеры — fire-and-forget; GetMuted несёт reply-канал
/// (единственный запрос значения). Порядок гарантирует FIFO актора.
pub enum Cmd {
    SetMPD,
    SetDialog(String),
    SetMuted(bool),
    GetMuted(oneshot::Sender<bool>),
    SetError(bool),
    SetLive(bool),
    SetSnapshot(bool),
    SetHA(bool),
    SetScale(f64),
    SetSolid(Color),
    Flash(Color, Duration),
}

/// Хэндл актора: команды через канал, Sync — можно клонировать во все задачи.
#[derive(Clone)]
pub struct Ring {
    tx: mpsc::UnboundedSender<Cmd>,
}

impl Ring {
    /// Создать актора и запустить его задачей. dev сейчас не используется
    /// (транспорт берёт SPI_DEV из spi.rs), как в Go.
    pub fn new(ctx: watch::Receiver<bool>) -> Ring {
        let (tx, rx) = mpsc::unbounded_channel();
        let actor = Actor {
            st: RingState::default(),
            frame_buf: Box::new([0u8; FRAME_LEN]),
            prime_buf: Box::new([0u8; PRIME_LEN]),
        };
        tokio::spawn(actor.serve(rx, ctx));
        Ring { tx }
    }

    /// Сцена источника «музыка»: всегда off (радугу убрали — мешала).
    pub fn set_mpd(&self) {
        let _ = self.tx.send(Cmd::SetMPD);
    }

    /// Активная фаза диалога LVA ("" = диалога нет).
    pub fn set_dialog(&self, name: impl Into<String>) {
        let _ = self.tx.send(Cmd::SetDialog(name.into()));
    }

    /// Микрофон заглушен (источник истины — LVA).
    pub fn set_muted(&self, m: bool) {
        let _ = self.tx.send(Cmd::SetMuted(m));
    }

    /// Прочитать состояние мьюта (для кнопки микрофона).
    pub async fn muted(&self) -> bool {
        let (tx, rx) = oneshot::channel();
        if self.tx.send(Cmd::GetMuted(tx)).is_err() {
            return false;
        }
        rx.await.unwrap_or(false)
    }

    /// Мигание ошибки диалога (сбрасывается по таймеру).
    pub fn set_error(&self, on: bool) {
        let _ = self.tx.send(Cmd::SetError(on));
    }

    /// LVA-соединение открыто/закрыто.
    pub fn set_live(&self, live: bool) {
        let _ = self.tx.send(Cmd::SetLive(live));
    }

    /// Первый коннект LVA прислал полное состояние.
    pub fn set_snapshot(&self, muted: bool) {
        let _ = self.tx.send(Cmd::SetSnapshot(muted));
    }

    /// Кольцом управляет Home Assistant.
    pub fn set_ha(&self, on: bool) {
        let _ = self.tx.send(Cmd::SetHA(on));
    }

    /// Глобальная яркость от датчика света (0..1).
    pub fn set_scale(&self, s: f64) {
        let _ = self.tx.send(Cmd::SetScale(s));
    }

    /// Цвет для сцены solid (управление кольцом из HA).
    pub fn set_solid(&self, c: Color) {
        let _ = self.tx.send(Cmd::SetSolid(c));
    }

    /// Вспышка цветом c длительностью d: мгновенно полная яркость, затем
    /// плавный спад (дорисовывается в рендере актора).
    pub fn flash(&self, c: Color, d: Duration) {
        let _ = self.tx.send(Cmd::Flash(c, d));
    }
}

/// Актор: состояние + постоянные DMA-буферы. Всё поле st трогает только serve.
struct Actor {
    st: RingState,
    /// 400 байт: reset + payload + reset — выделяются один раз и живут вечно,
    /// физические страницы не возвращаются ядру (см. spi.rs).
    frame_buf: Box<[u8; FRAME_LEN]>,
    /// 16 байт primer @1 МГц
    prime_buf: Box<[u8; PRIME_LEN]>,
}

impl Actor {
    /// Нарисовать кадр, если он отличается от уже показанного. Анимации
    /// рисуются не чаще ANIM_FRAME_INTERVAL: каждый кадр = rebind контроллера,
    /// а частый rebind портит память ядра (UAF в bind/unbind spidev).
    /// Только в акторе.
    fn render(&mut self) {
        let now = SystemTime::now();
        let (key, px, animated) = self.st.frame(now);
        if !animated && key == self.st.last_drawn {
            return; // статичный кадр уже на диодах
        }
        if animated
            && self
                .st
                .last_anim_draw
                .map(|t| now.duration_since(t).unwrap_or_default() < ANIM_FRAME_INTERVAL)
                .unwrap_or(false)
        {
            return; // прореживание анимаций: ≤8 rebind/с вместо 25
        }
        spi::encode_frame_into(&mut self.frame_buf, &px); // заполнить DMA-буфер
        if let Err(e) = spi::draw(&self.prime_buf, &self.frame_buf) {
            logln!("ring: draw: {}", e);
            self.st.last_drawn.clear(); // повторить при следующем тике
            return;
        }
        if animated {
            self.st.last_anim_draw = Some(now);
            self.st.last_drawn.clear(); // после анимации статичную сцену перерисовать
        } else {
            self.st.last_drawn = key;
        }
    }

    /// Актор: единственная задача, владеющая состоянием и рисующая кадры.
    /// Команды приходят через rx, каждая исполняется → кадр → (reply).
    /// При отмене — финальное гашение (диоды защёлкивают последний кадр).
    async fn serve(mut self, mut rx: mpsc::UnboundedReceiver<Cmd>, mut ctx: watch::Receiver<bool>) {
        let mut tick = tokio::time::interval(RENDER_INTERVAL);
        loop {
            tokio::select! {
                _ = ctx.changed() => {
                    if crate::util::cancelled(&ctx) {
                        // кадр в диодах защёлкнут — погасить кольцо напоследок
                        let black = [[0u8; 3]; LEDS];
                        spi::encode_frame_into(&mut self.frame_buf, &black);
                        if let Err(e) = spi::draw(&self.prime_buf, &self.frame_buf) {
                            logln!("ring: финальное гашение: {}", e);
                        }
                        return;
                    }
                }
                Some(cmd) = rx.recv() => {
                    // 1. состояние, 2. кадр, 3. reply (у запросов значения)
                    if let Cmd::GetMuted(tx) = cmd {
                        self.render();
                        let _ = tx.send(self.st.muted);
                    } else {
                        self.st.exec(&cmd);
                        self.render();
                    }
                }
                _ = tick.tick() => {
                    self.render();
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn st() -> RingState {
        RingState {
            scale: 1.0,
            ..RingState::default()
        }
    }

    #[tokio::test]
    async fn frame_flash_overrides_scene() {
        let mut r = st();
        r.muted = true;
        let now = SystemTime::now();
        r.flash(Color { r: 255, g: 255, b: 255 }, Duration::from_secs(1));

        let (key, px, animated) = r.frame(now);
        assert!(key.is_empty() && animated, "want анимацию вспышки");
        assert_eq!(px[0], [255, 255, 255], "want полную яркость в начале спада");
        // к концу вспышки яркость упала
        let (_, px_end, _) = r.frame(now + Duration::from_millis(900));
        assert!(px_end[0][0] < px[0][0], "хвост вспышки ярче начала");
    }

    #[tokio::test]
    async fn flash_actor_smoke() {
        // Сеттеры fire-and-forget: убеждаемся, что команда доходит до актора
        // (muted-запрос выстраивается в очередь за Flash — FIFO актора).
        let (ctx_tx, ctx_rx) = watch::channel(false);
        let r = Ring::new(ctx_rx);
        r.flash(WHITE, Duration::from_millis(800));
        assert!(!r.muted().await);
        let _ = ctx_tx.send(true);
    }

    #[tokio::test]
    async fn frame_muted() {
        let mut r = st();
        r.muted = true;
        let (key, px, animated) = r.frame(SystemTime::now());
        assert_eq!(key, "muted");
        assert!(!animated, "want muted без анимации");
        assert_eq!(px[0], MUTED_RGB);
    }

    #[tokio::test]
    async fn frame_animations_breathing() {
        let mut r = st();
        r.dialog = "listening".to_string();
        let now = SystemTime::now();
        let (key1, px1, anim1) = r.frame(now);
        let (_, px2, anim2) = r.frame(now + Duration::from_millis(400));
        assert!(key1.is_empty() && anim1 && anim2, "want анимацию дыхания");
        assert_ne!(px2[0], px1[0], "яркость дыхания не меняется между фазами");
    }

    #[tokio::test]
    async fn frame_off() {
        let r = st();
        let (key, px, animated) = r.frame(SystemTime::now());
        assert_eq!(key, "off");
        assert!(!animated);
        assert_eq!(px[0], [0, 0, 0], "want чёрный");
    }

    #[tokio::test]
    async fn scale_clamp() {
        let mut r = st();
        r.set_scale(5.0); // клампит в [0..1]
        assert_eq!(r.scale, 1.0, "want 1 (кламп сверху)");
        r.set_scale(-1.0);
        assert_eq!(r.scale, 0.0, "want 0 (кламп снизу)");
    }
}
