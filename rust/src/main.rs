//! marusia-d — демон периферии колонки VK Mini (Маруся), порт с Go на Rust.
//! Световое кольцо (SPI), сенсорная панель (evdev), панель-часы (seg-led),
//! датчик света (IIO), состояние музыки — от MPD, диалог ассистента — от LVA.
//!
//! Источники событий → state machine → рендер:
//!
//!   mpd watcher (player)      → play/pause кнопкой, лица на панели; кольцо от mpd не светится
//!   LVA peripheral API (ws)   → диалог: wake/listening/thinking/speaking/muted/error/noconn
//!   touch /dev/input/event0   → громкость ±10, play/pause, start_listening, mute
//!   light bh1730 (2с)         → глобальная яркость кольца
//!   clock                     → время ЧЧ:ММ, лица на события
//!
//! Приоритет сцен кольца: solid (HA) > error > noconn > muted > диалог LVA > сцена MPD.
//! Ни один из источников не фатален: mpd и LVA переподключаются сами.
//!
//! Сборка: cargo build --release --target aarch64-unknown-linux-musl (чистый Rust —
//! без cgo, статический musl-бинарь собирается без внешнего линкера).
mod clock;
mod light;
mod lva;
mod mpd;
mod ring;
mod touch;
mod util;

use std::io;
use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{mpsc, watch};

use crate::clock::Clock;
use crate::lva::Event;
use crate::ring::{Color, Ring};

// keycode'ы из DT keys@15: <0x72 0xF8 0x73 0x94 0x246>
// Назначения сверены на устройстве (evtest), кроме 582.
const KEY_VOLDOWN: u16 = 114; // 0x72  KEY_VOLUMEDOWN
const KEY_MUTE: u16 = 248; // 0xF8  mute микрофона (сверено)
const KEY_VOLUP: u16 = 115; // 0x73  KEY_VOLUMEUP
const KEY_CENTER: u16 = 148; // 0x94  центр сенсорного круга → play/pause (сверено)
const KEY_582: u16 = 582; // 0x246 — предположительно кнопка ассистента — TBD

// звук отклика на кнопки громкости (кладка в rootfs: /etc/sounds)
const SOUND_VOLUME: &str = "/etc/sounds/13_volume_voice.flac";
const SOUND_MIC_OFF: &str = "/etc/sounds/05_mic_on_to_mic_off.flac";
const SOUND_MIC_ON: &str = "/etc/sounds/06_mic_off_to_mic_on.flac";
/// вспышка громкости: дыхание 0.8 с
const FLASH: Duration = Duration::from_millis(800);
/// физическая громкость sink, больше нельзя
const VOLUME_MAX_PCT: i32 = 150;
/// визуальная шкала (кольцо, панель): 0..100
const VOLUME_VISUAL_MAX: i32 = 100;

fn main() {
    let (mpd_addr, lva_addr, lva_light, volapi_addr) = parse_flags();
    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .unwrap()
        .block_on(run(mpd_addr, lva_addr, lva_light, volapi_addr));
}

/// Разбор флагов в стиле Go flag: -mpd АДРЕС, -lva АДРЕС, -volapi АДРЕС,
/// -lva-light[=bool].
fn parse_flags() -> (String, String, bool, String) {
    let mut mpd_addr = "127.0.0.1:6600".to_string();
    let mut lva_addr = ":6055".to_string();
    let mut volapi_addr = "127.0.0.1:6056".to_string();
    let mut lva_light = true;
    let args: Vec<String> = std::env::args().skip(1).collect();
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "-mpd" if i + 1 < args.len() => {
                mpd_addr = args[i + 1].clone();
                i += 1;
            }
            "-lva" if i + 1 < args.len() => {
                lva_addr = args[i + 1].clone();
                i += 1;
            }
            "-volapi" if i + 1 < args.len() => {
                volapi_addr = args[i + 1].clone();
                i += 1;
            }
            f if f.starts_with("-lva-light") => {
                lva_light = !f.ends_with("=false");
            }
            other => {
                logln!("флаг {} не распознан (игнорирую)", other);
            }
        }
        i += 1;
    }
    (mpd_addr, lva_addr, lva_light, volapi_addr)
}

