//! Порт mpdLink из main.go (Go): переподключающийся клиент MPD и watcher.
//!
//! Протокол — текстовый MPD-протокол (command list, idle), используем ровно
//! те команды, что были в gompd: greeting «OK MPD …», status, pause N,
//! ping, idle player. gompd-пакет остаётся эталоном (внутри то же самое).
//!
//! Отличие от Go: tokio TcpStream не умеет try_clone, поэтому общих
//! коннекций нет — watcher/ping/toggle_pause открывают свои (mpd локальный,
//! dial дешёвый; повторные коннекции протоколом разрешены).
use std::collections::HashMap;
use std::io;
use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::TcpStream;
use tokio::sync::{mpsc, watch};

use crate::{logln, util};

/// Минимальный клиент MPD: одна коннекция, запрос-ответ.
pub struct Client {
    r: BufReader<TcpStream>,
}

impl Client {
    pub async fn dial(addr: &str) -> io::Result<Client> {
        let s = TcpStream::connect(addr).await?;
        let mut c = Client {
            r: BufReader::new(s),
        };
        let greeting = c.line().await?;
        if !greeting.starts_with("OK MPD") {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("не greeting: {}", greeting),
            ));
        }
        Ok(c)
    }

    async fn line(&mut self) -> io::Result<String> {
        let mut s = String::new();
        let n = self.r.read_line(&mut s).await?;
        if n == 0 {
            return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "соединение закрыто"));
        }
        Ok(s.trim_end().to_string())
    }

    /// Послать команду и прочитать ответ до OK/ACK.
    async fn cmd(&mut self, c: &str) -> io::Result<Vec<(String, String)>> {
        self.r.get_mut().write_all(format!("{}\n", c).as_bytes()).await?;
        self.r.flush().await?;
        let mut out = Vec::new();
        loop {
            let l = self.line().await?;
            if l == "OK" {
                return Ok(out);
            }
            if let Some(rest) = l.strip_prefix("ACK ") {
                return Err(io::Error::new(io::ErrorKind::Other, rest.to_string()));
            }
            if let Some((k, v)) = l.split_once(": ") {
                out.push((k.to_string(), v.to_string()));
            }
        }
    }

    pub async fn status(&mut self) -> io::Result<HashMap<String, String>> {
        Ok(self.cmd("status").await?.into_iter().collect())
    }

    pub async fn pause(&mut self, on: bool) -> io::Result<()> {
        self.cmd(if on { "pause 1" } else { "pause 0" }).await.map(|_| ())
    }

    pub async fn ping(&mut self) -> io::Result<()> {
        self.cmd("ping").await.map(|_| ())
    }

    /// idle — блокирует до события в перечисленных подсистемах.
    pub async fn idle(&mut self, systems: &str) -> io::Result<Vec<String>> {
        Ok(self
            .cmd(&format!("idle {}", systems))
            .await?
            .into_iter()
            .map(|(k, _)| k)
            .collect())
    }
}

/// Переподключающийся линк к MPD. watcher и ping живут в своих задачах;
/// наружу — serve, toggle_pause и канал состояний.
pub struct MpdLink {
    addr: String,
    states_tx: mpsc::UnboundedSender<String>,
}

impl MpdLink {
    pub fn new(addr: impl Into<String>) -> (Self, mpsc::UnboundedReceiver<String>) {
        let (tx, rx) = mpsc::unbounded_channel();
        let link = MpdLink {
            addr: addr.into(),
            states_tx: tx,
        };
        (link, rx)
    }

    /// Цикл переподключения к mpd: коннект → статус/watch → при обрыве
    /// повтор через 5 с. Запускать задачей.
    pub async fn serve(self: Arc<Self>, mut ctx: watch::Receiver<bool>) {
        loop {
            if crate::util::cancelled(&ctx) {
                return;
            }
            let mut c = match Client::dial(&self.addr).await {
                Ok(c) => c,
                Err(e) => {
                    logln!("mpd: {} (повтор через 5с)", e);
                    util::sleep_ctx(&mut ctx, Duration::from_secs(5)).await;
                    continue;
                }
            };
            logln!("mpd: подключено к {}", self.addr);
            if let Ok(st) = c.status().await {
                if let Some(state) = st.get("state") {
                    let _ = self.states_tx.send(state.clone());
                }
            }
            self.watch(&mut ctx, &mut c).await;
            let _ = self.states_tx.send("stop".to_string()); // нет связи с mpd — кольцо гасим
            logln!("mpd: связь потеряна (повтор через 5с)");
            util::sleep_ctx(&mut ctx, Duration::from_secs(5)).await;
        }
    }

    /// watch: idle-коннекция ловит события player (mixer не нужен: громкость
    /// — sink PA, пауза/плей переоткрывают pulse-вывод без смены громкости,
    /// отклик не дёргаем), ping каждые 2 с проверяет живость основной
    /// коннекции (через собственное соединение).
    async fn watch(&self, ctx: &mut watch::Receiver<bool>, c: &mut Client) {
        let mut w = match Client::dial(&self.addr).await {
            Ok(w) => w,
            Err(e) => {
                logln!("mpd watcher: {}", e);
                return;
            }
        };
        let (dead_tx, mut dead_rx) = mpsc::channel::<()>(1);

        // ping каждые 2 с: своё соединение, только для ping
        let mut ping_conn = match Client::dial(&self.addr).await {
            Ok(p) => p,
            Err(e) => {
                logln!("mpd watcher: ping-коннекция: {}", e);
                return;
            }
        };
        let mut ctx2 = ctx.clone();
        tokio::spawn(async move {
            let mut t = tokio::time::interval(Duration::from_secs(2));
            loop {
                tokio::select! {
                    _ = ctx2.changed() => {
                        if crate::util::cancelled(&ctx2) { return; }
                    }
                    _ = t.tick() => {
                        if ping_conn.ping().await.is_err() {
                            let _ = dead_tx.send(()).await;
                            return;
                        }
                    }
                }
            }
        });

        loop {
            tokio::select! {
                _ = ctx.changed() => {
                    if crate::util::cancelled(ctx) { return; }
                }
                _ = dead_rx.recv() => {
                    return;
                }
                ev = w.idle("player") => {
                    match ev {
                        Err(e) => {
                            logln!("mpd watcher: {}", e);
                            return;
                        }
                        Ok(subs) => {
                            if subs.iter().any(|s| s == "player") {
                                if let Ok(st) = c.status().await {
                                    if let Some(state) = st.get("state") {
                                        let _ = self.states_tx.send(state.clone());
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    /// play/pause: играет → пауза, на паузе → продолжить.
    pub async fn toggle_pause(&self) {
        let mut c = match Client::dial(&self.addr).await {
            Ok(c) => c,
            Err(e) => {
                logln!("mpd: toggle: {}", e);
                return;
            }
        };
        let Ok(st) = c.status().await else { return };
        let playing = st.get("state").map(|s| s == "play").unwrap_or(false);
        let _ = c.pause(playing).await;
    }
}