async fn run(mpd_addr: String, lva_addr: String, lva_light: bool, volapi_addr: String) {
    logln!(
        "marusia-d старт: mpd={} lva={:?} volapi={:?}",
        mpd_addr,
        lva_addr,
        volapi_addr
    );

    // ---------- отмена: SIGINT/SIGTERM ----------
    let (ctx_tx, mut ctx_rx) = watch::channel(false);
    tokio::spawn(async move {
        let mut term =
            match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
                Ok(s) => s,
                Err(_) => return,
            };
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = term.recv() => {}
        }
        let _ = ctx_tx.send(true);
    });

    // ---------- кольцо ----------
    let rg = Ring::new(ctx_rx.clone());

    // ---------- панель-часы ----------
    let clk: Option<Clock> = match Clock::open(ctx_rx.clone()) {
        Ok(c) => Some(c),
        Err(e) => {
            logln!("clock: {} — панель-часы выключена", e);
            None
        }
    };

    // ---------- датчик освещенности ----------
    {
        let rg = rg.clone();
        let clk = clk.clone();
        let crx = ctx_rx.clone();
        tokio::spawn(async move {
            let mut ema: f64 = -1.0;
            let (mut bright, mut cur) = (-1i32, -1i32);
            let mut lux_rx = light::start(crx, Duration::from_secs(2));
            while let Some(lux) = lux_rx.recv().await {
                if ema < 0.0 {
                    ema = lux;
                }
                ema = 0.7 * ema + 0.3 * lux;
                rg.set_scale(light::scale(ema));
                if let Some(clk) = &clk {
                    // яркость панели — от того же датчика; пишем только при изменении
                    let b = light::clock_pwm(ema);
                    if b != bright {
                        bright = b;
                        clk.set_brightness(b.clamp(0, 255) as u8);
                    }
                    let c = light::clock_current(ema);
                    if c != cur {
                        cur = c;
                        clk.set_current(c);
                    }
                }
            }
        });
    }

    // ---------- mpd ----------
    let (mpd_link, mut mpd_states_rx) = mpd::MpdLink::new(mpd_addr.clone());
    let mpd_link = Arc::new(mpd_link);
    tokio::spawn(Arc::clone(&mpd_link).serve(ctx_rx.clone()));

    // ---------- LVA (периферийный API) ----------
    let mut lva_events_rx: Option<mpsc::UnboundedReceiver<Event>> = None;
    let lva_client: Option<lva::Client>;
    if !lva_addr.is_empty() {
        let on_connect: lva::OnConnect = Arc::new(move |c: &lva::Client| {
            if lva_light {
                c.register_light("Кольцо", "ring", &["Voice Assistant"]);
            }
        });
        let (srv, client, rx) = lva::Server::new(lva_addr.clone(), Some(on_connect));
        lva_events_rx = Some(rx);
        lva_client = Some(client);
        tokio::spawn(srv.serve(ctx_rx.clone()));
    } else {
        lva_client = None;
    }

    // ---------- сенсорная панель ----------
    // громкость — СИСТЕМНАЯ (sink PulseAudio), а не mpd: через sink проходит
    // всё — музыка mpd, служебные звуки, голос ассистента.
    let key_ctx = ctx_rx.clone();
    let (key_tx, mut key_rx) = mpsc::channel::<u16>(1);

    // потребитель: единственная точка исполнения действий с клавиатуры
    {
        let rg = rg.clone();
        let clk = clk.clone();
        let lva_client = lva_client.clone();
        let mpd_link = Arc::clone(&mpd_link);
        let mut ctx = ctx_rx.clone();
        tokio::spawn(async move {
            loop {
                let code = tokio::select! {
                    c = key_rx.recv() => c,
                    _ = ctx.changed() => {
                        if crate::util::cancelled(&ctx) { return; }
                        None
                    }
                };
                let Some(code) = code else {
                    return; // producer ушёл
                };
                match code {
                    KEY_VOLUP => {
                        logln!("touch: KEY_VOLUP");
                        vol_step(&lva_client, &rg, &clk, true).await;
                    }
                    KEY_VOLDOWN => {
                        logln!("touch: KEY_VOLDOWN");
                        vol_step(&lva_client, &rg, &clk, false).await;
                    }
                    KEY_CENTER => {
                        // центр сенсорного круга — play/pause
                        mpd_link.toggle_pause().await;
                    }
                    KEY_582 => {
                        // кнопка ассистента — старт диалога в LVA
                        if let Some(c) = &lva_client {
                            if c.connected() {
                                c.send("start_listening", None);
                            }
                        }
                    }
                    KEY_MUTE => {
                        // mute микрофона — команда в LVA
                        if let Some(c) = &lva_client {
                            if c.connected() {
                                if rg.muted().await {
                                    c.send("unmute_mic", None);
                                } else {
                                    c.send("mute_mic", None);
                                }
                            }
                        }
                    }
                    other => {
                        logln!("touch: keycode {} (без действия)", other);
                    }
                }
            }
        });
    }

    // производитель: тач-события → в канал, лишние — дроп (потребитель занят)
    {
        let ctx = key_ctx;
        tokio::spawn(async move {
            let mut touch_rx = touch::start(ctx.clone(), "/dev/input/event0");
            while let Some(code) = touch_rx.recv().await {
                // небуферизированная семантика: потребитель занят — дропаем
                if key_tx.try_send(code).is_err() {
                    logln!("touch: keycode {} дропнут — потребитель занят", code);
                }
            }
        });
    }

    // ---------- volume API для upmpdcli (externalvolumecontrol) ----------
    // mpd при mixer_type none громкость не регулирует: DLNA-слайдер доезжает
    // через хуки upmpdcli (getexternalvolume/onvolumechange — dumb-пайпы
    // printf | nc на этот listener). Протокол построчный:
    //   GET          → визуальные 0..100 (текущий sink);
    //   SET <0..100> → sink = round(v*1.5), отклик кольцо/панель/звук.
    // Шкала: визуальная 0..VOLUME_VISUAL_MAX ↔ физическая 0..VOLUME_MAX_PCT.
    {
        let rg = rg.clone();
        let clk = clk.clone();
        let mut ctx = ctx_rx.clone();
        let addr = volapi_addr.clone();
        tokio::spawn(async move {
            if addr.is_empty() {
                return;
            }
            let ln = match TcpListener::bind(&addr).await {
                Ok(l) => l,
                Err(e) => {
                    logln!("volapi: {} — DLNA-громкость через upmpdcli не работает", e);
                    return;
                }
            };
            logln!("volapi: слушаю {}", addr);
            loop {
                let conn = tokio::select! {
                    _ = ctx.changed() => {
                        if crate::util::cancelled(&ctx) { return; }
                        continue;
                    }
                    c = ln.accept() => match c {
                        Ok((c, _)) => c,
                        Err(e) => {
                            logln!("volapi: accept: {}", e);
                            continue;
                        }
                    }
                };
                let rg = rg.clone();
                let clk = clk.clone();
                tokio::spawn(volapi_conn(conn, rg, clk));
            }
        });
    }

    // ---------- главный цикл ----------
    loop {
        tokio::select! {
            _ = ctx_rx.changed() => {
                if crate::util::cancelled(&ctx_rx) {
                    logln!("marusia-d стоп"); // панель и кольцо гасят свои акторы
                    return;
                }
            }
            state = mpd_states_rx.recv() => {
                let Some(state) = state else { continue };
                // Кольцо от mpd не светится (по просьбе): радуга на play мешает.
                // Состояние mpd нужно только для play/pause кнопкой и лиц на панели.
                rg.set_mpd();
                if let Some(clk) = &clk {
                    match state.as_str() {
                        "play" => clk.play_faces(&["neutral", "curious", "surprised"]),
                        "pause" => clk.play_faces(&["sleepy"]),
                        _ => {}
                    }
                }
                logln!("mpd: {}", state);
            }
            e = async {
                lva_events_rx
                    .as_mut()
                    .expect("lva_events_rx is Some")
                    .recv()
                    .await
            }, if lva_events_rx.is_some() => {
                let Some(e) = e else {
                    // поток закрыт
                    lva_events_rx = None;
                    continue;
                };
                handle_lva_event(&e, &rg, &clk).await;
            }
        }
    }
}

/// Одно соединение volapi: строка команды с таймаутом 2 с (dumb-пайп
/// printf | nc: открыл, послал, прочитал, закрыл).
async fn volapi_conn(conn: TcpStream, rg: Ring, clk: Option<Clock>) {
    let work = async {
        let (r, mut w) = conn.into_split();
        let mut r = BufReader::new(r);
        let mut line = String::new();
        if r.read_line(&mut line).await? == 0 {
            return Ok(()); // пустое соединение
        }
        let fields: Vec<&str> = line.trim().split_whitespace().collect();
        if fields.is_empty() {
            return Ok(());
        }
        match fields[0] {
            "GET" => {
                let Some(p) = get_sink_pct().await else { return Ok(()) };
                let p = p.min(VOLUME_MAX_PCT);
                // физическая → визуальная с округлением до ближайшего
                let vis = (p * 100 + 75) / VOLUME_MAX_PCT;
                w.write_all(format!("{}\n", vis).as_bytes()).await?;
            }
            "SET" => {
                let Some(q) = fields.get(1) else { return Ok(()) };
                let v = match q.parse::<i32>() {
                    Ok(v) if v >= 0 => v,
                    _ => {
                        logln!("volapi: SET {:?}: не число", q);
                        return Ok(());
                    }
                };
                let v = v.min(VOLUME_VISUAL_MAX);
                set_sink_pct((v * VOLUME_MAX_PCT + 50) / 100).await;
                vol_feedback(&rg, &clk, v).await;
            }
            other => {
                logln!("volapi: неизвестная команда {:?}", other);
            }
        }
        Ok::<(), io::Error>(())
    };
    if tokio::time::timeout(Duration::from_secs(2), work)
        .await
        .is_err()
    {
        logln!("volapi: таймаут 2с");
    }
}

async fn handle_lva_event(e: &Event, rg: &Ring, clk: &Option<Clock>) {
    match e.event.as_str() {
        lva::EV_DOWN => rg.set_live(false),
        "snapshot" => {
            rg.set_snapshot(e.bool("muted"));
            if e.has("volume") {
                // upstream: volume — доля 0.0..1.0 от максимума колонки
                // (150%); sink — источник истины по громкости колонки,
                // синхронизируем при коннекте в физической шкале
                set_sink_pct((e.float("volume") * VOLUME_MAX_PCT as f64).round() as i32).await;
            }
        }
        "zeroconf" => {
            if e.str("status") == "connected" {
                rg.set_live(true);
            }
        }
        "muted" => {
            // звук — на подтверждённую смену состояния (кнопка, голос, HA)
            let new_muted = e.bool("muted");
            if new_muted != rg.muted().await {
                if new_muted {
                    play_sound(SOUND_MIC_OFF);
                } else {
                    play_sound(SOUND_MIC_ON);
                }
            }
            rg.set_muted(new_muted);
        }
        "wake_word_detected" => rg.set_dialog("wake"),
        "listening" => {
            rg.set_dialog("listening");
            if let Some(clk) = clk {
                clk.play_faces(&["curious"]);
            }
        }
        "thinking" => {
            rg.set_dialog("thinking");
            if let Some(clk) = clk {
                clk.play_faces(&["wink"]);
            }
        }
        "tts_speaking" => {
            rg.set_dialog("speaking");
            if let Some(clk) = clk {
                clk.play_faces(&["neutral"]);
            }
        }
        "tts_finished" | "idle" => rg.set_dialog(""),
        "pipeline_error" => {
            rg.set_error(true);
            let rg2 = rg.clone();
            tokio::spawn(async move {
                tokio::time::sleep(Duration::from_millis(1600)).await;
                rg2.set_error(false);
            });
        }
        "media_player_playing" => {
            // музыка через LVA (HA media player) — но кольцо от музыки не светится
            rg.set_mpd();
        }
        "volume_changed" => {
            // upstream: {"volume": float} 0.0..1.0 — доля от максимума
            // колонки (150%); sink — в физической шкале, в отклик —
            // визуальные 0..100 (to_visual)
            let pct = (e.float("volume") * VOLUME_MAX_PCT as f64).round() as i32;
            set_sink_pct(pct).await;
            vol_feedback(rg, clk, to_visual(pct)).await;
        }
        "light_command" => {
            let lc = e.light_command();
            if lc.object_id != "ring" {
                return;
            }
            if lc.effect == "Voice Assistant" {
                // HA вернул управление демону
                rg.set_ha(false);
            } else {
                let k = lc.brightness;
                rg.set_solid(Color {
                    r: (lc.red * 255.0 * k) as u8,
                    g: (lc.green * 255.0 * k) as u8,
                    b: (lc.blue * 255.0 * k) as u8,
                });
                if !lc.state {
                    rg.set_solid(Color::default()); // ring.Off
                }
                rg.set_ha(true);
            }
        }
        other => logln!("lva: событие {} (без обработки)", other),
    }
}

/// Текущая физическая громкость default sink в процентах
/// 0..VOLUME_MAX_PCT, None = не узнали.
async fn get_sink_pct() -> Option<i32> {
    let out = pactl_output(&["get-sink-volume", "@DEFAULT_SINK@"]).await?;
    for f in out.split_whitespace() {
        if let Some(num) = f.strip_suffix('%') {
            if let Ok(v) = num.parse::<i32>() {
                return Some(v);
            }
        }
    }
    None
}

/// Отклик на смену громкости: звук, вспышка всего кольца одним цветом уровня,
/// проценты на панели-часах. Звук ПЕРВЫМ: он запускается асинхронно и должен
/// уйти раньше дальнейшей работы (в Go Flash был синхронным до первого кадра
/// (rebind), спад дорисовывается в рендере — paplay после Flash отставал бы
/// от старта вспышки). pct — ВИЗУАЛЬНАЯ громкость 0..100;
/// pct=-1 — громкость не узнали: вспышка белая, панель не трогаем.
async fn vol_feedback(rg: &Ring, clk: &Option<Clock>, pct: i32) {
    play_sound(SOUND_VOLUME);
    rg.flash(volume_color(pct), FLASH);
    if let (Some(clk), true) = (clk, pct >= 0) {
        clk.show_volume(pct);
    }
}

/// Физическая громкость sink 0..VOLUME_MAX_PCT (не доля 0..1!).
async fn set_sink_pct(p: i32) {
    let p = p.clamp(0, VOLUME_MAX_PCT);
    pactl(&["set-sink-volume", "@DEFAULT_SINK@", &format!("{}%", p)]).await;
}

/// up=true → KEY_VOLUP, up=false → KEY_VOLDOWN.
/// LVA подключен: команды volume_up/volume_down — шаг знает только LVA
/// (5% по умолчанию, --peripheral-volume-step), результат прилетит
/// volume_changed'ом. Автономно: локальный pactl, шаг наш собственный.
async fn vol_step(
    lva_client: &Option<lva::Client>,
    rg: &Ring,
    clk: &Option<Clock>,
    up: bool,
) {
    if let Some(c) = lva_client {
        if c.connected() {
            if up {
                c.send("volume_up", None);
            } else {
                c.send("volume_down", None);
            }
            return;
        }
    }
    // локальная ветка: шаг ±10% с потолком VOLUME_MAX_PCT.
    let mut pct = get_sink_pct().await.unwrap_or(-1);
    if pct >= 0 {
        if (up && pct >= VOLUME_MAX_PCT) || (!up && pct <= 0) {
            vol_feedback(rg, clk, to_visual(pct)).await; // на пределе — оранжевый отклик, не шагаем
            return;
        }
    }
    if up {
        pactl(&["set-sink-volume", "@DEFAULT_SINK@", "+10%"]).await;
    } else {
        pactl(&["set-sink-volume", "@DEFAULT_SINK@", "-10%"]).await;
    }
    pct = get_sink_pct().await.unwrap_or(-1);
    if pct > VOLUME_MAX_PCT {
        set_sink_pct(VOLUME_MAX_PCT).await; // PA умеет >150% — режем потолком
        pct = VOLUME_MAX_PCT;
    }
    vol_feedback(rg, clk, to_visual(pct)).await;
}

/// Физическая громкость sink (0..VOLUME_MAX_PCT) → визуальная шкала
/// 0..VOLUME_VISUAL_MAX: на кольце и панели громкость всегда 0..100,
/// физические 150% показываются как 100. Отрицательное (-1 = «не узнали»)
/// проходит насквозь без перевода.
fn to_visual(pct: i32) -> i32 {
    if pct < 0 {
        return -1;
    }
    pct * VOLUME_VISUAL_MAX / VOLUME_MAX_PCT
}

/// Цвет вспышки громкости, двухсегментный градиент по ВИЗУАЛЬНОЙ шкале
/// 0..VOLUME_VISUAL_MAX:
///   0..половины — синий → зелёный (B убывает, G растёт);
///   половина..максимума — зелёный → оранжевый (R растёт, G спадает до 140).
/// pct=-1 — уровень не узнали: белая.
fn volume_color(pct: i32) -> Color {
    if pct < 0 {
        return ring::WHITE;
    }
    let t = (pct as f64 / VOLUME_VISUAL_MAX as f64).min(1.0);
    let (r, g, b) = if t <= 0.5 {
        let s = t / 0.5;
        (0.0, 255.0 * s, 255.0 * (1.0 - s))
    } else {
        let s = (t - 0.5) / 0.5;
        // на максимуме оранжевый (255,140,0)
        (255.0 * s, 255.0 - (255.0 - 140.0) * s, 0.0)
    };
    Color {
        r: r as u8,
        g: g as u8,
        b: b as u8,
    }
}

/// pactl — управление системной громкостью PulseAudio. Сокет задаём явно,
/// как и в play_sound (в template нет client.conf с default-server).
async fn pactl(args: &[&str]) {
    if let Err(e) = pactl_cmd(args).output().await {
        logln!("pactl {:?}: {}", args, e);
    }
}

/// pactl_output — как pactl, но возвращает stdout (для чтения громкости).
async fn pactl_output(args: &[&str]) -> Option<String> {
    let out = pactl_cmd(args).output().await.ok()?;
    if !out.status.success() {
        logln!("pactl {:?}: статус {:?}", args, out.status.code());
        return None;
    }
    Some(String::from_utf8_lossy(&out.stdout).into_owned())
}

fn pactl_cmd(args: &[&str]) -> tokio::process::Command {
    let mut cmd = tokio::process::Command::new("pactl");
    cmd.args(args);
    cmd.env("PULSE_SERVER", "unix:/var/run/pulse/native");
    cmd
}

/// play_sound — короткий сервисный звук через paplay, не ждём окончания.
/// PA на колонке в system mode; unix-сокет задаём явно, чтобы не зависеть
/// от /etc/pulse/client.conf и рантайм-симлинка в $HOME root.
fn play_sound(path: &'static str) {
    tokio::spawn(async move {
        let mut cmd = tokio::process::Command::new("paplay");
        cmd.arg(path);
        cmd.env("PULSE_SERVER", "unix:/var/run/pulse/native");
        if let Err(e) = cmd.output().await {
            logln!("sound: {}: {}", path, e);
        }
    });
}

// Тесты — отдельным файлом (аналог _test.go): отсек cfg(test) в release
// не попадает, но видит приватные элементы этого модуля.
#[cfg(test)]
#[path = "main_test.rs"]
mod tests;
